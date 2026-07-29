package store

// migrations_soft_delete.go — the three schema changes the 2026-07 plant
// foreign-key repair depends on, for databases that already exist.
//
//	1. styles.deleted_at        + a live-only unique index
//	2. process_nodes.deleted_at + a live-only unique index
//	3. counter_snapshots.reporting_point_id -> ON DELETE CASCADE
//
// All three need a rename-rebuild, because SQLite cannot ALTER a table
// constraint or a foreign-key action in place. They go through db.rebuildTable
// so PRAGMA legacy_alter_table is ON and asserted — see the long argument on
// that function. A rebuild of a PARENT table (styles, process_nodes) done
// without it is precisely how the 11 dangling clauses this repair exists to
// fix came to be, so doing it wrong here would rebuild the defect while fixing
// it.
//
// There is no version marker to consult. `grep -rn schema_migrations
// shingo-edge/` returns nothing — the Edge migration runner is unversioned and
// every step must decide for itself whether it has already run. Each function
// below probes the stored schema text and returns early when the shape is
// already current.

import (
	"database/sql"
	"fmt"
	"strings"
)

// tableDDL returns the stored CREATE TABLE text, or "" when the table is
// absent. sqlite_master.sql is the only place a table constraint or an FK
// action is visible, so shape probes read it directly.
func tableDDL(db *sql.DB, table string) string {
	var s sql.NullString
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&s); err != nil {
		return ""
	}
	return s.String
}

// migrateStyleSoftDelete gives styles a deleted_at column and moves
// UNIQUE(process_id, name) off the table and onto a partial index that only
// covers live rows.
//
// The index move is not cosmetic. As a table constraint the UNIQUE applies to
// soft-deleted rows too, so retiring style "74368-6SA0A.95" and later
// re-creating it under the same name in the same process would fail with a
// constraint error the operator cannot act on. The partial index gives the
// intended behaviour: names are unique among live styles, and a tombstone
// holds no name.
func (db *DB) migrateStyleSoftDelete() error {
	ddl := tableDDL(db.DB, "styles")
	if ddl == "" {
		return nil // fresh DB; schema.Apply already created the current shape
	}
	// Idempotent ADD COLUMN first, so the rebuild's INSERT can name it and so a
	// database that only needs the column (never had the table UNIQUE) is done.
	// expected_catid is added here too: the rebuild's INSERT names it, and on a
	// vintage old enough to still carry the table UNIQUE the column may not
	// exist yet — SQLite resolves column names at prepare time, so a COALESCE
	// around a missing column still fails.
	db.Exec(`ALTER TABLE styles ADD COLUMN deleted_at TEXT`)
	db.Exec(`ALTER TABLE styles ADD COLUMN expected_catid TEXT NOT NULL DEFAULT ''`)

	if !strings.Contains(strings.ToUpper(strings.ReplaceAll(ddl, " ", "")), "UNIQUE(PROCESS_ID,NAME)") {
		// Already rebuilt, or a vintage that never carried the table
		// constraint. Ensure the index exists either way.
		_, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_styles_process_name_live
			ON styles(process_id, name) WHERE deleted_at IS NULL`)
		return err
	}

	// A pre-existing duplicate would make the new index fail to build and take
	// migrate() — and therefore Edge startup — down with it. The old table
	// constraint should have made duplicates impossible, but a plant schema is
	// whatever the migration path left behind, so check rather than assume.
	var dupes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT process_id, name FROM styles WHERE deleted_at IS NULL
		GROUP BY process_id, name HAVING COUNT(*) > 1)`).Scan(&dupes); err != nil {
		return fmt.Errorf("styles soft-delete: duplicate probe: %w", err)
	}
	if dupes > 0 {
		return fmt.Errorf("styles soft-delete: %d (process_id, name) pairs are duplicated among live styles; "+
			"the live-only unique index cannot be built until they are resolved", dupes)
	}

	return db.rebuildTable("styles", `
ALTER TABLE styles RENAME TO styles_softdel_old;
CREATE TABLE styles (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id     INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    expected_catid TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at     TEXT
);
INSERT INTO styles (id, process_id, name, description, expected_catid, created_at, deleted_at)
SELECT id, process_id, name, description, COALESCE(expected_catid, ''), created_at, deleted_at
FROM styles_softdel_old;
DROP TABLE styles_softdel_old;
CREATE UNIQUE INDEX IF NOT EXISTS idx_styles_process_name_live
    ON styles(process_id, name) WHERE deleted_at IS NULL;
`)
}

