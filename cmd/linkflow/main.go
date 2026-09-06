// Command linkflow serves the link redirection API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Talin12/URL-Shortner/internal/analytics"
	"github.com/Talin12/URL-Shortner/internal/config"
	"github.com/Talin12/URL-Shortner/internal/httpapi"
	"github.com/Talin12/URL-Shortner/internal/metrics"
	"github.com/Talin12/URL-Shortner/internal/resolver"
	"github.com/Talin12/URL-Shortner/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Signals cancel this context, which unwinds the whole shutdown sequence.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()

	db, err := store.New(dialCtx, cfg.DatabaseURL, cfg.MaxPoolConns)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Migrate(dialCtx); err != nil {
		return err
	}
	logger.Info("schema applied")

	m := metrics.New()

	recorder := newRecorder(cfg, db, logger)
	m.WatchAnalytics(recorder.Stats)
	logger.Info("analytics recorder ready", "mode", cfg.AnalyticsMode)

	res, err := newResolver(dialCtx, cfg, db, m, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := res.Close(); err != nil {
			logger.Error("resolver shutdown", "err", err)
		}
	}()

	api := httpapi.New(db, res, recorder, m, logger, cfg.BaseURL)

	srv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      api.Routes(),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.HTTPAddr, "base_url", cfg.BaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	// Stop accepting first, then flush the recorder, so nothing new arrives
	// while the buffer is draining. Phase 1's recorder has nothing to flush;
	// the ordering is what phase 2 needs.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown", "err", err)
	}
	if err := recorder.Close(shutdownCtx); err != nil {
		logger.Error("recorder shutdown", "err", err)
	}

	// Say what was lost. A drop policy is only defensible if the drops are
	// visible (PLAN.md section 5.1).
	stats := recorder.Stats()
	logger.Info("analytics summary",
		"mode", stats.Mode,
		"accepted", stats.Accepted,
		"written", stats.Written,
		"dropped", stats.Dropped(),
		"batches", stats.Batches,
	)
	logger.Info("stopped cleanly")
	return nil
}

// newResolver builds the read path. Either cache tier can be switched off --
// an empty LINKFLOW_REDIS_ADDR or a zero LINKFLOW_LOCAL_CACHE_ITEMS -- so the
// benchmark can isolate no-cache, Redis-only and two-tier on one binary.
func newResolver(ctx context.Context, cfg config.Config, db *store.Store, m *metrics.Metrics, logger *slog.Logger) (*resolver.Resolver, error) {
	var local resolver.LocalCache
	if cfg.LocalCacheItems > 0 {
		r, err := resolver.NewRistretto(int64(cfg.LocalCacheItems))
		if err != nil {
			return nil, err
		}
		local = r
	}

	var shared resolver.SharedCache
	if cfg.RedisAddr != "" {
		r, err := resolver.NewRedis(ctx, cfg.RedisAddr, cfg.RedisPoolSize)
		if err != nil {
			return nil, err
		}
		shared = r
	}

	// Benchmark affordance only: see resolver.NewSlowStore.
	var origin resolver.LinkStore = db
	if cfg.OriginDelay > 0 {
		origin = resolver.NewSlowStore(db, cfg.OriginDelay)
		logger.Warn("origin delay injected -- benchmark mode, not for real use", "delay", cfg.OriginDelay)
	}

	logger.Info("resolver ready",
		"local_cache_items", cfg.LocalCacheItems,
		"redis", cfg.RedisAddr,
		"singleflight", cfg.Singleflight,
	)

	return resolver.New(origin, local, shared, m, resolver.Config{
		TTL:           cfg.CacheTTL,
		NegativeTTL:   cfg.NegativeCacheTTL,
		Singleflight:  cfg.Singleflight,
		OriginTimeout: 3 * time.Second,
	}), nil
}

// newRecorder picks the click-recording strategy. Both live in the binary so
// the phase 1 baseline and the phase 2 decoupled path can be compared on the
// same hardware without rebuilding.
func newRecorder(cfg config.Config, db *store.Store, logger *slog.Logger) analytics.Recorder {
	if cfg.AnalyticsMode == config.ModeSync {
		return analytics.NewSync(db, logger)
	}
	return analytics.NewBatch(db, logger, analytics.BatchConfig{
		BufferSize:    cfg.AnalyticsBuffer,
		BatchSize:     cfg.AnalyticsBatchSize,
		FlushInterval: cfg.AnalyticsFlushInterval,
		FlushTimeout:  5 * time.Second,
	})
}
