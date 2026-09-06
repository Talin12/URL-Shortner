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

	// RedisAddr is the shared cache. Empty disables the tier entirely, which
	// is how the benchmark isolates local-only and no-cache configurations.
	RedisAddr string
	// RedisPoolSize caps connections to Redis.
	RedisPoolSize int
	// LocalCacheItems sizes the in-process tier. Zero disables it.
	LocalCacheItems int
	// CacheTTL and NegativeCacheTTL control entry lifetime. Tombstones expire
	// faster because a code that does not exist yet may exist shortly.
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
	// Singleflight collapses concurrent misses for one code into a single
	// origin query. Switchable so the stampede fix can be measured off and on.
	Singleflight bool
	// OriginDelay injects artificial latency before each origin query. Zero
	// in normal operation; used only to reproduce stampede conditions, where
	// a sub-millisecond local Postgres warms the cache before a herd can
	// even form.
	OriginDelay time.Duration

	// AnalyticsSink selects where click events land: "postgres" or
	// "clickhouse". Both are wired so the migration can be measured rather
	// than asserted (PLAN.md section 3).
	AnalyticsSink string
	// ClickHouseAddr is host:port for the native protocol (9000, not 8123).
	ClickHouseAddr string
	// ClickHouseDatabase, ClickHouseUser and ClickHousePassword address the
	// server.
	ClickHouseDatabase string
	ClickHouseUser     string
	ClickHousePassword string
}

// Analytics recorder modes.
const (
	ModeBatch = "batch"
	ModeSync  = "sync"
)

// Click event sinks.
const (
	SinkPostgres   = "postgres"
	SinkClickHouse = "clickhouse"
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

	// Present-but-empty means "no shared tier", which is different from unset.
	// The plain env() helper cannot express that, and treating empty as unset
	// silently re-enabled Redis on a run meant to measure life without it.
	cfg.RedisAddr = envAllowEmpty("LINKFLOW_REDIS_ADDR", "localhost:6379")
	if cfg.RedisPoolSize, err = envInt("LINKFLOW_REDIS_POOL_SIZE", 50); err != nil {
		return Config{}, err
	}
	if cfg.LocalCacheItems, err = envInt("LINKFLOW_LOCAL_CACHE_ITEMS", 100000); err != nil {
		return Config{}, err
	}
	if cfg.CacheTTL, err = envDuration("LINKFLOW_CACHE_TTL", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.NegativeCacheTTL, err = envDuration("LINKFLOW_NEGATIVE_CACHE_TTL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.Singleflight, err = envBool("LINKFLOW_SINGLEFLIGHT", true); err != nil {
		return Config{}, err
	}
	if cfg.OriginDelay, err = envDuration("LINKFLOW_ORIGIN_DELAY", 0); err != nil {
		return Config{}, err
	}

	cfg.AnalyticsSink = env("LINKFLOW_ANALYTICS_SINK", SinkPostgres)
	if cfg.AnalyticsSink != SinkPostgres && cfg.AnalyticsSink != SinkClickHouse {
		return Config{}, fmt.Errorf("config: LINKFLOW_ANALYTICS_SINK must be %q or %q, got %q", SinkPostgres, SinkClickHouse, cfg.AnalyticsSink)
	}
	cfg.ClickHouseAddr = env("LINKFLOW_CLICKHOUSE_ADDR", "localhost:9000")
	cfg.ClickHouseDatabase = env("LINKFLOW_CLICKHOUSE_DATABASE", "linkflow")
	cfg.ClickHouseUser = env("LINKFLOW_CLICKHOUSE_USER", "linkflow")
	cfg.ClickHousePassword = env("LINKFLOW_CLICKHOUSE_PASSWORD", "linkflow")

	return cfg, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("config: %s must be true or false: %w", key, err)
	}
	return v, nil
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

// envAllowEmpty returns the variable's value whenever it is set, including an
// empty string. Use it where empty is a meaningful choice rather than an
// absent one.
func envAllowEmpty(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
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
