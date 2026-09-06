// Package store owns all Postgres access: the links table that backs the
// redirect path and the click_events table that backs analytics.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a code has no row in links.
var ErrNotFound = errors.New("store: link not found")

// Link is one row of the links table.
type Link struct {
	ID          uint64
	Code        string
	Destination string
	CreatedAt   time.Time
}

// ClickEvent is one redirect, recorded for analytics.
type ClickEvent struct {
	Code       string
	OccurredAt time.Time
	UserAgent  string
	Referrer   string
}

// Store wraps a pgx pool. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// New dials Postgres and verifies the connection before returning.
func New(ctx context.Context, databaseURL string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	cfg.MaxConns = maxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases every pooled connection.
func (s *Store) Close() { s.pool.Close() }

// Ping reports whether Postgres is reachable, for the readiness endpoint.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// CreateLink allocates an ID from the sequence, encodes it with the supplied
// function, and inserts the row. The encode callback keeps base62 (and later
// the Feistel bijection) out of the storage layer.
func (s *Store) CreateLink(ctx context.Context, destination string, encode func(uint64) string) (Link, error) {
	var id uint64
	if err := s.pool.QueryRow(ctx, `SELECT nextval('link_id_seq')`).Scan(&id); err != nil {
		return Link{}, fmt.Errorf("store: allocate id: %w", err)
	}

	link := Link{ID: id, Code: encode(id), Destination: destination}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO links (id, code, destination) VALUES ($1, $2, $3) RETURNING created_at`,
		link.ID, link.Code, link.Destination,
	).Scan(&link.CreatedAt)
	if err != nil {
		return Link{}, fmt.Errorf("store: insert link: %w", err)
	}
	return link, nil
}

// Destination resolves a code to its target URL. This is the redirect path's
// only query, and the one every cache tier added later exists to avoid.
func (s *Store) Destination(ctx context.Context, code string) (string, error) {
	var destination string
	err := s.pool.QueryRow(ctx, `SELECT destination FROM links WHERE code = $1`, code).Scan(&destination)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: lookup %q: %w", code, err)
	}
	return destination, nil
}

// Link returns the full row for a code.
func (s *Store) Link(ctx context.Context, code string) (Link, error) {
	var l Link
	err := s.pool.QueryRow(ctx,
		`SELECT id, code, destination, created_at FROM links WHERE code = $1`, code,
	).Scan(&l.ID, &l.Code, &l.Destination, &l.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Link{}, ErrNotFound
		}
		return Link{}, fmt.Errorf("store: load %q: %w", code, err)
	}
	return l, nil
}

// InsertClick writes a single click event. Phase 1 calls this synchronously on
// the redirect path on purpose: it is the baseline that phase 2's batched,
// decoupled write path is measured against.
func (s *Store) InsertClick(ctx context.Context, ev ClickEvent) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO click_events (code, occurred_at, user_agent, referrer) VALUES ($1, $2, $3, $4)`,
		ev.Code, ev.OccurredAt, ev.UserAgent, ev.Referrer,
	)
	if err != nil {
		return fmt.Errorf("store: insert click: %w", err)
	}
	return nil
}

// InsertClicks writes a batch of events in one round trip. Unused by phase 1
// but it is the call phase 2's batcher goroutine drains into.
func (s *Store) InsertClicks(ctx context.Context, events []ClickEvent) error {
	if len(events) == 0 {
		return nil
	}
	rows := make([][]any, len(events))
	for i, ev := range events {
		rows[i] = []any{ev.Code, ev.OccurredAt, ev.UserAgent, ev.Referrer}
	}
	_, err := s.pool.CopyFrom(ctx,
		pgx.Identifier{"click_events"},
		[]string{"code", "occurred_at", "user_agent", "referrer"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("store: insert %d clicks: %w", len(events), err)
	}
	return nil
}

// ClickCount returns the total events recorded for a code.
func (s *Store) ClickCount(ctx context.Context, code string) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM click_events WHERE code = $1`, code).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count clicks for %q: %w", code, err)
	}
	return n, nil
}
