// Package clickstore writes click events to ClickHouse.
//
// PLAN.md section 3 is explicit that "I used ClickHouse because it is fast" is
// worthless. This package exists so the alternative can be measured: it
// implements the same ClickWriter the Postgres store does, so the batcher can
// be pointed at either without changing anything upstream of it.
package clickstore

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Talin12/URL-Shortner/internal/store"
)

//go:embed schema/*.sql
var schemaFS embed.FS

// Store is a ClickHouse-backed click event sink. Safe for concurrent use.
type Store struct {
	conn driver.Conn
}

// Options configure the connection.
type Options struct {
	// Addr is host:port for the native protocol (9000), not HTTP (8123).
	Addr string
	// Database, Username and Password address the server.
	Database string
	Username string
	Password string
	// MaxOpenConns caps the pool. Only the batcher goroutine writes, so this
	// stays small -- ClickHouse strongly prefers few large inserts to many
	// small ones.
	MaxOpenConns int
}

// New dials ClickHouse and verifies the connection.
func New(ctx context.Context, opts Options) (*Store, error) {
	if opts.MaxOpenConns <= 0 {
		opts.MaxOpenConns = 5
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{opts.Addr},
		Auth: clickhouse.Auth{
			Database: opts.Database,
			Username: opts.Username,
			Password: opts.Password,
		},
		MaxOpenConns:    opts.MaxOpenConns,
		MaxIdleConns:    opts.MaxOpenConns,
		ConnMaxLifetime: time.Hour,
		DialTimeout:     5 * time.Second,
		// LZ4 costs a little CPU on the sender and saves a lot of network on
		// batches of a thousand near-identical rows.
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
	})
	if err != nil {
		return nil, fmt.Errorf("clickstore: open: %w", err)
	}

	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickstore: ping: %w", err)
	}
	return &Store{conn: conn}, nil
}

// Close releases the connection pool.
func (s *Store) Close() error { return s.conn.Close() }

// Ping reports whether ClickHouse is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.conn.Ping(ctx) }

// Migrate applies the embedded schema files in filename order.
func (s *Store) Migrate(ctx context.Context) error {
	names, err := fs.Glob(schemaFS, "schema/*.sql")
	if err != nil {
		return fmt.Errorf("clickstore: list schema files: %w", err)
	}
	sort.Strings(names)

	for _, name := range names {
		body, err := schemaFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("clickstore: read %s: %w", name, err)
		}
		// ClickHouse's native protocol takes one statement per call, and the
		// materialized view has to be created after the table it reads.
		for _, stmt := range splitStatements(string(body)) {
			if err := s.conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("clickstore: apply %s: %w", name, err)
			}
		}
	}
	return nil
}

// InsertClick writes a single event. Present to satisfy the writer interface;
// ClickHouse hates single-row inserts, which is precisely why the batching
// recorder is the only thing that should be driving this.
func (s *Store) InsertClick(ctx context.Context, ev store.ClickEvent) error {
	return s.InsertClicks(ctx, []store.ClickEvent{ev})
}

// InsertClicks writes a batch in one round trip.
func (s *Store) InsertClicks(ctx context.Context, events []store.ClickEvent) error {
	if len(events) == 0 {
		return nil
	}

	batch, err := s.conn.PrepareBatch(ctx, "INSERT INTO click_events (code, occurred_at, user_agent, referrer)")
	if err != nil {
		return fmt.Errorf("clickstore: prepare batch: %w", err)
	}

	for _, ev := range events {
		if err := batch.Append(ev.Code, ev.OccurredAt, ev.UserAgent, ev.Referrer); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("clickstore: append event: %w", err)
		}
	}

	if err := batch.Send(); err != nil {
		return fmt.Errorf("clickstore: send %d events: %w", len(events), err)
	}
	return nil
}

// ClickCount reads the pre-aggregated daily rollup rather than the raw events,
// which is the point of maintaining it on insert.
func (s *Store) ClickCount(ctx context.Context, code string) (int64, error) {
	var total uint64
	err := s.conn.QueryRow(ctx,
		"SELECT sum(clicks) FROM click_counts_daily WHERE code = ?", code,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("clickstore: count clicks for %q: %w", code, err)
	}
	return int64(total), nil
}

// splitStatements breaks a schema file into individual statements, ignoring
// semicolons that only appear inside comments.
func splitStatements(body string) []string {
	var cleaned strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		cleaned.WriteString(line)
		cleaned.WriteByte('\n')
	}

	var out []string
	for _, stmt := range strings.Split(cleaned.String(), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}
