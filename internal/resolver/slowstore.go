package resolver

import (
	"context"
	"time"
)

// slowStore adds latency in front of the origin.
//
// This exists for one reason: reproducing a cache stampede requires the origin
// query to still be running when the herd arrives. Against a local Postgres a
// lookup finishes in well under a millisecond, so by the time a load generator
// has dispatched its second request the cache is already warm and the
// stampede never happens -- which measures the load generator, not the
// service. Injecting the latency a loaded database would have is what makes
// the singleflight comparison real.
//
// Off by default and never enabled in normal operation. See
// LINKFLOW_ORIGIN_DELAY.
type slowStore struct {
	inner LinkStore
	delay time.Duration
}

// NewSlowStore wraps a store so every origin query takes at least delay.
func NewSlowStore(inner LinkStore, delay time.Duration) LinkStore {
	if delay <= 0 {
		return inner
	}
	return &slowStore{inner: inner, delay: delay}
}

func (s *slowStore) Destination(ctx context.Context, code string) (string, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return s.inner.Destination(ctx, code)
}
