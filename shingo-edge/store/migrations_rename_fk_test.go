package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// A rename-rebuild must not leave the renamed table's children pointing at the
// scratch name.
//
// This is a regression test for damage that is already in production, not a
// hypothetical. Measured on the Springfield edge database dumped 2026-07-27
// (shingo-dumps/springfield-2026-07-27/edge.sql, md5
// 22fd294c2aa0b62d278c636e774f6a4a): 11 REFERENCES clauses across 8 tables name
// styles_legacy, processes_legacy or reporting_points_legacy — tables that were
// renamed and dropped by migrations.go years ago. Those 11 clauses account for
// 232,860 of that database's 234,624 foreign-key violations.
//
// The mechanism: since SQLite 3.25, ALTER TABLE ... RENAME rewrites the
// REFERENCES clauses stored in OTHER tables so they follow the rename. The
// rename/create/copy/drop helpers in migrations.go therefore re-point every
// child at the scratch table, and their closing DROP leaves the children
// dangling. Edge runs with foreign_keys OFF (store.Open), so nothing ever
// complained. db.rebuildTable now sets PRAGMA legacy_alter_table = ON and
// refuses to run if it did not take.
//
// Verified red: reverting rebuildTable to a bare db.Exec of the same script
// fails this test with
//
//	dangling REFERENCES after migrate(): [reporting_points -> "reporting_points_legacy"] ...
//
// which is the plant's exact clause shape.
func TestMigrate_RenameRebuildLeavesNoDanglingReferences(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "edge.db")

	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}

	// An old-vintage database, shaped so that every rename-rebuild helper that
	// touches a parent table actually fires:
	//   - processes carries active_job_style_id  -> migrateProcessColumns
	//   - styles carries line_id                 -> migrateStyleColumns
	//   - styles carries is_default              -> stripDeadStyleColumns
	//   - reporting_points carries job_style_id  -> migrateReportingPointColumns
	//   - hourly_counts carries line_id          -> migrateHourlyCountColumns
	// and children exist whose stored schema names those parents.
	const legacy = `
CREATE TABLE processes (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    name                 TEXT NOT NULL UNIQUE,
    description          TEXT NOT NULL DEFAULT '',
    active_job_style_id  INTEGER,
    target_job_style_id  INTEGER,
    production_state     TEXT NOT NULL DEFAULT 'active_production',
    created_at           TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE styles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    line_id     INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_default  INTEGER NOT NULL DEFAULT 0,
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE reporting_points (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    job_style_id INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    plc_name     TEXT NOT NULL,
    tag_name     TEXT NOT NULL,
    last_count   INTEGER NOT NULL DEFAULT 0,
    last_poll_at TEXT,
    enabled      INTEGER NOT NULL DEFAULT 1,
    warlink_managed INTEGER NOT NULL DEFAULT 0,
    UNIQUE(plc_name, tag_name)
);
CREATE TABLE counter_snapshots (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    reporting_point_id INTEGER NOT NULL REFERENCES reporting_points(id) ON DELETE CASCADE,
    count_value        INTEGER NOT NULL,
    delta              INTEGER NOT NULL DEFAULT 0,
    anomaly            TEXT,
    operator_confirmed INTEGER NOT NULL DEFAULT 0,
    recorded_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    line_id      INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    job_style_id INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(line_id, job_style_id, count_date, hour)
);
INSERT INTO processes (id, name) VALUES (1, 'Process A');
INSERT INTO styles (id, line_id, name) VALUES (1, 1, 'Style A');
INSERT INTO reporting_points (id, job_style_id, plc_name, tag_name) VALUES (1, 1, 'PLC1', 'TAG1');
INSERT INTO counter_snapshots (id, reporting_point_id, count_value) VALUES (1, 1, 7);
INSERT INTO hourly_counts (id, line_id, job_style_id, count_date, hour, delta) VALUES (1, 1, 1, '2026-07-27', 9, 3);
`
	if _, err := raw.Exec(legacy); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := Open(dbPath) // Open runs migrate()
	if err != nil {
		t.Fatalf("Open (runs migrate): %v", err)
	}
	defer db.Close()

	// 1. The primary metric from the plant repair: dangling REFERENCES clauses
	//    in the stored schema. Springfield reads 11; a migration that behaves
	//    must read 0.
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type='table' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()
	var dangling []string
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		for _, frag := range strings.Split(ddl, "REFERENCES ")[1:] {
			target := strings.TrimLeft(frag, ` "`)
			if i := strings.IndexAny(target, `" (`); i >= 0 {
				target = target[:i]
			}
			// _legacy / _old / _strip / _nofk are the scratch suffixes the
			// rename-rebuild helpers use.
			for _, suffix := range []string{"_legacy", "_old", "_strip", "_nofk"} {
				if strings.HasSuffix(target, suffix) {
					dangling = append(dangling, name+" -> "+target)
				}
			}
		}
	}
	if len(dangling) != 0 {
		t.Errorf("dangling REFERENCES after migrate(): %v\n"+
			"A rename-rebuild ran without PRAGMA legacy_alter_table=ON, so SQLite re-pointed "+
			"these clauses at the scratch table and the DROP left them naming nothing. "+
			"This is the defect that produced 232,860 of Springfield's 234,624 FK violations.", dangling)
	}

	// 2. And the consequence, measured the way the plant is measured.
	fkRows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer fkRows.Close()
	violations := 0
	for fkRows.Next() {
		violations++
	}
	if violations != 0 {
		t.Errorf("foreign_key_check = %d after migrate(), want 0", violations)
	}

	// 3. The rebuild must not have lost the rows it was moving.
	for _, c := range []struct {
		table string
		want  int
	}{
		{"processes", 1}, {"styles", 1}, {"reporting_points", 1},
		{"counter_snapshots", 1}, {"hourly_counts", 1},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM "` + c.table + `"`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if n != c.want {
			t.Errorf("%s has %d rows after migrate(), want %d", c.table, n, c.want)
		}
	}
}

// rebuildTable must refuse to run rather than run unguarded. The assertion is
// the whole point of the helper: PRAGMA is a statement that can silently
// no-op, and a no-op here is indistinguishable from success until a
// foreign_key_check years later.
func TestRebuildTable_AssertsLegacyAlterTableTook(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "edge.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// A script that renames a parent and drops the scratch table. With the
	// guard working, the child's clause survives naming the real table.
	if _, err := db.Exec(`CREATE TABLE t_parent (id INTEGER PRIMARY KEY);
		CREATE TABLE t_child (id INTEGER PRIMARY KEY, p INTEGER REFERENCES t_parent(id))`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.rebuildTable("t_parent", `
ALTER TABLE t_parent RENAME TO t_parent_old;
CREATE TABLE t_parent (id INTEGER PRIMARY KEY, extra TEXT NOT NULL DEFAULT '');
INSERT INTO t_parent (id) SELECT id FROM t_parent_old;
DROP TABLE t_parent_old;
`); err != nil {
		t.Fatalf("rebuildTable: %v", err)
	}

	var childDDL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='t_child'`).Scan(&childDDL); err != nil {
		t.Fatalf("read child ddl: %v", err)
	}
	if strings.Contains(childDDL, "t_parent_old") {
		t.Errorf("child clause followed the rename: %s\nPRAGMA legacy_alter_table did not hold across the rebuild", childDDL)
	}

	// And the pragma must be restored, or every later ALTER in this process
	// silently changes behaviour.
	var mode int
	if err := db.QueryRow(`PRAGMA legacy_alter_table`).Scan(&mode); err != nil {
		t.Fatalf("read legacy_alter_table: %v", err)
	}
	if mode != 0 {
		t.Errorf("legacy_alter_table = %d after rebuildTable, want 0 (must be restored)", mode)
	}
}
