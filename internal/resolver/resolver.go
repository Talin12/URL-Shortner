// Package resolver turns a short code into a destination URL through a
// two-tier cache, falling back to Postgres.
//
// The tiering is not decoration. Real link traffic is heavily skewed, so a
// small number of codes take most of the requests. A shared Redis alone would
// send every one of those across the network to the same node and saturate it
// (PLAN.md 5.3); an in-process cache absorbs them at roughly 100ns and never
// leaves the box. Cold keys still go to Redis, which is what keeps the local
// tier small.
package resolver

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Talin12/URL-Shortner/internal/metrics"
	"github.com/Talin12/URL-Shortner/internal/store"
)

// ErrNotFound is returned when no link exists for a code.
var ErrNotFound = errors.New("resolver: link not found")

// tombstone marks a code that is known not to exist. Without negative caching
// a flood of bad codes -- a scanner walking the keyspace, a dead link on a
// popular page -- would miss every tier and land on Postgres every time.
const tombstone = "\x00notfound"

// LinkStore is the origin of truth. Narrowed to one method so the resolver is
// testable without Postgres.
type LinkStore interface {
	Destination(ctx context.Context, code string) (string, error)
}

// LocalCache is the in-process tier.
type LocalCache interface {
	Get(code string) (string, bool)
	Set(code, destination string, ttl time.Duration)
	Close()
}

// SharedCache is the cross-instance tier.
type SharedCache interface {
	Get(ctx context.Context, code string) (string, bool, error)
	Set(ctx context.Context, code, destination string, ttl time.Duration) error
	Close() error
}

// Config tunes the resolver. A nil cache tier is simply skipped, which is how
// the benchmark isolates "no cache", "redis only" and "two tier".
type Config struct {
	// TTL is how long a resolved destination stays cached.
	TTL time.Duration
	// NegativeTTL is the (shorter) lifetime of a tombstone. Short because a
	// code that does not exist yet may exist in a moment.
	NegativeTTL time.Duration
	// Singleflight collapses concurrent misses for the same code into one
	// origin query. Switchable so the stampede fix can be measured with it
	// off and on rather than asserted.
	Singleflight bool
	// OriginTimeout bounds a Postgres lookup.
	OriginTimeout time.Duration
}

// DefaultConfig returns sensible values.
func DefaultConfig() Config {
	return Config{
		TTL:           10 * time.Minute,
		NegativeTTL:   30 * time.Second,
		Singleflight:  true,
		OriginTimeout: 3 * time.Second,
	}
}

// Resolver is safe for concurrent use.
type Resolver struct {
	local   LocalCache
	shared  SharedCache
	store   LinkStore
	metrics *metrics.Metrics
	cfg     Config
	group   singleflight.Group
}

// New wires the tiers. Either cache may be nil.
func New(s LinkStore, local LocalCache, shared SharedCache, m *metrics.Metrics, cfg Config) *Resolver {
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultConfig().TTL
	}
	if cfg.NegativeTTL <= 0 {
		cfg.NegativeTTL = DefaultConfig().NegativeTTL
	}
	if cfg.OriginTimeout <= 0 {
		cfg.OriginTimeout = DefaultConfig().OriginTimeout
	}
	return &Resolver{local: local, shared: shared, store: s, metrics: m, cfg: cfg}
}

// Destination resolves a code, walking local → shared → origin and populating
// the tiers it passed through on the way back.
func (r *Resolver) Destination(ctx context.Context, code string) (string, error) {
	if r.local != nil {
		if v, ok := r.local.Get(code); ok {
			r.metrics.CacheHit(metrics.TierLocal)
			return unwrap(v)
		}
		r.metrics.CacheMiss(metrics.TierLocal)
	}

	if r.shared != nil {
		v, ok, err := r.shared.Get(ctx, code)
		switch {
		case err != nil:
			// A Redis outage must not become a redirect outage: count it and
			// fall through to the origin.
			r.metrics.SharedCacheErrors.Inc()
			r.metrics.CacheMiss(metrics.TierShared)
		case ok:
			r.metrics.CacheHit(metrics.TierShared)
			r.setLocal(code, v)
			return unwrap(v)
		default:
			r.metrics.CacheMiss(metrics.TierShared)
		}
	}

	return r.load(ctx, code)
}

// load fetches from the origin, collapsing concurrent callers for the same
// code when singleflight is enabled.
func (r *Resolver) load(ctx context.Context, code string) (string, error) {
	fetch := func() (any, error) { return r.fetchOrigin(ctx, code) }

	if !r.cfg.Singleflight {
		v, err := fetch()
		if err != nil {
			return "", err
		}
		return unwrap(v.(string))
	}

	v, err, shared := r.group.Do(code, fetch)
	if shared {
		r.metrics.SingleflightShared.Inc()
	}
	if err != nil {
		return "", err
	}
	return unwrap(v.(string))
}

// fetchOrigin queries Postgres and populates the caches.
func (r *Resolver) fetchOrigin(ctx context.Context, code string) (string, error) {
	// Detached from the caller's context on purpose. Under singleflight the
	// first caller owns the query, and if that one request were cancelled --
	// client hung up, timeout -- everyone waiting behind it would fail with a
	// cancellation that has nothing to do with them.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.OriginTimeout)
	defer cancel()

	r.metrics.OriginQueries.Inc()
	r.metrics.CacheMiss(metrics.TierOrigin)

	destination, err := r.store.Destination(ctx, code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			r.populate(ctx, code, tombstone, r.cfg.NegativeTTL)
			return tombstone, nil
		}
		return "", err
	}

	r.metrics.CacheHit(metrics.TierOrigin)
	r.populate(ctx, code, destination, r.cfg.TTL)
	return destination, nil
}

// populate writes through both tiers.
func (r *Resolver) populate(ctx context.Context, code, value string, ttl time.Duration) {
	r.setLocal(code, value)
	if r.shared != nil {
		if err := r.shared.Set(ctx, code, value, ttl); err != nil {
			// Failing to warm the cache is not a failure to serve.
			r.metrics.SharedCacheErrors.Inc()
		}
	}
}

func (r *Resolver) setLocal(code, value string) {
	if r.local == nil {
		return
	}
	ttl := r.cfg.TTL
	if value == tombstone {
		ttl = r.cfg.NegativeTTL
	}
	r.local.Set(code, value, ttl)
}

// Close releases both cache tiers.
func (r *Resolver) Close() error {
	if r.local != nil {
		r.local.Close()
	}
	if r.shared != nil {
		return r.shared.Close()
	}
	return nil
}

// unwrap turns a cached value into a result, translating the tombstone back
// into a not-found error.
func unwrap(v string) (string, error) {
	if v == tombstone {
		return "", ErrNotFound
	}
	return v, nil
}