// migrateProcessNodeSoftDelete does the same for process_nodes.
//
// idx_process_nodes_process_core_name (built by collapseDuplicateProcessNodes)
// gains the same deleted_at predicate, for the same reason: a retired node
// must not reserve its core_node_name against a replacement.
func (db *DB) migrateProcessNodeSoftDelete() error {
	ddl := tableDDL(db.DB, "process_nodes")
	if ddl == "" {
		return nil
	}
	db.Exec(`ALTER TABLE process_nodes ADD COLUMN deleted_at TEXT`)

	if strings.Contains(strings.ToUpper(strings.ReplaceAll(ddl, " ", "")), "UNIQUE(PROCESS_ID,CODE)") {
		var dupes int
		if err := db.QueryRow(`SELECT COUNT(*) FROM (
			SELECT process_id, code FROM process_nodes WHERE deleted_at IS NULL
			GROUP BY process_id, code HAVING COUNT(*) > 1)`).Scan(&dupes); err != nil {
			return fmt.Errorf("process_nodes soft-delete: duplicate probe: %w", err)
		}
		if dupes > 0 {
			return fmt.Errorf("process_nodes soft-delete: %d (process_id, code) pairs are duplicated "+
				"among live nodes; the live-only unique index cannot be built until they are resolved", dupes)
		}
		if err := db.rebuildTable("process_nodes", `
ALTER TABLE process_nodes RENAME TO process_nodes_softdel_old;
CREATE TABLE process_nodes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id          INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    operator_station_id INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL,
    core_node_name      TEXT NOT NULL DEFAULT '',
    code                TEXT NOT NULL,
    name                TEXT NOT NULL,
    sequence            INTEGER NOT NULL DEFAULT 0,
    enabled             INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at          TEXT
);
INSERT INTO process_nodes (id, process_id, operator_station_id, core_node_name, code, name,
                           sequence, enabled, created_at, updated_at, deleted_at)
SELECT id, process_id, operator_station_id, COALESCE(core_node_name, ''), code, name,
       sequence, enabled, created_at, updated_at, deleted_at
FROM process_nodes_softdel_old;
DROP TABLE process_nodes_softdel_old;
`); err != nil {
			return err
		}
	}

	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_process_nodes_process_code_live
		ON process_nodes(process_id, code) WHERE deleted_at IS NULL`); err != nil {
		return fmt.Errorf("process_nodes soft-delete: live code index: %w", err)
	}
	// Re-point the core_node_name uniqueness at live rows only. This needs DROP
	// then CREATE, because CREATE ... IF NOT EXISTS would keep the old
	// (deleted_at-blind) predicate — but it must therefore be GUARDED, or it
	// re-runs on every boot. An unconditional DROP+CREATE here bumped
	// schema_version by two on every startup and was caught by
	// TestRebuildStyleNodeClaims_DoesNotRunAgain, which exists to make exactly
	// this mistake loud: churning a live plant's schema on each restart is a
	// real risk taken for no reason.
	//
	// "Absent" counts as needing it, not as nothing to do: the rebuild above
	// drops the table, which drops its indexes with it, so on the boot that
	// rebuilds there is no index to inspect. Treating that as "nothing to fix"
	// left collapseDuplicateProcessNodes to re-create the OLD predicate on the
	// next boot, and this block to correct it on the boot after — a schema that
	// churned for two startups instead of one. Measured on the Springfield
	// database: schema_version 66 -> 69 across the second Open.
	var coreNameIdx sql.NullString
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index'
		AND name='idx_process_nodes_process_core_name'`).Scan(&coreNameIdx); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("process_nodes soft-delete: read core-name index: %w", err)
	}
	if !coreNameIdx.Valid || !strings.Contains(coreNameIdx.String, "deleted_at") {
		if _, err := db.Exec(`DROP INDEX IF EXISTS idx_process_nodes_process_core_name;
			CREATE UNIQUE INDEX idx_process_nodes_process_core_name
				ON process_nodes(process_id, core_node_name)
				WHERE core_node_name <> '' AND deleted_at IS NULL`); err != nil {
			return fmt.Errorf("process_nodes soft-delete: live core-name index: %w", err)
		}
	}
	return nil
}

