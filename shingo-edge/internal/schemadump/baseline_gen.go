package schemadump

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"shingoedge/store"
)

// BaselinePath is the committed baseline, relative to the shingo-edge module
// root: the Edge's whole schema and its schema_migrations rows as one SQL
// text, which store.migrate applies to a file with no tables. It is
// generated, never edited.
const BaselinePath = "store/schema/baseline.sql"

// BaselineRegenCommand is what the staleness test tells you to run. The
// snapshot is regenerated after it, by a build that embeds the new baseline.
const BaselineRegenCommand = "make schema-baseline (go run ./cmd/schemabaseline), then make schema-snapshot"

// GenerateBaseline builds the baseline and returns its text. SQLite only: no
// Docker.
//
//  1. An empty file is booted by store.RetiredChainRelease's own store.Open,
//     built from git (BuildRelease): the chain v1..RetiredChain, by the code
//     the plants ran.
//  2. This tree's migrations above RetiredChain run on it (store.Migrate).
//  3. Its sqlite_master statements are written out in the order the file
//     made them, with the schema_migrations rows. The text is the statements
//     SQLite stored, so their comments are the docs' column notes
//     (shared/scripts/docgen). The order is the plants': where two indexes
//     cost a query the same, SQLite's planner takes the one made later, so a
//     born file sorted by name planned a query differently from every
//     migrated one (TestKeptSpotRows_ReadsNoHistory).
//
// It refuses a result with a row in any other table: the Edge's chain seeds
// none, and the baseline carries none.
//
// moduleRoot is the shingo-edge directory; workDir holds the release build and
// is the caller's to remove.
func GenerateBaseline(ctx context.Context, moduleRoot, workDir string) (string, error) {
	rel, err := BuildRelease(ctx, moduleRoot, store.RetiredChainRelease, workDir)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(workDir, "baseline-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "baseline.db")
	if _, err := rel.Boot(ctx, path); err != nil {
		return "", err
	}
	if err := store.Migrate(path); err != nil {
		return "", fmt.Errorf("this tree's migrations on a %s database: %w", rel.Rev, err)
	}
	return renderBaseline(path)
}

func renderBaseline(path string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	stmts, err := catalogue(db, `rowid`)
	if err != nil {
		return "", err
	}
	tables, err := column(db, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_migrations' ORDER BY name`)
	if err != nil {
		return "", err
	}
	for _, t := range tables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + t + `"`).Scan(&n); err != nil {
			return "", fmt.Errorf("count %s: %w", t, err)
		}
		if n != 0 {
			return "", fmt.Errorf("%s has %d rows: the baseline carries no row but schema_migrations'", t, n)
		}
	}
	versions, err := column(db, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return "", err
	}
	recorded := map[string]bool{}
	for _, v := range versions {
		recorded[v] = true
	}
	for v := 1; v <= store.RetiredChain; v++ {
		if !recorded[fmt.Sprint(v)] {
			return "", fmt.Errorf("schema_migrations is missing v%d of v1..v%d", v, store.RetiredChain)
		}
	}
	var rows []string
	for i := 0; i < len(versions); i += 10 {
		rows = append(rows, "    ("+strings.Join(versions[i:min(i+10, len(versions))], "), (")+")")
	}
	body := Normalize(strings.Join(stmts, "\n\n") + "\n\nINSERT INTO schema_migrations (version) VALUES\n" +
		strings.Join(rows, ",\n") + ";\n")
	for _, note := range retiredNotes {
		if n := strings.Count(body, note); n != 1 {
			return "", fmt.Errorf("a retired note is in the dump %d times, want 1; correct retiredNotes: %.60q", n, note)
		}
		body = strings.Replace(body, note, "", 1)
	}
	return baselineHeader(versions[len(versions)-1]) + "\n" + body, nil
}

// retiredNotes are notes in RetiredChainRelease's DDL that are false once v60
// has run, taken out by their text; one not found stops the generator. DROP
// COLUMN removes a column and leaves the note above it, which would then be
// read as the next column's (load_type's over waybill_id, uop_capacity's over
// reorder_point). The third described the CREATE IF NOT EXISTS pass.
var retiredNotes = []string{
	`    -- load_type: dormant. Nothing on either module read it, and its writers went
    -- (2026-10-25); kept at its default so a rolled-back Edge's INSERT runs.
`,
	`    -- DEAD. Capacity is Core's fact, resolved from payload_catalog on every
    -- claim read (capacity.SQL) and keyed on payload_code above.
    -- Nothing reads or writes this column; it holds whatever it held when the
    -- copies stopped, and it goes on the next rebuildStyleNodeClaims(). It is
    -- not dropped here because a column rebuild is its own change with its own
    -- risk, and because the old numbers are the only record of how far the
    -- copies had drifted.
`,
	` Fresh databases only: this is
    -- CREATE TABLE IF NOT EXISTS and there is no ALTER for this column, so an
    -- existing plant DB keeps its current table definition and its current
    -- per-claim values either way.`,
}

func baselineHeader(head string) string {
	return strings.Join([]string{
		fmt.Sprintf("-- The Edge's schema at v%s: GENERATED, never edited by hand.", head),
		fmt.Sprintf("-- cmd/schemabaseline boots %s's chain (v1..v%d) on an empty file, runs this tree's", store.RetiredChainRelease, store.RetiredChain),
		"-- migrations above it, and writes out sqlite_master. The only rows are schema_migrations'.",
		"-- Regenerate: " + BaselineRegenCommand + ".",
		"-- store.migrate applies it, in one transaction, to a file with no tables.",
	}, "\n") + "\n"
}

// column reads a one-column query.
func column(db *sql.DB, query string) ([]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
