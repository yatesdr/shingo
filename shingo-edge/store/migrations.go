// migrations.go — Edge SQLite migration runner.
//
// Phase 6.0b extracted this from the 1013-line schema.go. Layout:
//
//   schema/sqlite_ddl.go     — canonical "fresh DB" CREATE TABLE constant
//   schema/schema.go         — Apply() + introspection helpers
//   migrations.go (this file) — legacyDropDDL constant, migrate() entry
//                              point, per-table rename/rebuild/strip
//                              helpers, db.tableHasColumn / db.tableExists
//                              wrappers (kept for migration_test.go's
//                              existing call sites).
//
// All migrations are idempotent — safe to re-run on an already-migrated
// DB. New columns are added via ALTER TABLE ... ADD COLUMN (SQLite
// silently fails on duplicates, which we ignore). Structural changes
// use the rename-rebuild pattern: rename existing → CREATE new →
// INSERT INTO ... SELECT → DROP old. Versioned per-column migrations
// run AFTER schema.Apply().

package store

import (
	"strings"

	"shingoedge/store/schema"
)

// legacyDropDDL drops tables that have been removed entirely from the
// canonical schema. Runs first in migrate() so the rest of the
// migration logic operates on a clean table set. DROP IF EXISTS is
// safe on every database state.
const legacyDropDDL = `
DROP TABLE IF EXISTS bom_entries;
DROP TABLE IF EXISTS inventory;
DROP TABLE IF EXISTS materials;
DROP TABLE IF EXISTS kanban_templates;
DROP TABLE IF EXISTS operator_screens;
`

