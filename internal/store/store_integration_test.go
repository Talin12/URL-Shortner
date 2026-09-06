//go:build integration

// These tests need a live Postgres. Point LINKFLOW_TEST_DATABASE_URL at a
// throwaway database and run `make test-integration`; without it they skip,
// so the default `go test ./...` stays dependency-free.
package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Talin12/URL-Shortner/internal/shortcode"
)

// insert stores a link using the codec, mirroring what the creator does.
func insert(t *testing.T, s *Store, id uint64, destination string) Link {
	t.Helper()
	codec := shortcode.NewCodec(1)
	link, err := s.InsertLink(context.Background(), id, codec.Encode(id), destination)
	if err != nil {
		t.Fatalf("InsertLink: %v", err)
	}
	return link
}

func newTestStore(t *testing.T) *Store {
	t.Helper()

	url := os.Getenv("LINKFLOW_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LINKFLOW_TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s, err := New(ctx, url, 10)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestCreateAndResolve(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	link := insert(t, s, 4242, "https://example.com/one")
	if link.Code == "" {
		t.Fatal("InsertLink returned an empty code")
	}

	got, err := s.Destination(ctx, link.Code)
	if err != nil {
		t.Fatalf("Destination: %v", err)
	}
	if got != "https://example.com/one" {
		t.Errorf("Destination = %q, want %q", got, "https://example.com/one")
	}
}

func TestDestinationMissingCode(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Destination(context.Background(), "definitely-not-a-code")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Destination on unknown code = %v, want ErrNotFound", err)
	}
}

// Concurrent block claims must never overlap. The row lock the UPDATE takes is
// the only thing standing between two instances and duplicate short codes, so
// this exercises it against a real Postgres rather than a fake counter.
func TestConcurrentBlockClaimsNeverOverlap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const claims, size = 50, 1000
	starts := make(chan uint64, claims)
	errs := make(chan error, claims)

	for i := 0; i < claims; i++ {
		go func() {
			start, err := s.ClaimIDBlock(ctx, "links", size)
			if err != nil {
				errs <- err
				return
			}
			starts <- start
		}()
	}

	seen := make(map[uint64]bool, claims)
	for i := 0; i < claims; i++ {
		select {
		case err := <-errs:
			t.Fatalf("concurrent ClaimIDBlock: %v", err)
		case start := <-starts:
			if seen[start] {
				t.Fatalf("block starting at %d was handed out twice", start)
			}
			seen[start] = true
		}
	}

	// Every block must be a clean multiple of the size apart, or ranges
	// overlap somewhere.
	for start := range seen {
		if start%size != 0 {
			t.Errorf("block start %d is not aligned to the block size; ranges overlap", start)
		}
	}
}

func TestClaimUnknownBlockFails(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ClaimIDBlock(context.Background(), "not-a-block", 10); err == nil {
		t.Error("ClaimIDBlock succeeded for a name that does not exist")
	}
}

func TestClickInsertAndCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	link := insert(t, s, 5252, "https://example.com/clicks")

	if err := s.InsertClick(ctx, ClickEvent{Code: link.Code, OccurredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("InsertClick: %v", err)
	}

	batch := make([]ClickEvent, 50)
	for i := range batch {
		batch[i] = ClickEvent{Code: link.Code, OccurredAt: time.Now().UTC(), UserAgent: "bench"}
	}
	if err := s.InsertClicks(ctx, batch); err != nil {
		t.Fatalf("InsertClicks: %v", err)
	}

	count, err := s.ClickCount(ctx, link.Code)
	if err != nil {
		t.Fatalf("ClickCount: %v", err)
	}
	if count != 51 {
		t.Errorf("ClickCount = %d, want 51", count)
	}
}

func TestInsertClicksEmptyBatchIsNoop(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertClicks(context.Background(), nil); err != nil {
		t.Errorf("InsertClicks(nil) = %v, want nil", err)
	}
}
