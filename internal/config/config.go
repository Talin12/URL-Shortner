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
}

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
	}

	conns, err := envInt("LINKFLOW_MAX_POOL_CONNS", 25)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxPoolConns = int32(conns)

	return cfg, nil
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