// migrate runs the full forward-migration pipeline: legacy DROPs,
// legacy table renames, canonical CREATE (via schema.Apply), per-
// column renames/rebuilds, idempotent ALTER ADD COLUMN, and data
// fixups. Callable on databases of any age — every step is no-op on
// already-migrated state.
func (db *DB) migrate() error {
	// 1. Cleanup: drop tables removed from the canonical schema
	if _, err := db.Exec(legacyDropDDL); err != nil {
		return err
	}
	db.Exec("ALTER TABLE orders DROP COLUMN material_id")

	// 2. Rename legacy tables BEFORE schema.Apply so existing data
	//    migrates into the new table names instead of being orphaned
	//    behind a freshly-created empty replacement.
	db.Exec("ALTER TABLE production_lines RENAME TO processes")
	db.Exec("ALTER TABLE job_styles RENAME TO styles")
	db.Exec("ALTER TABLE location_nodes RENAME TO nodes")

	// 3. Canonical CREATE TABLE IF NOT EXISTS pass.
	if err := schema.Apply(db.DB); err != nil {
		return err
	}

	// 4. Per-table column renames (graceful rebuilds).
	if err := db.migrateProcessColumns(); err != nil {
		return err
	}
	if err := db.migrateStyleColumns(); err != nil {
		return err
	}
	if err := db.stripDeadStyleColumns(); err != nil {
		return err
	}
	if err := db.migrateReportingPointColumns(); err != nil {
		return err
	}
	if err := db.migrateHourlyCountColumns(); err != nil {
		return err
	}
	if err := db.migrateHourlyCountFKs(); err != nil {
		return err
	}
	if err := db.migrateOperatorStationColumns(); err != nil {
		return err
	}
	if err := db.migrateProcessNodeOwnership(); err != nil {
		return err
	}
	if err := db.stripLegacyRuntimeStateColumns(); err != nil {
		return err
	}

	// Remove vestigial loaded_bin_label and loaded_at columns (safe no-ops if absent)
	db.Exec("ALTER TABLE process_node_runtime_states DROP COLUMN loaded_bin_label")
	db.Exec("ALTER TABLE process_node_runtime_states DROP COLUMN loaded_at")

	// Drop zombie nodes table (replaced by process_nodes + core node sync)
	db.Exec("DROP TABLE IF EXISTS nodes")

	// Drop tables replaced by style_node_claims
	db.Exec("DROP TABLE IF EXISTS process_node_style_assignments")
	db.Exec("DROP TABLE IF EXISTS op_node_style_assignments_legacy")
	db.Exec("DROP TABLE IF EXISTS changeover_log")

	// 5. ALTER TABLE additions (idempotent — duplicate column adds fail
	//    silently in SQLite and we deliberately ignore the error).
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN swap_mode TEXT NOT NULL DEFAULT 'simple'")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN staging_node TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN release_node TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN keep_staged INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN evacuate_on_changeover INTEGER NOT NULL DEFAULT 0")

	// Rename staging_node → inbound_staging, release_node → outbound_staging on style_node_claims
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN inbound_staging TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN outbound_staging TEXT NOT NULL DEFAULT ''")
	db.Exec("UPDATE style_node_claims SET inbound_staging = staging_node WHERE staging_node != ''")
	db.Exec("UPDATE style_node_claims SET outbound_staging = release_node WHERE release_node != ''")

	// Source / destination routing on style_node_claims (collapsed from node/node_group pairs)
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN inbound_source_node TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN inbound_source_node_group TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN outbound_source_node TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN outbound_source_node_group TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN inbound_source TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN outbound_source TEXT NOT NULL DEFAULT ''")
	db.Exec("UPDATE style_node_claims SET inbound_source = COALESCE(NULLIF(inbound_source_node, ''), inbound_source_node_group) WHERE inbound_source = ''")
	db.Exec("UPDATE style_node_claims SET outbound_source = COALESCE(NULLIF(outbound_source_node, ''), outbound_source_node_group) WHERE outbound_source = ''")

	// Allowed payload codes and auto-request for manual_swap nodes
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN allowed_payload_codes TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN auto_request_payload TEXT NOT NULL DEFAULT ''")

	// 6. Data fixups
	db.Exec("UPDATE orders SET status='pending' WHERE status='queued'")

	// Legacy catalog renames
	db.Exec("ALTER TABLE style_catalog RENAME TO blueprint_catalog")
	db.Exec("ALTER TABLE blueprint_catalog DROP COLUMN form_factor")
	db.Exec("ALTER TABLE blueprint_catalog RENAME TO payload_catalog")

	// Legacy order columns
	db.Exec("ALTER TABLE orders ADD COLUMN steps_json TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE orders ADD COLUMN staged_expire_at TEXT")
	db.Exec("ALTER TABLE orders ADD COLUMN process_node_id INTEGER REFERENCES process_nodes(id) ON DELETE SET NULL")
	// Index must come after ALTER in case legacy orders table lacked the column
	db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_process_node_id ON orders(process_node_id)")

	// WarLink tag management tracking
	db.Exec("ALTER TABLE reporting_points ADD COLUMN warlink_managed INTEGER NOT NULL DEFAULT 0")

	// Counter config columns on processes
	db.Exec("ALTER TABLE processes ADD COLUMN counter_plc_name TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE processes ADD COLUMN counter_tag_name TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE processes ADD COLUMN counter_enabled INTEGER NOT NULL DEFAULT 0")

	// Migrate counter binding data to process columns
	if exists, _ := schema.TableExists(db.DB, "process_counter_bindings"); exists {
		db.Exec(`UPDATE processes SET
			counter_plc_name = COALESCE((SELECT plc_name FROM process_counter_bindings WHERE process_id = processes.id), ''),
			counter_tag_name = COALESCE((SELECT tag_name FROM process_counter_bindings WHERE process_id = processes.id), ''),
			counter_enabled = COALESCE((SELECT enabled FROM process_counter_bindings WHERE process_id = processes.id), 0)`)
		db.Exec("DROP TABLE process_counter_bindings")
	}

	// Auto-create default process if styles exist but no processes do
	var processCount int
	db.QueryRow("SELECT COUNT(*) FROM processes").Scan(&processCount)
	if processCount == 0 {
		var styleCount int
		db.QueryRow("SELECT COUNT(*) FROM styles").Scan(&styleCount)
		if styleCount > 0 {
			db.Exec("INSERT INTO processes (name, description) VALUES ('Line 1', 'Default production process')")
			db.Exec("UPDATE styles SET process_id = (SELECT id FROM processes WHERE name = 'Line 1') WHERE process_id IS NULL")
		}
	}

	// Rename pickup_node → source_node on orders (aligns with protocol SourceNode)
	db.Exec("ALTER TABLE orders RENAME COLUMN pickup_node TO source_node")

	// Rename outbound_source → outbound_destination on style_node_claims (it's a dropoff destination, not a source)
	db.Exec("ALTER TABLE style_node_claims RENAME COLUMN outbound_source TO outbound_destination")

	// A/B node cycling: paired_core_node on claims, active_pull on runtime
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN paired_core_node TEXT NOT NULL DEFAULT ''")

	// Claim-level auto-confirm: allows manual_swap claims to auto-confirm delivery
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN auto_confirm INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE process_node_runtime_states ADD COLUMN active_pull INTEGER NOT NULL DEFAULT 1")

	// Bin loader/unloader mode: "loader" (default) or "unloader"
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN mode TEXT NOT NULL DEFAULT 'loader'")

	// v15: Convert bin_loader role to produce/consume with manual_swap.
	// The mode column stays as a harmless vestige — code no longer reads it.
	db.Exec(`UPDATE style_node_claims SET role = 'produce', swap_mode = 'manual_swap' WHERE role = 'bin_loader' AND (mode = 'loader' OR mode = '')`)
	db.Exec(`UPDATE style_node_claims SET role = 'consume', swap_mode = 'manual_swap' WHERE role = 'bin_loader' AND mode = 'unloader'`)

	// v15 backfill: manual_swap claims need allowed_payload_codes populated.
	// Seed from the legacy single payload_code for any manual_swap claim that was
	// converted before this backfill existed (otherwise the edit modal's picker
	// is empty and Save rejects with "Select at least one allowed payload").
	db.Exec(`UPDATE style_node_claims
		SET allowed_payload_codes = '["' || payload_code || '"]'
		WHERE swap_mode = 'manual_swap'
		  AND (allowed_payload_codes = '' OR allowed_payload_codes = '[]')
		  AND payload_code <> ''`)

	// v16: Add payload_code to orders for per-payload demand mapping.
	db.Exec("ALTER TABLE orders ADD COLUMN payload_code TEXT NOT NULL DEFAULT ''")

	// v17 (lineside phase 6): per-claim soft threshold for the release
	// qty-override prompt. Zero means "off" — the default. When >0, the
	// HMI warns if the operator enters a qty greater than 2× this value.
	db.Exec("ALTER TABLE style_node_claims ADD COLUMN lineside_soft_threshold INTEGER NOT NULL DEFAULT 0")

	return nil
}

