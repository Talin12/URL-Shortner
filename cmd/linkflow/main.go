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
	"github.com/Talin12/URL-Shortner/internal/clickstore"
	"github.com/Talin12/URL-Shortner/internal/config"
	"github.com/Talin12/URL-Shortner/internal/httpapi"
	"github.com/Talin12/URL-Shortner/internal/idgen"
	"github.com/Talin12/URL-Shortner/internal/links"
	"github.com/Talin12/URL-Shortner/internal/metrics"
	"github.com/Talin12/URL-Shortner/internal/resolver"
	"github.com/Talin12/URL-Shortner/internal/shortcode"
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

	sink, closeSink, err := newClickSink(dialCtx, cfg, db, logger)
	if err != nil {
		return err
	}
	defer closeSink()

	recorder := newRecorder(cfg, sink, logger)
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

	// One database write per ID block, none per link. Every instance derives
	// identical codec keys from the shared seed, so a code means the same
	// thing whichever replica issued it.
	allocator := idgen.New(
		func(ctx context.Context, size uint64) (uint64, error) {
			return db.ClaimIDBlock(ctx, "links", size)
		},
		uint64(cfg.IDBlockSize),
	)
	creator := links.New(allocator, shortcode.NewCodec(cfg.CodeSeed), db)
	logger.Info("id allocator ready", "block_size", cfg.IDBlockSize)

	api := httpapi.New(db, creator, res, sink, recorder, m, logger, cfg.BaseURL)

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

// clickSink is everything the service needs from a click event store: the
// write side the batcher drives, and the read side the stats endpoint uses.
type clickSink interface {
	analytics.ClickWriter
	httpapi.ClickReader
}

// newClickSink picks where click events land. Postgres is the phase 1-3
// answer and stays the default; ClickHouse is phase 4, kept switchable so the
// two can be measured against each other rather than argued about.
func newClickSink(ctx context.Context, cfg config.Config, db *store.Store, logger *slog.Logger) (clickSink, func(), error) {
	if cfg.AnalyticsSink != config.SinkClickHouse {
		logger.Info("click sink ready", "sink", config.SinkPostgres)
		return db, func() {}, nil
	}

	ch, err := clickstore.New(ctx, clickstore.Options{
		Addr:     cfg.ClickHouseAddr,
		Database: cfg.ClickHouseDatabase,
		Username: cfg.ClickHouseUser,
		Password: cfg.ClickHousePassword,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := ch.Migrate(ctx); err != nil {
		_ = ch.Close()
		return nil, nil, err
	}

	logger.Info("click sink ready", "sink", config.SinkClickHouse, "addr", cfg.ClickHouseAddr)
	return ch, func() {
		if err := ch.Close(); err != nil {
			logger.Error("clickhouse shutdown", "err", err)
		}
	}, nil
}

// newRecorder picks the click-recording strategy. Both live in the binary so
// the phase 1 baseline and the phase 2 decoupled path can be compared on the
// same hardware without rebuilding.
func newRecorder(cfg config.Config, sink clickSink, logger *slog.Logger) analytics.Recorder {
	if cfg.AnalyticsMode == config.ModeSync {
		return analytics.NewSync(sink, logger)
	}
	return analytics.NewBatch(sink, logger, analytics.BatchConfig{
		BufferSize:    cfg.AnalyticsBuffer,
		BatchSize:     cfg.AnalyticsBatchSize,
		FlushInterval: cfg.AnalyticsFlushInterval,
		FlushTimeout:  5 * time.Second,
	})
}
