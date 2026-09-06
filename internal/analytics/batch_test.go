package analytics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Talin12/URL-Shortner/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeWriter stands in for the store. It copies each batch because flush
// reuses its backing array, so holding the slice would alias live memory.
type fakeWriter struct {
	mu      sync.Mutex
	batches [][]store.ClickEvent

	entered chan struct{}
	release chan struct{}
	err     error
}

func (f *fakeWriter) InsertClick(context.Context, store.ClickEvent) error { return f.err }

func (f *fakeWriter) InsertClicks(_ context.Context, events []store.ClickEvent) error {
	f.mu.Lock()
	f.batches = append(f.batches, append([]store.ClickEvent(nil), events...))
	f.mu.Unlock()

	// Non-blocking: a test only ever waits for the first flush or two, and a
	// blocking send here would wedge the batcher once nobody is reading.
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		<-f.release
	}
	return f.err
}

func (f *fakeWriter) batchSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	sizes := make([]int, len(f.batches))
	for i, b := range f.batches {
		sizes[i] = len(b)
	}
	return sizes
}

func event(code string) store.ClickEvent {
	return store.ClickEvent{Code: code, OccurredAt: time.Now().UTC()}
}

func TestBatchFlushesOnBatchSize(t *testing.T) {
	w := &fakeWriter{entered: make(chan struct{}, 4)}
	r := NewBatch(w, testLogger(), BatchConfig{
		BufferSize: 100, BatchSize: 3, FlushInterval: time.Hour, FlushTimeout: time.Minute,
	})
	t.Cleanup(func() { _ = r.Close(context.Background()) })

	for i := 0; i < 3; i++ {
		r.Record(context.Background(), event("abc"))
	}

	waitFor(t, w.entered, "flush on reaching batch size")
	if got := w.batchSizes(); len(got) != 1 || got[0] != 3 {
		t.Errorf("batch sizes = %v, want [3]", got)
	}
}

func TestBatchFlushesOnInterval(t *testing.T) {
	// A partial batch has to land on the timer, or a low-traffic link's clicks
	// would sit in memory until the buffer happened to fill.
	w := &fakeWriter{entered: make(chan struct{}, 4)}
	r := NewBatch(w, testLogger(), BatchConfig{
		BufferSize: 100, BatchSize: 1000, FlushInterval: 20 * time.Millisecond, FlushTimeout: time.Minute,
	})
	t.Cleanup(func() { _ = r.Close(context.Background()) })

	r.Record(context.Background(), event("abc"))
	r.Record(context.Background(), event("def"))

	waitFor(t, w.entered, "flush on interval")
	if got := w.batchSizes(); len(got) != 1 || got[0] != 2 {
		t.Errorf("batch sizes = %v, want [2]", got)
	}
}

func TestBatchDropsWhenBufferFull(t *testing.T) {
	// The point of the whole design: a stalled writer must cost analytics
	// events, never redirect availability.
	w := &fakeWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	r := NewBatch(w, testLogger(), BatchConfig{
		BufferSize: 4, BatchSize: 1, FlushInterval: time.Hour, FlushTimeout: time.Minute,
	})

	// This one is picked up by the batcher, which then blocks inside the write.
	r.Record(context.Background(), event("stuck"))
	waitFor(t, w.entered, "batcher to enter a blocking write")

	for i := 0; i < 4; i++ { // exactly fills the buffer
		r.Record(context.Background(), event("buffered"))
	}
	for i := 0; i < 3; i++ { // no room left
		r.Record(context.Background(), event("dropped"))
	}

	got := r.Stats()
	if got.DroppedBufferFull != 3 {
		t.Errorf("DroppedBufferFull = %d, want 3", got.DroppedBufferFull)
	}
	if got.Accepted != 8 {
		t.Errorf("Accepted = %d, want 8", got.Accepted)
	}
	if got.BufferLen != 4 || got.BufferCap != 4 {
		t.Errorf("buffer = %d/%d, want 4/4", got.BufferLen, got.BufferCap)
	}

	close(w.release)
	_ = r.Close(context.Background())
}

func TestBatchRecordNeverBlocks(t *testing.T) {
	w := &fakeWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	r := NewBatch(w, testLogger(), BatchConfig{
		BufferSize: 1, BatchSize: 1, FlushInterval: time.Hour, FlushTimeout: time.Minute,
	})

	r.Record(context.Background(), event("stuck"))
	waitFor(t, w.entered, "batcher to enter a blocking write")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			r.Record(context.Background(), event("flood"))
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked while the writer was stalled")
	}

	close(w.release)
	_ = r.Close(context.Background())
}

func TestBatchCloseFlushesBuffer(t *testing.T) {
	w := &fakeWriter{}
	r := NewBatch(w, testLogger(), BatchConfig{
		BufferSize: 100, BatchSize: 1000, FlushInterval: time.Hour, FlushTimeout: time.Minute,
	})

	for i := 0; i < 5; i++ {
		r.Record(context.Background(), event("abc"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := r.Stats().Written; got != 5 {
		t.Errorf("Written after Close = %d, want 5", got)
	}
}

func TestBatchCloseIsIdempotent(t *testing.T) {
	r := NewBatch(&fakeWriter{}, testLogger(), DefaultBatchConfig())
	ctx := context.Background()
	if err := r.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := r.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestBatchDropsAfterClose(t *testing.T) {
	r := NewBatch(&fakeWriter{}, testLogger(), DefaultBatchConfig())
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r.Record(context.Background(), event("late"))
	if got := r.Stats().DroppedAfterClose; got != 1 {
		t.Errorf("DroppedAfterClose = %d, want 1", got)
	}
}

func TestBatchCountsWriteFailures(t *testing.T) {
	w := &fakeWriter{entered: make(chan struct{}, 1), err: errors.New("postgres is down")}
	r := NewBatch(w, testLogger(), BatchConfig{
		BufferSize: 100, BatchSize: 2, FlushInterval: time.Hour, FlushTimeout: time.Minute,
	})
	t.Cleanup(func() { _ = r.Close(context.Background()) })

	r.Record(context.Background(), event("a"))
	r.Record(context.Background(), event("b"))
	waitFor(t, w.entered, "failing flush")

	// Give the batcher a moment to record the failure after the write returns.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := r.Stats(); s.DroppedWriteFailed == 2 {
			if s.Written != 0 {
				t.Errorf("Written = %d, want 0 when the write failed", s.Written)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("DroppedWriteFailed = %d, want 2", r.Stats().DroppedWriteFailed)
}

func TestSyncRecorderCountsFailures(t *testing.T) {
	w := &fakeWriter{err: errors.New("postgres is down")}
	r := NewSync(w, testLogger())

	r.Record(context.Background(), event("a"))

	got := r.Stats()
	if got.Mode != "sync" {
		t.Errorf("Mode = %q, want sync", got.Mode)
	}
	if got.Accepted != 1 || got.DroppedWriteFailed != 1 || got.Written != 0 {
		t.Errorf("stats = %+v, want 1 accepted / 1 failed / 0 written", got)
	}
}

func waitFor(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