// ── Table rebuild helpers (rename-rebuild pattern) ───────────────────

// migrateProcessColumns renames active_job_style_id → active_style_id
func (db *DB) migrateProcessColumns() error {
	has, err := schema.TableHasColumn(db.DB, "processes", "active_job_style_id")
	if err != nil || !has {
		// Already migrated or fresh DB — ensure new columns exist
		db.Exec("ALTER TABLE processes ADD COLUMN target_style_id INTEGER REFERENCES styles(id) ON DELETE SET NULL")
		db.Exec("ALTER TABLE processes ADD COLUMN production_state TEXT NOT NULL DEFAULT 'active_production'")
		return err
	}
	// Rebuild table with new column names
	_, err = db.Exec(`
ALTER TABLE processes RENAME TO processes_legacy;
CREATE TABLE processes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    active_style_id     INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    target_style_id     INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    production_state    TEXT NOT NULL DEFAULT 'active_production',
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO processes (id, name, description, active_style_id, target_style_id, production_state, created_at)
SELECT id, name, description, active_job_style_id,
    CASE WHEN EXISTS(SELECT 1 FROM pragma_table_info('processes_legacy') WHERE name='target_job_style_id')
         THEN target_job_style_id ELSE NULL END,
    COALESCE(CASE WHEN EXISTS(SELECT 1 FROM pragma_table_info('processes_legacy') WHERE name='production_state')
         THEN production_state ELSE NULL END, 'active_production'),
    created_at
FROM processes_legacy;
DROP TABLE processes_legacy;
`)
	return err
}

