// Package analytics records click events. The interface exists so the redirect
// handler never learns which strategy is in use: phase 1 ships a synchronous
// recorder that blocks the redirect on a Postgres insert, and phase 2 swaps in
// a bounded-channel batcher behind the same two methods.
package analytics

import (
	"context"
	"log/slog"
	"time"

	"github.com/Talin12/URL-Shortner/internal/store"
)

// Recorder accepts click events from the redirect path.
type Recorder interface {
	// Record submits one event. Implementations must not return an error:
	// analytics is explicitly allowed to degrade, and the redirect must never
	// fail because of it (PLAN.md section 5.1).
	Record(ctx context.Context, ev store.ClickEvent)
	// Close flushes anything buffered and releases resources.
	Close(ctx context.Context) error
}

// SyncRecorder writes each event to Postgres before returning, which puts a
// database round trip on the redirect's critical path. This is the phase 1
// baseline and it is meant to look bad under load.
type SyncRecorder struct {
	store  *store.Store
	logger *slog.Logger
	// timeout bounds the insert so a stalled database degrades redirect
	// latency by a known amount instead of an unbounded one.
	timeout time.Duration
}

// NewSync returns a recorder that inserts synchronously.
func NewSync(s *store.Store, logger *slog.Logger) *SyncRecorder {
	return &SyncRecorder{store: s, logger: logger, timeout: 2 * time.Second}
}

// Record inserts the event, logging and swallowing any failure.
func (r *SyncRecorder) Record(ctx context.Context, ev store.ClickEvent) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if err := r.store.InsertClick(ctx, ev); err != nil {
		r.logger.Warn("dropping click event", "code", ev.Code, "err", err)
	}
}

// Close is a no-op: nothing is buffered.
func (r *SyncRecorder) Close(context.Context) error { return nil }
