package clickstore

import (
	"strings"
	"testing"
)

func TestSplitStatementsIgnoresComments(t *testing.T) {
	// The schema file is mostly comments, and the native protocol takes one
	// statement per call -- a semicolon inside a comment would split a
	// statement in half and produce a confusing server-side syntax error.
	schema := `
-- a comment; with a semicolon in it
CREATE TABLE a (x String) ENGINE = MergeTree ORDER BY x;

-- another; comment
CREATE TABLE b (y String) ENGINE = MergeTree ORDER BY y;
`
	got := splitStatements(schema)
	if len(got) != 2 {
		t.Fatalf("splitStatements returned %d statements, want 2: %#v", len(got), got)
	}
	for i, want := range []string{"CREATE TABLE a", "CREATE TABLE b"} {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("statement %d = %q, want prefix %q", i, got[i], want)
		}
	}
}

func TestSplitStatementsSkipsEmptyTrailer(t *testing.T) {
	if got := splitStatements("SELECT 1;\n\n;\n"); len(got) != 1 {
		t.Errorf("splitStatements = %#v, want one statement", got)
	}
}

func TestEmbeddedSchemaParses(t *testing.T) {
	body, err := schemaFS.ReadFile("schema/0001_init.sql")
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}

	stmts := splitStatements(string(body))
	if len(stmts) != 3 {
		t.Fatalf("embedded schema has %d statements, want 3 (events table, rollup table, materialized view)", len(stmts))
	}
	// The materialized view reads the events table, so it must come last.
	if !strings.Contains(stmts[2], "MATERIALIZED VIEW") {
		t.Errorf("last statement is not the materialized view: %q", stmts[2])
	}
}
