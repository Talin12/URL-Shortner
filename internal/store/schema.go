package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
)

// schemaFS holds the SQL files applied by Migrate. Embedding them in the store
// package keeps the schema next to the queries that depend on it, and means
// the binary carries everything it needs to bootstrap an empty database.
//
//go:embed schema/*.sql
var schemaFS embed.FS

// Migrate applies every embedded schema file in filename order. The statements
// are all IF NOT EXISTS, so running this on each boot is safe; a multi-instance
// deployment would move to a versioned migration tool with advisory locking.
func (s *Store) Migrate(ctx context.Context) error {
	names, err := fs.Glob(schemaFS, "schema/*.sql")
	if err != nil {
		return fmt.Errorf("store: list schema files: %w", err)
	}
	sort.Strings(names)

	for _, name := range names {
		sqlText, err := schemaFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", name, err)
		}
		if _, err := s.pool.Exec(ctx, string(sqlText)); err != nil {
			return fmt.Errorf("store: apply %s: %w", name, err)
		}
	}
	return nil
}
