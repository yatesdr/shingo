package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The hand-copied edge DDL must not fall behind the real schema.
//
// edgeDDL exists so this test can validate the seeder's raw INSERT column names
// without importing the edge module. It is a COPY, and it was a copy of the
// subset the seeder happened to write — so the "no such column" tripwire only
// guarded columns already being written. A column the seeder does not write yet
// was absent from both, and the day somebody adds it to the seeder the test
// fails for a reason that looks like a typo rather than like drift.
//
// So the copy is held to the whole table. The authoritative shape is the
// committed schema snapshot, read as a FILE — cross-module import is what
// edgeDDL exists to avoid, and reading a checked-in artefact is not one.
// ---------------------------------------------------------------------------

// edgeSchemaSnapshot is the committed dump of a fresh edge database, relative to
// this package.
const edgeSchemaSnapshot = "../../../shingo-edge/store/schema/schema.snapshot.sql"

// guardedTables are the tables edgeDDL must mirror COMPLETELY. Others are
// present only to satisfy foreign keys and may stay minimal.
//
// style_node_claims is here because it is the table the seeder's coverage gap
// was found in and the one that grows every round.
var guardedTables = []string{"style_node_claims"}

func TestEdgeDDLMatchesTheSchemaSnapshot(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Clean(edgeSchemaSnapshot))
	if err != nil {
		t.Fatalf("read %s: %v\n(the snapshot is committed; regenerate with `make schema-snapshot` in shingo-edge)",
			edgeSchemaSnapshot, err)
	}
	snapshot := string(raw)

	for _, table := range guardedTables {
		want, ok := tableColumns(snapshot, table)
		if !ok {
			t.Errorf("no CREATE TABLE %s in %s — the snapshot or the table name has moved",
				table, edgeSchemaSnapshot)
			continue
		}
		got, ok := tableColumns(edgeDDL, table)
		if !ok {
			t.Errorf("edgeDDL has no CREATE TABLE %s", table)
			continue
		}
		have := make(map[string]bool, len(got))
		for _, c := range got {
			have[c] = true
		}
		var missing []string
		for _, c := range want {
			if !have[c] {
				missing = append(missing, c)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("edgeDDL's %s is missing %d column(s) the real schema has:\n  %s\n\n"+
				"edgeDDL is what makes seed_edge's INSERTs fail loudly on a wrong column name. "+
				"A column it does not declare is one the tripwire cannot guard, so the coverage "+
				"is exactly as wide as this copy — not as wide as the schema. Copy the "+
				"declarations across from %s.",
				table, len(missing), strings.Join(missing, "\n  "), edgeSchemaSnapshot)
		}
	}
}

// tableColumns returns the column names declared by ddl's CREATE TABLE for
// table, in declaration order.
func tableColumns(ddl, table string) ([]string, bool) {
	re := regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?` + regexp.QuoteMeta(table) + `\s*\(`)
	loc := re.FindStringIndex(ddl)
	if loc == nil {
		return nil, false
	}
	// Walk to the matching close paren, so a column with its own parentheses —
	// DEFAULT (datetime('now')), REFERENCES x(y) — does not end the body early.
	depth, start := 1, loc[1]
	end := -1
	for i := start; i < len(ddl); i++ {
		switch ddl[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil, false
	}
	return columnNames(ddl[start:end]), true
}

var lineComment = regexp.MustCompile(`--[^\n]*`)

func columnNames(body string) []string {
	body = lineComment.ReplaceAllString(body, "")
	var out []string
	depth, start := 0, 0
	parts := make([]string, 0, 48)
	for i, ch := range body {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, body[start:])
	for _, p := range parts {
		p = strings.Join(strings.Fields(p), " ")
		if p == "" {
			continue
		}
		name := strings.Fields(p)[0]
		switch strings.ToUpper(name) {
		case "UNIQUE", "PRIMARY", "FOREIGN", "CHECK", "CONSTRAINT":
			continue
		}
		out = append(out, name)
	}
	return out
}
