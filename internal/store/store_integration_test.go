//go:build integration

// These tests need a live Postgres. Point LINKFLOW_TEST_DATABASE_URL at a
// throwaway database and run `make test-integration`; without it they skip,
// so the default `go test ./...` stays dependency-free.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Talin12/URL-Shortner/internal/shortcode"
)

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

	link, err := s.CreateLink(ctx, "https://example.com/one", shortcode.Encode)
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if link.Code == "" {
		t.Fatal("CreateLink returned an empty code")
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

func TestCodesAreUniqueAcrossConcurrentCreates(t *testing.T) {
	// The sequence is the coordination point phase 1 accepts and the block
	// allocator later removes. This test is what proves the replacement still
	// holds the same invariant.
	s := newTestStore(t)
	ctx := context.Background()

	const n = 200
	codes := make(chan string, n)
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			link, err := s.CreateLink(ctx, fmt.Sprintf("https://example.com/%d", i), shortcode.Encode)
			if err != nil {
				errs <- err
				return
			}
			codes <- link.Code
		}(i)
	}

	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatalf("concurrent CreateLink: %v", err)
		case code := <-codes:
			if seen[code] {
				t.Fatalf("duplicate code issued: %q", code)
			}
			seen[code] = true
		}
	}
}

func TestClickInsertAndCount(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	link, err := s.CreateLink(ctx, "https://example.com/clicks", shortcode.Encode)
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

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
