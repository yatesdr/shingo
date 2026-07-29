package store_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"shingoedge/store"
)

// The v33 rebuild is the most failure-prone migration on this branch: a live
// plant's style_node_claims is renamed, recreated, copied into and dropped,
// while three columns in two other tables hold REFERENCES to it. These tests
// build the pre-v33 shape by hand and put the real migration through it.

// oldStyleNodeClaims is the shape a plant carries: DEFAULT 1 on auto_reorder,
// DEFAULT 'simple' on swap_mode, no below_reorder_since. Trimmed to the columns
// that matter here; migrate()'s ALTER pass adds the rest before the rebuild
// runs, which is itself part of what this exercises.
const oldStyleNodeClaims = `
CREATE TABLE styles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL
);
CREATE TABLE style_node_claims (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    style_id                INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    core_node_name          TEXT NOT NULL,
    role                    TEXT NOT NULL DEFAULT 'consume',
    swap_mode               TEXT NOT NULL DEFAULT 'simple',
    payload_code            TEXT NOT NULL DEFAULT '',
    uop_capacity            INTEGER NOT NULL DEFAULT 0,
    reorder_point           INTEGER NOT NULL DEFAULT 0,
    auto_reorder            INTEGER NOT NULL DEFAULT 1,
    reorder_point_source    TEXT NOT NULL DEFAULT 'legacy',
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(style_id, core_node_name)
);
CREATE TABLE probe_referencing (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    claim_id INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL
);
`

// openAgedEdge builds a database carrying the pre-v33 shape and some rows, then
// opens it through the production path so the real migrate() runs against it.
func openAgedEdge(t *testing.T) (*store.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aged.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(oldStyleNodeClaims); err != nil {
		raw.Close()
		t.Fatalf("apply old shape: %v", err)
	}
	if _, err := raw.Exec(`
		INSERT INTO styles (id, name) VALUES (1, 'RUN-A');
		INSERT INTO style_node_claims
			(id, style_id, core_node_name, role, swap_mode, payload_code, uop_capacity,
			 reorder_point, auto_reorder, reorder_point_source)
		VALUES
			(7,  1, 'ALN_003', 'consume', 'two_robot',  'PANEL-B', 30, 50, 1, 'manual'),
			(11, 1, 'PLN_001', 'produce', 'sequential', 'PANEL-A', 30,  0, 0, 'legacy');
		INSERT INTO probe_referencing (id, claim_id) VALUES (1, 7);`); err != nil {
		raw.Close()
		t.Fatalf("seed rows: %v", err)
	}
	raw.Close()

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("migrate aged edge database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// The rebuild must not lose a row, change a value, or renumber an id. Claim ids
// are referenced from three columns across two other tables, so a renumber
// would silently re-point live changeover tasks and runtime states at the wrong
// claim.
func TestRebuildStyleNodeClaims_PreservesRowsAndIDs(t *testing.T) {
	db, _ := openAgedEdge(t)

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM style_node_claims`).Scan(&n); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if n != 2 {
		t.Fatalf("claim count = %d, want 2 — the rebuild lost rows", n)
	}

	var (
		node, swap, payload, source string
		reorder, auto               int
	)
	if err := db.QueryRow(`SELECT core_node_name, swap_mode, payload_code, reorder_point_source,
	                              reorder_point, auto_reorder
	                         FROM style_node_claims WHERE id = 7`).
		Scan(&node, &swap, &payload, &source, &reorder, &auto); err != nil {
		t.Fatalf("read claim 7: %v", err)
	}
	if node != "ALN_003" || swap != "two_robot" || payload != "PANEL-B" || source != "manual" {
		t.Errorf("claim 7 identity changed: node=%q swap=%q payload=%q source=%q", node, swap, payload, source)
	}
	if reorder != 50 {
		t.Errorf("reorder_point = %d, want 50", reorder)
	}
	// The per-claim VALUE is untouched. Only the column default moved, so a
	// claim that was armed stays armed — this migration must not disarm a
	// running plant.
	if auto != 1 {
		t.Errorf("auto_reorder = %d, want 1 — an armed claim must stay armed; only the DEFAULT changes", auto)
	}
}

// The point of the rebuild: no defaults on those two columns afterwards, and
// the new column present.
func TestRebuildStyleNodeClaims_DropsTheDefaults(t *testing.T) {
	db, _ := openAgedEdge(t)

	defaults := map[string]sql.NullString{}
	rows, err := db.Query(`SELECT name, dflt_value FROM pragma_table_info('style_node_claims')`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var dflt sql.NullString
		if err := rows.Scan(&name, &dflt); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		defaults[name] = dflt
	}

	// The target is the shape sqlite_ddl.go declares, NOT "no default":
	// auto_reorder keeps a default and it is 0, so a claim nobody armed lands
	// OFF. Asserting absence here would have been asserting the wrong thing,
	// and the migration would have had to rebuild the table on every startup to
	// satisfy it.
	if d, ok := defaults["auto_reorder"]; !ok || !d.Valid || d.String != "0" {
		t.Errorf("auto_reorder DEFAULT = %q (present=%v) — want 0; a plant defaulting to 1 arms every new claim", d.String, d.Valid)
	}
	if d, ok := defaults["swap_mode"]; !ok || d.Valid {
		t.Errorf("swap_mode still carries DEFAULT %q", d.String)
	}
	if _, ok := defaults["below_reorder_since"]; !ok {
		t.Error("below_reorder_since missing — the falling edge has nowhere durable to live")
	}

	// And the reason it matters: an INSERT that omits auto_reorder lands off.
	if _, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, swap_mode, payload_code)
	                      VALUES (1, 'HAND_WRITTEN', 'simple', 'PANEL-C')`); err != nil {
		t.Fatalf("hand-written insert: %v", err)
	}
	var auto int
	if err := db.QueryRow(`SELECT auto_reorder FROM style_node_claims WHERE core_node_name='HAND_WRITTEN'`).
		Scan(&auto); err != nil {
		t.Fatalf("read hand-written claim: %v", err)
	}
	if auto != 0 {
		t.Errorf("a hand-written INSERT that omits auto_reorder armed the claim (=%d) — this is the exposure the rebuild closes", auto)
	}
}

// PRAGMA legacy_alter_table is the load-bearing line in the migration. Without
// it, SQLite 3.25+ rewrites REFERENCES clauses in OTHER tables when the table is
// renamed, so all three referencing columns end up pointing at
// style_node_claims_legacy — which the migration then drops. Edge runs with
// foreign_keys OFF, so nothing would complain; the damage would sit in the
// stored schema until someone turned them on.
func TestRebuildStyleNodeClaims_ReferencesStillPointAtTheRealTable(t *testing.T) {
	db, _ := openAgedEdge(t)

	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='probe_referencing'`).
		Scan(&ddl); err != nil {
		t.Fatalf("read probe schema: %v", err)
	}
	if strings.Contains(ddl, "style_node_claims_legacy") {
		t.Fatalf("the rename re-pointed a foreign key at the scratch table, which was then dropped:\n%s\n"+
			"PRAGMA legacy_alter_table must be ON across the rebuild", ddl)
	}
	if !strings.Contains(ddl, "style_node_claims") {
		t.Fatalf("the reference to style_node_claims did not survive:\n%s", ddl)
	}

	// The scratch table must be gone, not left behind holding a copy of the
	// plant's claims.
	var leftovers int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='style_node_claims_legacy'`).
		Scan(&leftovers); err != nil {
		t.Fatalf("check leftovers: %v", err)
	}
	if leftovers != 0 {
		t.Error("style_node_claims_legacy survived the rebuild")
	}
}

