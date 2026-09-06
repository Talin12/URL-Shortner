package analytics

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Talin12/URL-Shortner/internal/store"
)

// BatchConfig tunes the batching recorder.
type BatchConfig struct {
	// BufferSize is the depth of the bounded channel between the redirect
	// path and the batcher. It is the entire backpressure budget: once full,
	// events are dropped rather than queued, so this is really a decision
	// about how much of a downstream stall to absorb before degrading.
	BufferSize int
	// BatchSize flushes as soon as this many events are buffered.
	BatchSize int
	// FlushInterval flushes a partial batch after this long, so a quiet
	// service still lands its events in bounded time.
	FlushInterval time.Duration
	// FlushTimeout bounds one insert. A stalled database must not wedge the
	// batcher, because a wedged batcher fills the buffer and starts dropping.
	FlushTimeout time.Duration
}

// DefaultBatchConfig matches PLAN.md section 4: flush on 1000 events or 200ms.
func DefaultBatchConfig() BatchConfig {
	return BatchConfig{
		BufferSize:    10000,
		BatchSize:     1000,
		FlushInterval: 200 * time.Millisecond,
		FlushTimeout:  5 * time.Second,
	}
}

// BatchRecorder decouples click writes from the redirect path. Record does a
// non-blocking send onto a bounded channel and returns; a single goroutine
// drains that channel and writes to Postgres in batches.
//
// The trade-off this makes, deliberately: the 302 goes out before the click
// event is durable anywhere. An instance killed with events still buffered
// loses them. That is acceptable for analytics and would not be for billing
// (PLAN.md section 5.1).
type BatchRecorder struct {
	writer ClickWriter
	logger *slog.Logger
	cfg    BatchConfig

	events chan store.ClickEvent
	// quit tells the batcher to drain and exit; done is closed once it has.
	quit chan struct{}
	done chan struct{}

	closing  atomic.Bool
	closeOne sync.Once
	counts   counters
}

// NewBatch starts the batcher goroutine and returns a ready recorder.
func NewBatch(w ClickWriter, logger *slog.Logger, cfg BatchConfig) *BatchRecorder {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = DefaultBatchConfig().BufferSize
	}
	if cfg.BatchSize <= 0 || cfg.BatchSize > cfg.BufferSize {
		cfg.BatchSize = min(DefaultBatchConfig().BatchSize, cfg.BufferSize)
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultBatchConfig().FlushInterval
	}
	if cfg.FlushTimeout <= 0 {
		cfg.FlushTimeout = DefaultBatchConfig().FlushTimeout
	}

	r := &BatchRecorder{
		writer: w,
		logger: logger,
		cfg:    cfg,
		events: make(chan store.ClickEvent, cfg.BufferSize),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go r.run()
	return r
}

// Record enqueues the event without ever blocking the caller.
//
// This select is the whole availability decision in five lines: when the
// buffer is full the event is discarded and counted, so a ClickHouse or
// Postgres outage degrades analytics instead of taking redirects down with it.
func (r *BatchRecorder) Record(_ context.Context, ev store.ClickEvent) {
	r.counts.accepted.Add(1)

	if r.closing.Load() {
		r.counts.droppedAfterClose.Add(1)
		return
	}

	select {
	case r.events <- ev:
	default:
		r.counts.droppedBufferFull.Add(1)
	}
}

// run owns the buffer. Being the only reader means the batch slice needs no
// locking, and the flush is never concurrent with itself.
func (r *BatchRecorder) run() {
	defer close(r.done)

	batch := make([]store.ClickEvent, 0, r.cfg.BatchSize)
	ticker := time.NewTicker(r.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case ev := <-r.events:
			batch = append(batch, ev)
			if len(batch) >= r.cfg.BatchSize {
				batch = r.flush(batch)
			}

		case <-ticker.C:
			// A partial batch still has to land, or a low-traffic link's
			// clicks would sit in memory until the buffer happened to fill.
			batch = r.flush(batch)

		case <-r.quit:
			// Drain whatever the redirect path already handed us. Anything
			// arriving after this point is counted as dropped, not lost
			// silently.
			for {
				select {
				case ev := <-r.events:
					batch = append(batch, ev)
					if len(batch) >= r.cfg.BatchSize {
						batch = r.flush(batch)
					}
				default:
					r.flush(batch)
					return
				}
			}
		}
	}
}

// flush writes the batch and returns an emptied slice for reuse. The backing
// array is kept so steady-state batching does not allocate.
func (r *BatchRecorder) flush(batch []store.ClickEvent) []store.ClickEvent {
	if len(batch) == 0 {
		return batch
	}

	// Deliberately not the request context: the request is long gone, and
	// tying the write to it would cancel flushes for no reason.
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.FlushTimeout)
	defer cancel()

	if err := r.writer.InsertClicks(ctx, batch); err != nil {
		r.counts.droppedWriteFailed.Add(uint64(len(batch)))
		r.logger.Warn("click batch lost", "events", len(batch), "err", err)
	} else {
		r.counts.written.Add(uint64(len(batch)))
	}
	r.counts.batches.Add(1)

	return batch[:0]
}

// Stats reports the batcher's tallies and current queue depth.
func (r *BatchRecorder) Stats() Stats {
	return Stats{
		Mode:               "batch",
		Accepted:           r.counts.accepted.Load(),
		Written:            r.counts.written.Load(),
		DroppedBufferFull:  r.counts.droppedBufferFull.Load(),
		DroppedAfterClose:  r.counts.droppedAfterClose.Load(),
		DroppedWriteFailed: r.counts.droppedWriteFailed.Load(),
		Batches:            r.counts.batches.Load(),
		BufferLen:          len(r.events),
		BufferCap:          cap(r.events),
	}
}

// Close stops accepting events, flushes the buffer, and waits for the batcher
// to exit or ctx to expire. Callers must stop serving HTTP first, otherwise
// new events keep arriving behind the drain.
func (r *BatchRecorder) Close(ctx context.Context) error {
	r.closeOne.Do(func() {
		r.closing.Store(true)
		close(r.quit)
	})

	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		// Report how much went unflushed rather than exiting quietly.
		r.logger.Warn("shutdown timed out with events still buffered", "buffered", len(r.events))
		return ctx.Err()
	}
}
