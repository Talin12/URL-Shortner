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

	recorder := analytics.NewSync(db, logger)
	api := httpapi.New(db, recorder, logger, cfg.BaseURL)

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
	logger.Info("stopped cleanly")
	return nil
}
