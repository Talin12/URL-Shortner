// Package config loads service configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds every knob the service reads at startup. Everything has a
// default that works against the docker-compose stack.
type Config struct {
	// HTTPAddr is the listen address for the public server.
	HTTPAddr string
	// DatabaseURL is a libpq-style connection string for Postgres.
	DatabaseURL string
	// BaseURL prefixes short codes in API responses.
	BaseURL string
	// MaxPoolConns caps the pgx connection pool. The redirect path is a point
	// lookup, so this bounds how much concurrency reaches Postgres.
	MaxPoolConns int32
	// ShutdownTimeout bounds graceful shutdown before in-flight requests are cut.
	ShutdownTimeout time.Duration
	// ReadTimeout and WriteTimeout guard against slow clients holding sockets.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// AnalyticsMode selects the click recorder: "batch" (phase 2, decoupled)
	// or "sync" (phase 1 baseline). Keeping both in one binary means the two
	// can be A/B'd on identical hardware in the same session, which is the
	// only way the phase-over-phase comparison means anything.
	AnalyticsMode string
	// AnalyticsBuffer is the bounded channel depth, i.e. the entire
	// backpressure budget before events start being dropped.
	AnalyticsBuffer int
	// AnalyticsBatchSize and AnalyticsFlushInterval trigger a flush on
	// whichever comes first.
	AnalyticsBatchSize     int
	AnalyticsFlushInterval time.Duration
}

// Analytics recorder modes.
const (
	ModeBatch = "batch"
	ModeSync  = "sync"
)

// Load reads configuration from the environment, applying defaults for any
// variable that is unset.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        env("LINKFLOW_HTTP_ADDR", ":8080"),
		DatabaseURL:     env("LINKFLOW_DATABASE_URL", "postgres://linkflow:linkflow@localhost:5433/linkflow?sslmode=disable"),
		BaseURL:         env("LINKFLOW_BASE_URL", "http://localhost:8080"),
		ShutdownTimeout: 10 * time.Second,
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    10 * time.Second,
		AnalyticsMode:   env("LINKFLOW_ANALYTICS_MODE", ModeBatch),
	}

	if cfg.AnalyticsMode != ModeBatch && cfg.AnalyticsMode != ModeSync {
		return Config{}, fmt.Errorf("config: LINKFLOW_ANALYTICS_MODE must be %q or %q, got %q", ModeBatch, ModeSync, cfg.AnalyticsMode)
	}

	conns, err := envInt("LINKFLOW_MAX_POOL_CONNS", 25)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxPoolConns = int32(conns)

	if cfg.AnalyticsBuffer, err = envInt("LINKFLOW_ANALYTICS_BUFFER", 10000); err != nil {
		return Config{}, err
	}
	if cfg.AnalyticsBatchSize, err = envInt("LINKFLOW_ANALYTICS_BATCH_SIZE", 1000); err != nil {
		return Config{}, err
	}
	if cfg.AnalyticsFlushInterval, err = envDuration("LINKFLOW_ANALYTICS_FLUSH_INTERVAL", 200*time.Millisecond); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration such as 200ms: %w", key, err)
	}
	return d, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
	}
	return n, nil
}
