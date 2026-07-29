package store

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The baseline DDL must not index a column that a migration adds.
//
// WHY THIS EXISTS. The baseline in store/schema/postgres_ddl.go is applied on
// EVERY boot, before migrations run, and CREATE TABLE IF NOT EXISTS is a no-op
// on a database that already has the table. So putting a new column in the
// baseline CREATE TABLE is harmless — an existing database simply does not get
// it there — but putting an INDEX over that column in the baseline is not: on
// any existing database the index statement runs before the migration that
// adds the column, and the whole schema apply fails. Core does not start.
//
//	open database: migrate: schema apply:
//	ERROR: column "code" does not exist (SQLSTATE 42703)
//
// That is exactly what happened deploying migration 55 to the houseserver sim
// on 2026-07-25.
//
// EVERY OTHER TEST PASSED. Each spins up a FRESH database, where the baseline
// CREATE TABLE does include the column and the index is fine. Only the upgrade
// path breaks — which is every real deployment and no test. That asymmetry is
// the entire reason this file exists: it is the one check that reads the
// upgrade order rather than the end state.
//
// THE REPO ALREADY HAS THE ESCAPE HATCH. migrateAddBaselineColumns() runs
// BEFORE the baseline apply and exists for precisely this: "columns the
// baseline DDL assumes-present (e.g. via a CREATE INDEX on the column) but
// which are not added by any versioned migration." So a baseline index over a
// migration-added column is safe when — and only when — that column is also in
// that pre-baseline list.
//
// Migration 55's code/ref were not, which is why the boot failed. Two fixes
// were available: move the index into the migration (what ef0078ac did —
// simpler, and the convention) or add the columns to the pre-baseline list.
// This check enforces "one or the other", not a preference between them.

var (
	// CREATE INDEX ... ON <table> (<cols or expression>)
	baselineIndexRe = regexp.MustCompile(`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX[^;]*?\sON\s+([a-z_][a-z0-9_]*)\s*\(([^;]*?)\)\s*(?:WHERE[^;]*)?;`)
	// ALTER TABLE <table> ADD COLUMN [IF NOT EXISTS] <column>
	addColumnRe = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+([a-z_][a-z0-9_]*)\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	// An entry in migrateAddBaselineColumns' adds list: {"table", "column", `...`}
	preBaselineRe = regexp.MustCompile(`\{"([a-z_][a-z0-9_]*)",\s*"([a-z_][a-z0-9_]*)",\s*` + "`")
	// A bare identifier, to pull column names out of an index's column list or
	// expression (e.g. `(ref->>'payload')` yields ref and payload).
	identRe = regexp.MustCompile(`[a-z_][a-z0-9_]*`)
)

// sqlKeywords appear inside index expressions and are not column names.
var sqlKeywords = map[string]bool{
	"asc": true, "desc": true, "nulls": true, "first": true, "last": true,
	"text": true, "jsonb": true, "int": true, "bigint": true, "boolean": true,
	"timestamptz": true, "coalesce": true, "lower": true, "upper": true,
	"date_trunc": true, "gin": true, "gist": true, "btree": true, "using": true,
	"varchar": true, "numeric": true, "double": true, "precision": true,
}

func mustReadSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestBaselineDDL_DoesNotIndexMigrationAddedColumns(t *testing.T) {
	ddl := mustReadSource(t, "schema/postgres_ddl.go")
	migrations := mustReadSource(t, "migrations.go")

	// Every column any migration adds, keyed table -> column.
	added := map[string]map[string]bool{}
	for _, m := range addColumnRe.FindAllStringSubmatch(migrations, -1) {
		table, col := strings.ToLower(m[1]), strings.ToLower(m[2])
		if added[table] == nil {
			added[table] = map[string]bool{}
		}
		added[table][col] = true
	}
	if len(added) == 0 {
		t.Fatal("parsed no ADD COLUMN statements from migrations.go — the regex has drifted, " +
			"and a test that silently matches nothing is worse than no test")
	}

	// Columns migrateAddBaselineColumns adds AHEAD of the baseline apply. An
	// index over one of these is safe: the column is guaranteed present by the
	// time the baseline runs, on old and new databases alike.
	preBaseline := map[string]map[string]bool{}
	for _, m := range preBaselineRe.FindAllStringSubmatch(migrations, -1) {
		table, col := strings.ToLower(m[1]), strings.ToLower(m[2])
		if preBaseline[table] == nil {
			preBaseline[table] = map[string]bool{}
		}
		preBaseline[table][col] = true
	}
	if len(preBaseline) == 0 {
		t.Fatal("parsed no entries from migrateAddBaselineColumns — the regex has drifted, and " +
			"every legitimate pre-baseline column would now report as a violation")
	}

	// Every column the baseline indexes, keyed the same way.
	var violations []string
	for _, m := range baselineIndexRe.FindAllStringSubmatch(ddl, -1) {
		table := strings.ToLower(m[1])
		cols := added[table]
		if cols == nil {
			continue
		}
		for _, ident := range identRe.FindAllString(strings.ToLower(m[2]), -1) {
			if sqlKeywords[ident] {
				continue
			}
			if cols[ident] && !preBaseline[table][ident] {
				violations = append(violations,
					"baseline indexes "+table+"("+ident+"), added by a migration and NOT in "+
						"migrateAddBaselineColumns")
			}
		}
	}

	if len(violations) > 0 {
		t.Fatalf("baseline DDL indexes migration-added columns — Core will FAIL TO BOOT on any "+
			"EXISTING database, because the baseline is applied before migrations run and "+
			"CREATE TABLE IF NOT EXISTS does not add a column to a table that already exists:"+
			"\n  %s\n\n"+
			"Two fixes, either is fine:\n"+
			"  1. Move the CREATE INDEX into the migration that adds the column. Fresh installs "+
			"still get it — schema_migrations gates on version, so the migration runs on a new "+
			"database even when the baseline already satisfies its verify.\n"+
			"  2. Add the column to migrateAddBaselineColumns, which runs ahead of the baseline "+
			"for exactly this reason.\n\n"+
			"No other test catches this: they all run against FRESH databases, where the "+
			"baseline CREATE TABLE includes the column and the index is fine. Only the upgrade "+
			"path breaks, which is every real deployment.",
			strings.Join(violations, "\n  "))
	}
}

