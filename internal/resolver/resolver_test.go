package resolver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Talin12/URL-Shortner/internal/metrics"
	"github.com/Talin12/URL-Shortner/internal/store"
)

// fakeStore counts queries and can be made slow, which is what turns the
// stampede test into a real race rather than a formality.
type fakeStore struct {
	queries atomic.Int64
	delay   time.Duration
	links   map[string]string
	err     error
}

func (f *fakeStore) Destination(_ context.Context, code string) (string, error) {
	f.queries.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.err != nil {
		return "", f.err
	}
	dest, ok := f.links[code]
	if !ok {
		return "", store.ErrNotFound
	}
	return dest, nil
}

// mapCache is a synchronous stand-in for a cache tier. Ristretto admits
// asynchronously, which would make these assertions flaky.
type mapCache struct {
	mu      sync.Mutex
	entries map[string]string
	getErr  error
	setErr  error
	gets    atomic.Int64
}

func newMapCache() *mapCache { return &mapCache{entries: map[string]string{}} }

func (c *mapCache) Get(code string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[code]
	return v, ok
}

func (c *mapCache) Set(code, destination string, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[code] = destination
}

func (c *mapCache) Close() {}

// The same map, behind the SharedCache interface.
type sharedMap struct{ *mapCache }

func (s sharedMap) Get(_ context.Context, code string) (string, bool, error) {
	s.gets.Add(1)
	if s.getErr != nil {
		return "", false, s.getErr
	}
	v, ok := s.mapCache.Get(code)
	return v, ok, nil
}

func (s sharedMap) Set(_ context.Context, code, destination string, ttl time.Duration) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.mapCache.Set(code, destination, ttl)
	return nil
}

func (s sharedMap) Close() error { return nil }

func counterValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	c.Collect(ch)
	close(ch)

	var total float64
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		total += pb.GetCounter().GetValue()
	}
	return total
}

func labelledCounter(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	return counterValue(t, vec.WithLabelValues(labels...))
}

func TestResolvesFromOriginAndPopulatesBothTiers(t *testing.T) {
	st := &fakeStore{links: map[string]string{"abc": "https://example.com"}}
	local, shared := newMapCache(), sharedMap{newMapCache()}
	r := New(st, local, shared, metrics.New(), DefaultConfig())

	got, err := r.Destination(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Destination: %v", err)
	}
	if got != "https://example.com" {
		t.Errorf("Destination = %q", got)
	}

	if v, ok := local.Get("abc"); !ok || v != "https://example.com" {
		t.Error("local tier was not populated on the way back")
	}
	if v, ok := shared.mapCache.Get("abc"); !ok || v != "https://example.com" {
		t.Error("shared tier was not populated on the way back")
	}
}

func TestLocalHitSkipsSharedAndOrigin(t *testing.T) {
	st := &fakeStore{links: map[string]string{"abc": "https://example.com"}}
	local, shared := newMapCache(), sharedMap{newMapCache()}
	r := New(st, local, shared, metrics.New(), DefaultConfig())

	if _, err := r.Destination(context.Background(), "abc"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	before := shared.gets.Load()

	for i := 0; i < 10; i++ {
		if _, err := r.Destination(context.Background(), "abc"); err != nil {
			t.Fatalf("cached read: %v", err)
		}
	}

	if got := st.queries.Load(); got != 1 {
		t.Errorf("origin queries = %d, want 1", got)
	}
	if got := shared.gets.Load(); got != before {
		t.Errorf("shared cache was probed %d times after a local hit", got-before)
	}
}

func TestSharedHitPromotesToLocal(t *testing.T) {
	st := &fakeStore{links: map[string]string{"abc": "https://example.com"}}
	local, shared := newMapCache(), sharedMap{newMapCache()}
	shared.mapCache.Set("abc", "https://example.com", time.Minute)

	r := New(st, local, shared, metrics.New(), DefaultConfig())
	if _, err := r.Destination(context.Background(), "abc"); err != nil {
		t.Fatalf("Destination: %v", err)
	}

	if _, ok := local.Get("abc"); !ok {
		t.Error("a shared-tier hit did not promote the entry to the local tier")
	}
	if got := st.queries.Load(); got != 0 {
		t.Errorf("origin queries = %d, want 0 -- redis had the answer", got)
	}
}

// TestStampedeCollapsesToOneQuery is the claim in PLAN.md 5.2, checked rather
// than asserted: 10,000 concurrent requests for one cold key must produce one
// origin query, not 10,000.
func TestStampedeCollapsesToOneQuery(t *testing.T) {
	const concurrent = 10000

	st := &fakeStore{
		links: map[string]string{"viral": "https://example.com/viral"},
		delay: 50 * time.Millisecond, // long enough that everyone piles up
	}
	m := metrics.New()
	r := New(st, newMapCache(), sharedMap{newMapCache()}, m, DefaultConfig())

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, concurrent)

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := r.Destination(context.Background(), "viral"); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent resolve: %v", err)
	}

	if got := st.queries.Load(); got != 1 {
		t.Errorf("origin queries = %d, want 1 -- singleflight did not collapse the stampede", got)
	}
	if got := counterValue(t, m.OriginQueries); got != 1 {
		t.Errorf("linkflow_origin_queries_total = %v, want 1", got)
	}
	if got := counterValue(t, m.SingleflightShared); got < 1 {
		t.Error("no caller was recorded as joining an in-flight load")
	}
}