// migrateStyleColumns renames line_id → process_id
func (db *DB) migrateStyleColumns() error {
	hasLineID, err := schema.TableHasColumn(db.DB, "styles", "line_id")
	if err != nil {
		return err
	}
	if !hasLineID {
		// Fresh DB or already migrated — ensure process_id column
		db.Exec("ALTER TABLE styles ADD COLUMN process_id INTEGER REFERENCES processes(id) ON DELETE CASCADE")
		return nil
	}
	// Has line_id — rebuild.
	_, err = db.Exec(`
ALTER TABLE styles RENAME TO styles_legacy;
CREATE TABLE styles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id  INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, name)
);
INSERT INTO styles (id, process_id, name, description, created_at)
SELECT id, line_id, name, description, created_at
FROM styles_legacy;
DROP TABLE styles_legacy;
`)
	return err
}

// stripDeadStyleColumns removes unused is_default and active columns from styles.
func (db *DB) stripDeadStyleColumns() error {
	has, _ := schema.TableHasColumn(db.DB, "styles", "is_default")
	if !has {
		return nil
	}
	_, err := db.Exec(`
ALTER TABLE styles RENAME TO styles_strip;
CREATE TABLE styles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id  INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, name)
);
INSERT INTO styles (id, process_id, name, description, created_at)
SELECT id, process_id, name, description, created_at
FROM styles_strip;
DROP TABLE styles_strip;
`)
	return err
}

// migrateReportingPointColumns renames job_style_id → style_id
func (db *DB) migrateReportingPointColumns() error {
	has, err := schema.TableHasColumn(db.DB, "reporting_points", "job_style_id")
	if err != nil || !has {
		return err
	}
	_, err = db.Exec(`
ALTER TABLE reporting_points RENAME TO reporting_points_legacy;
CREATE TABLE reporting_points (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    style_id        INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    plc_name        TEXT NOT NULL,
    tag_name        TEXT NOT NULL,
    last_count      INTEGER NOT NULL DEFAULT 0,
    last_poll_at    TEXT,
    enabled         INTEGER NOT NULL DEFAULT 1,
    warlink_managed INTEGER NOT NULL DEFAULT 0,
    UNIQUE(plc_name, tag_name)
);
INSERT INTO reporting_points (id, style_id, plc_name, tag_name, last_count, last_poll_at, enabled, warlink_managed)
SELECT id, job_style_id, plc_name, tag_name, last_count, last_poll_at, enabled, COALESCE(warlink_managed, 0)
FROM reporting_points_legacy;
DROP TABLE reporting_points_legacy;
`)
	return err
}

// migrateHourlyCountColumns renames line_id → process_id, job_style_id → style_id
func (db *DB) migrateHourlyCountColumns() error {
	has, err := schema.TableHasColumn(db.DB, "hourly_counts", "line_id")
	if err != nil || !has {
		return err
	}
	_, err = db.Exec(`
ALTER TABLE hourly_counts RENAME TO hourly_counts_legacy;
CREATE TABLE hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date, hour)
);
INSERT INTO hourly_counts (id, process_id, style_id, count_date, hour, delta, updated_at)
SELECT id, line_id, job_style_id, count_date, hour, delta, updated_at
FROM hourly_counts_legacy;
DROP TABLE hourly_counts_legacy;
`)
	return err
}

// migrateHourlyCountFKs adds foreign keys to hourly_counts if missing.
func (db *DB) migrateHourlyCountFKs() error {
	var tableSql string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='hourly_counts'`).Scan(&tableSql)
	if err != nil {
		return nil // table doesn't exist yet, schema constant will create it
	}
	if strings.Contains(tableSql, "REFERENCES") {
		return nil // already has FKs
	}
	// Prune orphans before adding FK constraints
	db.Exec(`DELETE FROM hourly_counts WHERE process_id NOT IN (SELECT id FROM processes)`)
	db.Exec(`DELETE FROM hourly_counts WHERE style_id NOT IN (SELECT id FROM styles)`)
	_, err = db.Exec(`
ALTER TABLE hourly_counts RENAME TO hourly_counts_nofk;
CREATE TABLE hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date, hour)
);
INSERT INTO hourly_counts (id, process_id, style_id, count_date, hour, delta, updated_at)
SELECT id, process_id, style_id, count_date, hour, delta, updated_at
FROM hourly_counts_nofk;
DROP TABLE hourly_counts_nofk;
`)
	return err
}

// migrateOperatorStationColumns removes parent_station_id and expected_client_type
func (db *DB) migrateOperatorStationColumns() error {
	hasParent, err := schema.TableHasColumn(db.DB, "operator_stations", "parent_station_id")
	if err != nil {
		return err
	}
	hasExpected, _ := schema.TableHasColumn(db.DB, "operator_stations", "expected_client_type")
	if !hasParent && !hasExpected {
		// Add columns that may be missing on legacy DBs
		db.Exec("ALTER TABLE operator_stations ADD COLUMN note TEXT NOT NULL DEFAULT ''")
		db.Exec("ALTER TABLE operator_stations ADD COLUMN controller_node_id TEXT NOT NULL DEFAULT ''")
		return nil
	}
	_, err = db.Exec(`
