// Package metrics owns the Prometheus collectors for the service.
//
// Everything here exists to make a decision visible. The cache tier counters
// prove the two-tier design earns its complexity; the origin-query counter is
// what makes the singleflight stampede claim checkable rather than asserted;
// the analytics drop counters are what make dropping events a defensible
// policy instead of silent loss (PLAN.md sections 5.1-5.3).
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Talin12/URL-Shortner/internal/analytics"
)

// Cache tier names, used as the "tier" label.
const (
	TierLocal  = "local"
	TierShared = "redis"
	TierOrigin = "postgres"
)

// Metrics holds every collector the service updates.
type Metrics struct {
	registry *prometheus.Registry

	// CacheLookups counts every tier probe by outcome. Hit rate per tier is
	// derived from this, and PLAN.md 5.3 asks for the tiers reported
	// separately rather than as one blended number.
	CacheLookups *prometheus.CounterVec
	// OriginQueries counts actual Postgres lookups on the redirect path.
	// Under a stampede this is the number that must stay near 1.
	OriginQueries prometheus.Counter
	// SingleflightShared counts callers that joined an in-flight load instead
	// of issuing their own query -- the stampede collapse, measured.
	SingleflightShared prometheus.Counter
	// SharedCacheErrors counts Redis failures the resolver degraded past.
	SharedCacheErrors prometheus.Counter

	// Redirects counts responses by outcome.
	Redirects *prometheus.CounterVec
	// RedirectDuration is the server-side handler latency histogram.
	RedirectDuration prometheus.Histogram
}

// New builds the collectors on a private registry. A private registry keeps
// the exposed surface to what this service actually publishes.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		registry: reg,
		CacheLookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkflow_cache_lookups_total",
			Help: "Cache probes by tier and outcome.",
		}, []string{"tier", "result"}),
		OriginQueries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "linkflow_origin_queries_total",
			Help: "Postgres lookups issued by the redirect path.",
		}),
		SingleflightShared: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "linkflow_singleflight_shared_total",
			Help: "Redirects that joined an in-flight origin load instead of issuing their own.",
		}),
		SharedCacheErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "linkflow_shared_cache_errors_total",
			Help: "Redis errors the resolver degraded past.",
		}),
		Redirects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "linkflow_redirects_total",
			Help: "Redirect outcomes.",
		}, []string{"result"}),
		RedirectDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "linkflow_redirect_duration_seconds",
			Help: "Redirect handler latency.",
			// Buckets are tight at the bottom: a cache hit should land around
			// 100 microseconds, and default buckets would put everything
			// interesting in one bar.
			Buckets: []float64{
				0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005,
				0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1,
			},
		}),
	}

	reg.MustRegister(
		m.CacheLookups, m.OriginQueries, m.SingleflightShared,
		m.SharedCacheErrors, m.Redirects, m.RedirectDuration,
	)
	return m
}

// Handler serves the exposition endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// CacheHit and CacheMiss record a tier probe.
func (m *Metrics) CacheHit(tier string)  { m.CacheLookups.WithLabelValues(tier, "hit").Inc() }
func (m *Metrics) CacheMiss(tier string) { m.CacheLookups.WithLabelValues(tier, "miss").Inc() }

// WatchAnalytics publishes the recorder's counters by polling its snapshot at
// scrape time. The recorder keeps plain atomics and stays unaware of
// Prometheus, which keeps it unit-testable without a registry.
func (m *Metrics) WatchAnalytics(snapshot func() analytics.Stats) {
	counter := func(name, help string, pick func(analytics.Stats) uint64) prometheus.Collector {
		return prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help},
			func() float64 { return float64(pick(snapshot())) })
	}

	m.registry.MustRegister(
		counter("linkflow_analytics_accepted_total", "Click events handed to the recorder.",
			func(s analytics.Stats) uint64 { return s.Accepted }),
		counter("linkflow_analytics_written_total", "Click events confirmed by the store.",
			func(s analytics.Stats) uint64 { return s.Written }),
		counter("linkflow_analytics_batches_total", "Batch flushes issued.",
			func(s analytics.Stats) uint64 { return s.Batches }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name:        "linkflow_events_dropped_total",
			Help:        "Click events discarded because the buffer was full.",
			ConstLabels: prometheus.Labels{"reason": "buffer_full"},
		}, func() float64 { return float64(snapshot().DroppedBufferFull) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name:        "linkflow_events_dropped_reason_total",
			Help:        "Click events discarded, by reason.",
			ConstLabels: prometheus.Labels{"reason": "write_failed"},
		}, func() float64 { return float64(snapshot().DroppedWriteFailed) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name:        "linkflow_events_dropped_after_close_total",
			Help:        "Click events discarded during shutdown.",
			ConstLabels: prometheus.Labels{"reason": "after_close"},
		}, func() float64 { return float64(snapshot().DroppedAfterClose) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "linkflow_analytics_buffer_depth",
			Help: "Events currently waiting in the bounded channel.",
		}, func() float64 { return float64(snapshot().BufferLen) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "linkflow_analytics_buffer_capacity",
			Help: "Bounded channel capacity.",
		}, func() float64 { return float64(snapshot().BufferCap) }),
	)
}
