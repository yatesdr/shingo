package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Edge v3 (drop_payload_catalog_cycle_seconds). The column was added by an
// unguarded frozen-chain ALTER that re-ran on every boot, removed in the same
// change as v3, so the proof here is not "v3 ran" but "the schema a boot
// leaves is the schema the next boot leaves": the full dump after boot 1 and
// after boot 2 must be byte-identical. A step that silently re-adds the
// column shows up as a difference, which "boot 2 applied no versions" cannot
// see.

// probeRaw answers a one-integer query over a plain connection. It is NOT a
// boot: Open would run the migration path, which is the thing under test.
func probeRaw(t *testing.T, path, query string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("probe %q: %v", query, err)
	}
	return n
}

const cycleSecondsProbe = `SELECT COUNT(*) FROM pragma_table_info('payload_catalog') WHERE name = 'cycle_seconds'`

// bootAndDump opens (boots) the DB at path, closes it, and returns the full
// schema dump the boot left behind.
func bootAndDump(t *testing.T, path string) string {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("boot %s: %v", path, err)
	}
	db.Close()
	return schemaDump(t, path)
}

// schemaDump is every sqlite_master row (type, name, table, CREATE text) in a
// fixed order. SQLite rewrites a table's stored CREATE text on ADD and DROP
// COLUMN, so a column coming back changes this dump. (internal/schemadump
// cannot be used here: it imports this package.)
func schemaDump(t *testing.T, path string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open for dump: %v", err)
	}
	defer raw.Close()
	rows, err := raw.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master
		ORDER BY type, name`)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, name, tbl, text string
		if err := rows.Scan(&typ, &name, &tbl, &text); err != nil {
			t.Fatalf("dump scan: %v", err)
		}
		b.WriteString(typ + " " + name + " ON " + tbl + "\n" + text + "\n\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump rows: %v", err)
	}
	return b.String()
}

// The plant path: an aged DB (no schema_migrations) whose payload_catalog
// carries cycle_seconds with non-zero values. Boot 1 drops it; boot 2 leaves
// the schema byte-identical.
func TestV4DropsCycleSeconds_AgedThenIdenticalSecondBoot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "aged.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE payload_catalog ADD COLUMN cycle_seconds REAL NOT NULL DEFAULT 0`,
		`INSERT INTO payload_catalog (id, name, code, cycle_seconds) VALUES (1, 'P', 'P', 12.5)`,
		`DROP TABLE schema_migrations`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("plant the aged shape: %v", err)
		}
	}
	db.Close()

	first := bootAndDump(t, path)
	if probeRaw(t, path, cycleSecondsProbe) != 0 {
		t.Fatal("cycle_seconds survived the first boot")
	}
	if n := probeRaw(t, path, `SELECT COUNT(*) FROM payload_catalog WHERE code = 'P'`); n != 1 {
		t.Fatalf("payload_catalog row lost with the column: %d rows, want 1", n)
	}

	second := bootAndDump(t, path)
	if first != second {
		t.Fatalf("the schema after boot 2 differs from the schema after boot 1 — something in the boot path "+
			"re-creates what a versioned migration dropped.\n%s", firstDumpDiff(first, second))
	}
}

// A fresh DB never has the column: v3 is a guarded no-op, and two boots
// leave identical schemas.
func TestV4DropsCycleSeconds_FreshIsNoOp(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	first := bootAndDump(t, path)
	second := bootAndDump(t, path)
	if first != second {
		t.Fatalf("fresh DB: boot 2 schema differs from boot 1.\n%s", firstDumpDiff(first, second))
	}
	if probeRaw(t, path, cycleSecondsProbe) != 0 {
		t.Fatal("a fresh DB has cycle_seconds")
	}
	if probeRaw(t, path, `SELECT COUNT(*) FROM schema_migrations WHERE version = `+
		strconv.Itoa(edgeMigrationVersion(t, "drop_payload_catalog_cycle_seconds"))) != 1 {
		t.Fatal("v3 not recorded on a fresh DB")
	}
}

func firstDumpDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) || i < len(lb); i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			return "first difference at line " + strconv.Itoa(i+1) + ":\n  boot 1: " + x + "\n  boot 2: " + y
		}
	}
	return "(no line difference)"
}