// The parsers are the load-bearing part — a regex that quietly stops matching
// turns the check above into a no-op that always passes. These pin both
// against the real shapes this repo uses.
func TestSchemaDriftParsers_MatchTheShapesInUse(t *testing.T) {
	ddl := mustReadSource(t, "schema/postgres_ddl.go")
	if n := len(baselineIndexRe.FindAllStringSubmatch(ddl, -1)); n < 10 {
		t.Errorf("baseline index regex matched %d indexes; the DDL has far more, so it has drifted", n)
	}

	migrations := mustReadSource(t, "migrations.go")
	if n := len(addColumnRe.FindAllStringSubmatch(migrations, -1)); n < 10 {
		t.Errorf("ADD COLUMN regex matched %d statements; migrations.go has far more, so it has drifted", n)
	}

	// The exact statement that caused the 2026-07-25 boot failure must be
	// recognised by both halves, or the check would not have caught it.
	sample := `CREATE INDEX IF NOT EXISTS idx_order_history_code
		ON order_history(code, created_at) WHERE code IS NOT NULL;`
	m := baselineIndexRe.FindStringSubmatch(sample)
	if m == nil || strings.ToLower(m[1]) != "order_history" {
		t.Fatalf("index regex did not parse the statement that broke the boot: %#v", m)
	}
	cols := identRe.FindAllString(strings.ToLower(m[2]), -1)
	if !contains(cols, "code") {
		t.Errorf("index column list %v did not yield 'code'", cols)
	}

	addSample := `ALTER TABLE order_history ADD COLUMN IF NOT EXISTS code TEXT`
	am := addColumnRe.FindStringSubmatch(addSample)
	if am == nil || strings.ToLower(am[2]) != "code" {
		t.Fatalf("ADD COLUMN regex did not parse the matching migration statement: %#v", am)
	}

	// The pre-baseline list is the exemption; failing to parse it would report
	// every legitimate entry as a violation.
	preSample := "{\"bins\", \"payload_code\", `ALTER TABLE bins ADD COLUMN IF NOT EXISTS payload_code TEXT`},"
	pm := preBaselineRe.FindStringSubmatch(preSample)
	if pm == nil || strings.ToLower(pm[1]) != "bins" || strings.ToLower(pm[2]) != "payload_code" {
		t.Fatalf("pre-baseline regex did not parse an adds-list entry: %#v", pm)
	}
	if n := len(preBaselineRe.FindAllStringSubmatch(migrations, -1)); n < 5 {
		t.Errorf("pre-baseline regex matched %d entries; migrateAddBaselineColumns has more", n)
	}

	// And an expression index must yield the columns inside it, or a
	// `(ref->>'payload')` index over a migration-added `ref` slips through.
	exprSample := `CREATE INDEX IF NOT EXISTS idx_x ON order_history((ref->>'payload')) WHERE ref IS NOT NULL;`
	em := baselineIndexRe.FindStringSubmatch(exprSample)
	if em == nil {
		t.Fatal("index regex did not parse an expression index")
	}
	if !contains(identRe.FindAllString(strings.ToLower(em[2]), -1), "ref") {
		t.Errorf("expression index did not yield 'ref' from %q", em[2])
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