ALTER TABLE operator_stations RENAME TO operator_stations_legacy;
CREATE TABLE operator_stations (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id         INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    code               TEXT NOT NULL,
    name               TEXT NOT NULL,
    note               TEXT NOT NULL DEFAULT '',
    area_label         TEXT NOT NULL DEFAULT '',
    sequence           INTEGER NOT NULL DEFAULT 0,
    controller_node_id TEXT NOT NULL DEFAULT '',
    device_mode        TEXT NOT NULL DEFAULT 'touch_hmi',
    enabled            INTEGER NOT NULL DEFAULT 1,
    health_status      TEXT NOT NULL DEFAULT 'offline',
    last_seen_at       TEXT,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);
INSERT INTO operator_stations (
    id, process_id, code, name, note, area_label, sequence, controller_node_id,
    device_mode, enabled, health_status, last_seen_at, created_at, updated_at
)
SELECT
    id, process_id, code, name, COALESCE(note, ''), COALESCE(area_label, ''), COALESCE(sequence, 0), COALESCE(controller_node_id, ''),
    COALESCE(device_mode, 'touch_hmi'), COALESCE(enabled, 1), COALESCE(health_status, 'offline'), last_seen_at, created_at, updated_at
FROM operator_stations_legacy;
DROP TABLE operator_stations_legacy;
`)
	return err
}

// ── Process node ownership migration ────────────────────────────────

// migrateProcessNodeOwnership migrates from op_station_nodes + junction table to inline operator_station_id
func (db *DB) migrateProcessNodeOwnership() error {
	// Phase 1: migrate from old op_station_nodes table if it exists
	opStationNodesExist, err := schema.TableExists(db.DB, "op_station_nodes")
	if err != nil {
		return err
	}
	if opStationNodesExist {
		if err := db.rebuildFromOpStationNodes(); err != nil {
			return err
		}
	}

	// Phase 2: migrate from junction table to inline FK (if junction table exists)
	junctionExists, err := schema.TableExists(db.DB, "operator_station_process_nodes")
	if err != nil {
		return err
	}
	if junctionExists {
		hasInlineFK, err := schema.TableHasColumn(db.DB, "process_nodes", "operator_station_id")
		if err != nil {
			return err
		}
		if !hasInlineFK {
			// Need to rebuild process_nodes with inline FK (simplified schema)
			_, err = db.Exec(`
ALTER TABLE process_nodes RENAME TO process_nodes_legacy;
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
    UNIQUE(process_id, code)
);
INSERT INTO process_nodes (
    id, process_id, operator_station_id, core_node_name, code, name, sequence, enabled, created_at, updated_at
)
SELECT
    n.id, n.process_id, d.operator_station_id, COALESCE(n.core_node_name, ''), n.code, n.name, n.sequence, n.enabled, n.created_at, n.updated_at