// migrateCounterSnapshotCascade turns counter_snapshots.reporting_point_id from
// NO ACTION into ON DELETE CASCADE.
//
// This is the edge that makes style deletion impossible once foreign_keys is
// enabled: styles -> reporting_points is CASCADE, and that cascade used to hit
// this NOT NULL / NO ACTION column as a restrict, aborting the whole delete.
// Measured on the Springfield dump with enforcement ON: 6 of 8 style deletions
// refused before, 0 of 57 after.
//
// The rebuild reads the FK action from pragma_foreign_key_list rather than
// grepping the DDL text, because "no action clause at all" and "ON DELETE NO
// ACTION" are the same thing to SQLite and only one of them is greppable.
func (db *DB) migrateCounterSnapshotCascade() error {
	if tableDDL(db.DB, "counter_snapshots") == "" {
		return nil
	}
	var onDelete string
	err := db.QueryRow(`SELECT "on_delete" FROM pragma_foreign_key_list('counter_snapshots')
		WHERE "from" = 'reporting_point_id'`).Scan(&onDelete)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil // no FK at all on this vintage; schema.Apply owns it
		}
		return fmt.Errorf("counter_snapshots cascade: read fk action: %w", err)
	}
	if strings.EqualFold(onDelete, "CASCADE") {
		return nil
	}

	// A dangling reporting_point_id would survive the rebuild and then be a
	// live FK violation under enforcement. Springfield has none once §0.5's
	// clause repair has run (the 232,392 "violations" here were entirely the
	// reporting_points_legacy clause), but a plant that has not been repaired
	// would carry real ones. Drop them rather than carry them forward: they are
	// readings whose reporting point no longer exists, and counters.
	// SnapshotRetention would delete them within 14 days regardless.
	db.Exec(`DELETE FROM counter_snapshots WHERE reporting_point_id NOT IN (SELECT id FROM reporting_points)`)

	return db.rebuildTable("counter_snapshots", `
ALTER TABLE counter_snapshots RENAME TO counter_snapshots_noc;
CREATE TABLE counter_snapshots (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    reporting_point_id INTEGER NOT NULL REFERENCES reporting_points(id) ON DELETE CASCADE,
    count_value        INTEGER NOT NULL,
    delta              INTEGER NOT NULL DEFAULT 0,
    anomaly            TEXT,
    operator_confirmed INTEGER NOT NULL DEFAULT 0,
    recorded_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO counter_snapshots (id, reporting_point_id, count_value, delta, anomaly, operator_confirmed, recorded_at)
SELECT id, reporting_point_id, count_value, delta, anomaly, operator_confirmed, recorded_at
FROM counter_snapshots_noc;
DROP TABLE counter_snapshots_noc;
CREATE INDEX IF NOT EXISTS idx_counter_snapshots_anomaly ON counter_snapshots(anomaly, operator_confirmed)
    WHERE anomaly IS NOT NULL AND operator_confirmed = 0;
`)
}
