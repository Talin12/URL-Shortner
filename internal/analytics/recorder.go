// Package analytics records click events. The interface exists so the redirect
// handler never learns which strategy is in use: phase 1 ships a synchronous
// recorder that blocks the redirect on a Postgres insert, and phase 2 adds a
// bounded-channel batcher behind the same methods.
package analytics

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Talin12/URL-Shortner/internal/store"
)

// ClickWriter is the slice of the store that analytics needs. Narrowing it to
// an interface keeps the recorders unit-testable without a live Postgres.
type ClickWriter interface {
	InsertClick(ctx context.Context, ev store.ClickEvent) error
	InsertClicks(ctx context.Context, events []store.ClickEvent) error
}

// Stats is a snapshot of what a recorder has done. Every counter here exists
// because a failure it describes would otherwise be silent -- the whole point
// of choosing analytics as the subsystem allowed to fail is that the failure
// has to be visible (PLAN.md section 5.1).
type Stats struct {
	// Mode names the active strategy, "sync" or "batch".
	Mode string `json:"mode"`
	// Accepted counts events handed to Record.
	Accepted uint64 `json:"accepted"`
	// Written counts events Postgres confirmed.
	Written uint64 `json:"written"`
	// DroppedBufferFull counts events discarded because the buffer was full,
	// which is the deliberate backpressure policy rather than a malfunction.
	DroppedBufferFull uint64 `json:"dropped_buffer_full"`
	// DroppedAfterClose counts events that arrived during shutdown.
	DroppedAfterClose uint64 `json:"dropped_after_close"`
	// DroppedWriteFailed counts events lost to a failing database.
	DroppedWriteFailed uint64 `json:"dropped_write_failed"`
	// Batches counts flushes issued (always 0 for the synchronous recorder).
	Batches uint64 `json:"batches"`
	// BufferLen and BufferCap describe queue depth at snapshot time.
	BufferLen int `json:"buffer_len"`
	BufferCap int `json:"buffer_cap"`
}

// Dropped totals every category of loss.
func (s Stats) Dropped() uint64 {
	return s.DroppedBufferFull + s.DroppedAfterClose + s.DroppedWriteFailed
}

// Recorder accepts click events from the redirect path.
type Recorder interface {
	// Record submits one event. Implementations must not return an error:
	// analytics is explicitly allowed to degrade, and the redirect must never
	// fail because of it. The interface gives them no way to propagate
	// failure onto the hot path.
	Record(ctx context.Context, ev store.ClickEvent)
	// Stats reports what has been accepted, written and lost.
	Stats() Stats
	// Close flushes anything buffered and releases resources.
	Close(ctx context.Context) error
}

// counters is the atomic tally shared by both recorder implementations.
type counters struct {
	accepted           atomic.Uint64
	written            atomic.Uint64
	droppedBufferFull  atomic.Uint64
	droppedAfterClose  atomic.Uint64
	droppedWriteFailed atomic.Uint64
	batches            atomic.Uint64
}

// SyncRecorder writes each event to Postgres before returning, which puts a
// database round trip on the redirect's critical path. This is the phase 1
// baseline, kept so the phase 2 numbers have something to be measured against
// on identical hardware -- switch with LINKFLOW_ANALYTICS_MODE=sync.
type SyncRecorder struct {
	writer ClickWriter
	logger *slog.Logger
	// timeout bounds the insert so a stalled database degrades redirect
	// latency by a known amount instead of an unbounded one.
	timeout time.Duration
	counts  counters
}

// NewSync returns a recorder that inserts synchronously.
func NewSync(w ClickWriter, logger *slog.Logger) *SyncRecorder {
	return &SyncRecorder{writer: w, logger: logger, timeout: 2 * time.Second}
}

// Record inserts the event, logging and swallowing any failure.
func (r *SyncRecorder) Record(ctx context.Context, ev store.ClickEvent) {
	r.counts.accepted.Add(1)

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if err := r.writer.InsertClick(ctx, ev); err != nil {
		r.counts.droppedWriteFailed.Add(1)
		r.logger.Warn("click event lost", "code", ev.Code, "err", err)
		return
	}
	r.counts.written.Add(1)
}

// Stats reports the synchronous recorder's tallies.
func (r *SyncRecorder) Stats() Stats {
	return Stats{
		Mode:               "sync",
		Accepted:           r.counts.accepted.Load(),
		Written:            r.counts.written.Load(),
		DroppedWriteFailed: r.counts.droppedWriteFailed.Load(),
	}
}

// Close is a no-op: nothing is buffered.
func (r *SyncRecorder) Close(context.Context) error { return nil }
