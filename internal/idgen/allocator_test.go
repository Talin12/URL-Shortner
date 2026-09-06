package idgen

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeCounter stands in for the id_blocks row, with the same atomicity.
type fakeCounter struct {
	mu     sync.Mutex
	nextID uint64
	claims atomic.Int64
	err    error
}

func (f *fakeCounter) claim(_ context.Context, size uint64) (uint64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.claims.Add(1)

	f.mu.Lock()
	defer f.mu.Unlock()
	start := f.nextID
	f.nextID += size
	return start, nil
}

func TestOneClaimServesAWholeBlock(t *testing.T) {
	c := &fakeCounter{}
	a := New(c.claim, 1000)
	ctx := context.Background()

	for i := 0; i < 1000; i++ {
		if _, err := a.Next(ctx); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	if got := c.claims.Load(); got != 1 {
		t.Errorf("database claims = %d, want 1 for 1000 IDs", got)
	}
}

func TestBlockExhaustionClaimsAgain(t *testing.T) {
	c := &fakeCounter{}
	a := New(c.claim, 100)
	ctx := context.Background()

	for i := 0; i < 250; i++ {
		if _, err := a.Next(ctx); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	if got := c.claims.Load(); got != 3 {
		t.Errorf("database claims = %d, want 3 for 250 IDs in blocks of 100", got)
	}
}

// The invariant everything else rests on.
func TestConcurrentCallersNeverShareAnID(t *testing.T) {
	c := &fakeCounter{}
	a := New(c.claim, 64) // small blocks so refills happen constantly
	ctx := context.Background()

	const goroutines, perGoroutine = 50, 200

	var wg sync.WaitGroup
	results := make([][]uint64, goroutines)
	errs := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ids := make([]uint64, 0, perGoroutine)
			for i := 0; i < perGoroutine; i++ {
				id, err := a.Next(ctx)
				if err != nil {
					errs <- err
					return
				}
				ids = append(ids, id)
			}
			results[g] = ids
		}(g)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Next: %v", err)
	}

	seen := make(map[uint64]bool, goroutines*perGoroutine)
	for _, ids := range results {
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("id %d was handed out twice", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != goroutines*perGoroutine {
		t.Errorf("got %d unique ids, want %d", len(seen), goroutines*perGoroutine)
	}
}

// Two allocators sharing one counter is the multi-instance case: separate
// processes must never be handed overlapping blocks.
func TestSeparateAllocatorsNeverOverlap(t *testing.T) {
	c := &fakeCounter{}
	a, b := New(c.claim, 32), New(c.claim, 32)
	ctx := context.Background()

	seen := make(map[uint64]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, alloc := range []*Allocator{a, b} {
		for g := 0; g < 10; g++ {
			wg.Add(1)
			go func(alloc *Allocator) {
				defer wg.Done()
				for i := 0; i < 100; i++ {
					id, err := alloc.Next(ctx)
					if err != nil {
						t.Errorf("Next: %v", err)
						return
					}
					mu.Lock()
					if seen[id] {
						t.Errorf("id %d issued by both allocators", id)
					}
					seen[id] = true
					mu.Unlock()
				}
			}(alloc)
		}
	}
	wg.Wait()
}

func TestClaimFailureIsReported(t *testing.T) {
	c := &fakeCounter{err: errors.New("postgres is down")}
	a := New(c.claim, 10)

	if _, err := a.Next(context.Background()); err == nil {
		t.Error("Next succeeded despite a failing claim")
	}
}

func TestRemainingTracksTheBlock(t *testing.T) {
	c := &fakeCounter{}
	a := New(c.claim, 100)
	ctx := context.Background()

	if got := a.Remaining(); got != 0 {
		t.Errorf("Remaining before the first claim = %d, want 0", got)
	}
	for i := 0; i < 10; i++ {
		if _, err := a.Next(ctx); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if got := a.Remaining(); got != 90 {
		t.Errorf("Remaining = %d, want 90", got)
	}
}

func BenchmarkNextWithinBlock(b *testing.B) {
	c := &fakeCounter{}
	a := New(c.claim, uint64(b.N)+1000)
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		if _, err := a.Next(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