FROM process_nodes_legacy n
LEFT JOIN operator_station_process_nodes d ON d.process_node_id = n.id;
DROP TABLE process_nodes_legacy;
`)
			if err != nil {
				return err
			}
		}

		// Drop junction table
		db.Exec("DROP TABLE IF EXISTS operator_station_process_nodes")
	}

	// Strip old routing/allows columns from process_nodes if they exist
	if err := db.stripLegacyProcessNodeColumns(); err != nil {
		return err
	}

	// Also rebuild related tables that had op_node_id references
	if err := db.rebuildLegacyAssignments(); err != nil {
		return err
	}
	if err := db.rebuildLegacyRuntimeStates(); err != nil {
		return err
	}
	if err := db.rebuildLegacyOrders(); err != nil {
		return err
	}
	if err := db.rebuildLegacyChangeoverNodeTasks(); err != nil {
		return err
	}

	return nil
}

func (db *DB) stripLegacyRuntimeStateColumns() error {
	hasOldCol, _ := schema.TableHasColumn(db.DB, "process_node_runtime_states", "effective_style_id")
	if !hasOldCol {
		return nil
	}
	_, err := db.Exec(`
ALTER TABLE process_node_runtime_states RENAME TO process_node_runtime_states_old;
CREATE TABLE process_node_runtime_states (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_node_id    INTEGER NOT NULL UNIQUE REFERENCES process_nodes(id) ON DELETE CASCADE,
    active_claim_id    INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    remaining_uop      INTEGER NOT NULL DEFAULT 0,
    active_order_id    INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    staged_order_id    INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO process_node_runtime_states (id, process_node_id, remaining_uop, active_order_id, staged_order_id, updated_at)
SELECT id, process_node_id, remaining_uop, active_order_id, staged_order_id, updated_at
FROM process_node_runtime_states_old;
DROP TABLE process_node_runtime_states_old;
`)
	return err
}

func (db *DB) stripLegacyProcessNodeColumns() error {
	hasPositionType, _ := schema.TableHasColumn(db.DB, "process_nodes", "position_type")
	if !hasPositionType {
		return nil // already stripped
	}
	_, err := db.Exec(`
ALTER TABLE process_nodes RENAME TO process_nodes_old;
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
    UNIQUE(process_id, code)
);
INSERT INTO process_nodes (id, process_id, operator_station_id, core_node_name, code, name, sequence, enabled, created_at, updated_at)
SELECT id, process_id, operator_station_id, COALESCE(core_node_name, ''), code, name, sequence, enabled, created_at, updated_at
FROM process_nodes_old;
DROP TABLE process_nodes_old;
`)
	return err
}

func (db *DB) rebuildFromOpStationNodes() error {
	// Check if process_nodes already has data
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_nodes`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		db.Exec("DROP TABLE IF EXISTS op_station_nodes")
		return nil
	}

	hasCoreNodeName, _ := schema.TableHasColumn(db.DB, "op_station_nodes", "core_node_name")
	coreNodeExpr := "''"
	if hasCoreNodeName {
		coreNodeExpr = "COALESCE(n.core_node_name, '')"
	}

	// Migrate directly to inline operator_station_id model (simplified schema)
	_, err := db.Exec(`
INSERT INTO process_nodes (
    id, process_id, operator_station_id, core_node_name, code, name, sequence, enabled, created_at, updated_at
)
SELECT
    n.id, s.process_id, n.operator_station_id, ` + coreNodeExpr + `, n.code, n.name, COALESCE(n.sequence, 0),
    COALESCE(n.enabled, 1), n.created_at, n.updated_at
FROM op_station_nodes n
JOIN operator_stations s ON s.id = n.operator_station_id;
`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`DROP TABLE IF EXISTS op_station_nodes`)
	return err
}

func (db *DB) rebuildLegacyAssignments() error {
	// Legacy assignment tables are no longer needed (replaced by style_node_claims).
	// Just drop them if they exist.
	for _, name := range []string{"op_node_style_assignments_legacy", "op_node_style_assignments"} {
		db.Exec(`DROP TABLE IF EXISTS ` + name)
	}
	return nil
}

func (db *DB) rebuildLegacyRuntimeStates() error {
	for _, name := range []string{"op_node_runtime_states_legacy", "op_node_runtime_states"} {
		has, err := schema.TableHasColumn(db.DB, name, "op_node_id")
		if err != nil || !has {
			continue
		}
		_, err = db.Exec(`INSERT OR IGNORE INTO process_node_runtime_states (
    id, process_node_id, remaining_uop,
    active_order_id, staged_order_id, updated_at
)
SELECT id, op_node_id, remaining_uop,
    active_order_id, staged_order_id, updated_at
FROM ` + name)
		if err != nil {
			return err
		}
		db.Exec(`DROP TABLE IF EXISTS ` + name)
		return nil
	}
	return nil
}