// TestStampedeWithoutSingleflight is the control: the same load with the fix
// disabled must hammer the origin, or the test above proves nothing.
func TestStampedeWithoutSingleflight(t *testing.T) {
	const concurrent = 200

	st := &fakeStore{
		links: map[string]string{"viral": "https://example.com/viral"},
		delay: 20 * time.Millisecond,
	}
	cfg := DefaultConfig()
	cfg.Singleflight = false
	r := New(st, newMapCache(), sharedMap{newMapCache()}, metrics.New(), cfg)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = r.Destination(context.Background(), "viral")
		}()
	}
	close(start)
	wg.Wait()

	// Not exactly `concurrent`: some goroutines start late enough to find the
	// warmed cache. The point is that it is many, not one.
	if got := st.queries.Load(); got < 10 {
		t.Errorf("origin queries = %d without singleflight; expected the stampede to reach the origin", got)
	}
}

func TestUnknownCodeIsNegativelyCached(t *testing.T) {
	st := &fakeStore{links: map[string]string{}}
	local, shared := newMapCache(), sharedMap{newMapCache()}
	r := New(st, local, shared, metrics.New(), DefaultConfig())

	for i := 0; i < 5; i++ {
		_, err := r.Destination(context.Background(), "missing")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Destination(missing) = %v, want ErrNotFound", err)
		}
	}

	if got := st.queries.Load(); got != 1 {
		t.Errorf("origin queries = %d, want 1 -- the tombstone was not cached", got)
	}
	if v, ok := local.Get("missing"); !ok || v != tombstone {
		t.Error("no tombstone stored in the local tier")
	}
}

func TestRedisFailureDegradesToOrigin(t *testing.T) {
	// A Redis outage must cost latency, not availability.
	st := &fakeStore{links: map[string]string{"abc": "https://example.com"}}
	shared := sharedMap{newMapCache()}
	shared.getErr = errors.New("redis is down")
	shared.setErr = errors.New("redis is down")

	m := metrics.New()
	r := New(st, nil, shared, m, DefaultConfig())

	got, err := r.Destination(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Destination with a failing Redis: %v", err)
	}
	if got != "https://example.com" {
		t.Errorf("Destination = %q", got)
	}
	if counterValue(t, m.SharedCacheErrors) < 1 {
		t.Error("redis failure was not counted")
	}
}

func TestOriginErrorIsPropagated(t *testing.T) {
	st := &fakeStore{err: errors.New("postgres is down")}
	r := New(st, newMapCache(), sharedMap{newMapCache()}, metrics.New(), DefaultConfig())

	_, err := r.Destination(context.Background(), "abc")
	if err == nil || !strings.Contains(err.Error(), "postgres is down") {
		t.Errorf("Destination error = %v, want the origin failure", err)
	}
}

func TestNilTiersAreSkipped(t *testing.T) {
	st := &fakeStore{links: map[string]string{"abc": "https://example.com"}}
	r := New(st, nil, nil, metrics.New(), DefaultConfig())

	for i := 0; i < 3; i++ {
		if _, err := r.Destination(context.Background(), "abc"); err != nil {
			t.Fatalf("Destination with no cache tiers: %v", err)
		}
	}
	if got := st.queries.Load(); got != 3 {
		t.Errorf("origin queries = %d, want 3 -- with no cache every read is a query", got)
	}
}

func TestTierMetricsAreRecordedSeparately(t *testing.T) {
	st := &fakeStore{links: map[string]string{"abc": "https://example.com"}}
	local, shared := newMapCache(), sharedMap{newMapCache()}
	m := metrics.New()
	r := New(st, local, shared, m, DefaultConfig())

	// First read misses everything; the next two hit the local tier.
	for i := 0; i < 3; i++ {
		if _, err := r.Destination(context.Background(), "abc"); err != nil {
			t.Fatalf("Destination: %v", err)
		}
	}

	if got := labelledCounter(t, m.CacheLookups, metrics.TierLocal, "hit"); got != 2 {
		t.Errorf("local hits = %v, want 2", got)
	}
	if got := labelledCounter(t, m.CacheLookups, metrics.TierLocal, "miss"); got != 1 {
		t.Errorf("local misses = %v, want 1", got)
	}
	if got := labelledCounter(t, m.CacheLookups, metrics.TierShared, "miss"); got != 1 {
		t.Errorf("shared misses = %v, want 1", got)
	}
}

func TestRistrettoTierRoundTrips(t *testing.T) {
	c, err := NewRistretto(1000)
	if err != nil {
		t.Fatalf("NewRistretto: %v", err)
	}
	defer c.Close()

	c.Set("abc", "https://example.com", time.Minute)

	// Ristretto admits asynchronously, so poll rather than assuming the entry
	// is visible on the next read.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := c.Get("abc"); ok {
			if v != "https://example.com" {
				t.Fatalf("Get = %q", v)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("entry never became visible in the local cache")
}
