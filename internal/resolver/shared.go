package resolver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix namespaces link entries so the cache can share a Redis instance.
const keyPrefix = "linkflow:code:"

// Redis is the cross-instance cache tier.
type Redis struct {
	client *redis.Client
}

// NewRedis dials Redis and verifies the connection.
func NewRedis(ctx context.Context, addr string, poolSize int) (*Redis, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		PoolSize: poolSize,
		// Short timeouts on purpose: this tier is an optimisation, and the
		// resolver degrades to Postgres on error. Waiting a long time for a
		// sick Redis would be slower than skipping it.
		DialTimeout:  2 * time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("resolver: redis ping: %w", err)
	}
	return &Redis{client: client}, nil
}

// Get returns a cached destination. A cache miss is (", false, nil"); only a
// genuine failure returns an error.
func (r *Redis) Get(ctx context.Context, code string) (string, bool, error) {
	v, err := r.client.Get(ctx, keyPrefix+code).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

// Set caches a destination with a TTL.
func (r *Redis) Set(ctx context.Context, code, destination string, ttl time.Duration) error {
	return r.client.Set(ctx, keyPrefix+code, destination, ttl).Err()
}

// Close releases the connection pool.
func (r *Redis) Close() error { return r.client.Close() }