func (db *DB) rebuildLegacyOrders() error {
	has, err := schema.TableHasColumn(db.DB, "orders", "op_node_id")
	if err != nil || !has {
		return err
	}
	_, err = db.Exec(`
ALTER TABLE orders RENAME TO orders_legacy;
CREATE TABLE orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid            TEXT NOT NULL UNIQUE,
    order_type      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    process_node_id INTEGER REFERENCES process_nodes(id) ON DELETE SET NULL,
    retrieve_empty  INTEGER NOT NULL DEFAULT 1,
    quantity        INTEGER NOT NULL DEFAULT 0,
    delivery_node   TEXT NOT NULL DEFAULT '',
    staging_node    TEXT NOT NULL DEFAULT '',
    source_node     TEXT NOT NULL DEFAULT '',
    load_type       TEXT NOT NULL DEFAULT '',
    waybill_id      TEXT,
    external_ref    TEXT,
    final_count     INTEGER,
    count_confirmed INTEGER NOT NULL DEFAULT 0,
    eta             TEXT,
    auto_confirm    INTEGER NOT NULL DEFAULT 0,
    steps_json      TEXT NOT NULL DEFAULT '',
    staged_expire_at TEXT,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO orders (
    id, uuid, order_type, status, process_node_id, retrieve_empty, quantity, delivery_node, staging_node, source_node,
    load_type, waybill_id, external_ref, final_count, count_confirmed, eta, auto_confirm, created_at, updated_at,
    steps_json, staged_expire_at
)
SELECT
    id, uuid, order_type, status, op_node_id, retrieve_empty, quantity, delivery_node, staging_node, pickup_node,
    load_type, waybill_id, external_ref, final_count, count_confirmed, eta, auto_confirm, created_at, updated_at,
    COALESCE(steps_json, ''), staged_expire_at
FROM orders_legacy;
DROP TABLE orders_legacy;
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_uuid ON orders(uuid);
CREATE INDEX IF NOT EXISTS idx_orders_process_node_id ON orders(process_node_id);
`)
	return err
}

func (db *DB) rebuildLegacyChangeoverNodeTasks() error {
	// Drop legacy changeover node task tables. Old changeover data is not
	// migrated — the schema has changed fundamentally (claims vs assignments).
	db.Exec("DROP TABLE IF EXISTS changeover_node_tasks_legacy")

	// If the current table has the old column layout, rebuild it
	hasOldCol, _ := schema.TableHasColumn(db.DB, "changeover_node_tasks", "changeover_station_task_id")
	if !hasOldCol {
		hasOldCol, _ = schema.TableHasColumn(db.DB, "changeover_node_tasks", "from_assignment_id")
	}
	if hasOldCol {
		_, err := db.Exec(`
DROP TABLE IF EXISTS changeover_node_tasks;
CREATE TABLE IF NOT EXISTS changeover_node_tasks (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id      INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    process_node_id            INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    from_claim_id              INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    to_claim_id                INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    situation                  TEXT NOT NULL DEFAULT 'unchanged',
    state                      TEXT NOT NULL DEFAULT 'pending',
    next_material_order_id     INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    old_material_release_order_id INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_changeover_id, process_node_id)
);
`)
		return err
	}
	return nil
}

// ── Compatibility wrappers (kept for migration_test.go) ─────────────

// tableHasColumn delegates to schema.TableHasColumn so existing
// migration_test.go call sites compile unchanged. Phase 6.4 may
// migrate the test directly to the schema package and drop this
// wrapper.
func (db *DB) tableHasColumn(tableName, columnName string) (bool, error) {
	return schema.TableHasColumn(db.DB, tableName, columnName)
}

// tableExists delegates to schema.TableExists for the same reason
// as tableHasColumn above.
func (db *DB) tableExists(tableName string) (bool, error) {
	return schema.TableExists(db.DB, tableName)
}