// The rebuild must not RUN a second time, not merely survive running twice.
//
// It is a rename/create/copy/drop of a live plant's claims on every Edge
// startup otherwise, which is a real risk taken for no reason. The near-miss
// was branching on "does auto_reorder have a default" — it does, and correctly:
// the target shape keeps DEFAULT 0. A presence check is satisfied by a correct
// database and so would have rebuilt forever.
//
// Detected with PRAGMA schema_version, which SQLite bumps on every DDL change.
// Row ids are no good here — the copy carries them across deliberately, so they
// are identical whether or not the rebuild ran.
func TestRebuildStyleNodeClaims_DoesNotRunAgain(t *testing.T) {
	db, path := openAgedEdge(t)

	schemaVersion := func(d *store.DB) int64 {
		t.Helper()
		var v int64
		if err := d.QueryRow(`PRAGMA schema_version`).Scan(&v); err != nil {
			t.Fatalf("read schema_version: %v", err)
		}
		return v
	}
	before := schemaVersion(db)
	db.Close()

	again, err := store.Open(path)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	defer again.Close()

	if after := schemaVersion(again); after != before {
		t.Fatalf("the schema changed again on the second startup (schema_version %d -> %d).\n"+
			"The rebuild must run ONCE: renaming, copying and dropping a live plant's "+
			"claims on every boot is a real risk taken for no reason.", before, after)
	}
}

// Running the migration twice must also be harmless in what it leaves behind.
func TestRebuildStyleNodeClaims_Idempotent(t *testing.T) {
	db, path := openAgedEdge(t)
	db.Close()

	again, err := store.Open(path)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	defer again.Close()

	var n int
	if err := again.QueryRow(`SELECT count(*) FROM style_node_claims`).Scan(&n); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if n != 2 {
		t.Fatalf("claim count = %d after a second migrate, want 2", n)
	}
	var auto int
	if err := again.QueryRow(`SELECT auto_reorder FROM style_node_claims WHERE id=7`).Scan(&auto); err != nil {
		t.Fatalf("read claim 7: %v", err)
	}
	if auto != 1 {
		t.Errorf("auto_reorder = %d after a second migrate, want 1", auto)
	}
}
