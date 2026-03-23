package store

const schemaMigrations = `
DROP TABLE IF EXISTS bom_entries;
DROP TABLE IF EXISTS inventory;
DROP TABLE IF EXISTS materials;
DROP TABLE IF EXISTS kanban_templates;
DROP TABLE IF EXISTS operator_screens;
`

const schema = `
CREATE TABLE IF NOT EXISTS admin_users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS processes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    active_style_id     INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    target_style_id     INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    production_state    TEXT NOT NULL DEFAULT 'active_production',
    counter_plc_name    TEXT NOT NULL DEFAULT '',
    counter_tag_name    TEXT NOT NULL DEFAULT '',
    counter_enabled     INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS styles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id  INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_default  INTEGER NOT NULL DEFAULT 0,
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, name)
);

CREATE TABLE IF NOT EXISTS reporting_points (
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

CREATE TABLE IF NOT EXISTS counter_snapshots (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    reporting_point_id INTEGER NOT NULL REFERENCES reporting_points(id),
    count_value        INTEGER NOT NULL,
    delta              INTEGER NOT NULL DEFAULT 0,
    anomaly            TEXT,
    operator_confirmed INTEGER NOT NULL DEFAULT 0,
    recorded_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid            TEXT NOT NULL UNIQUE,
    order_type      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    process_node_id INTEGER REFERENCES process_nodes(id) ON DELETE SET NULL,
    retrieve_empty  INTEGER NOT NULL DEFAULT 1,
    quantity        INTEGER NOT NULL DEFAULT 0,
    delivery_node   TEXT NOT NULL DEFAULT '',
    staging_node    TEXT NOT NULL DEFAULT '',
    pickup_node     TEXT NOT NULL DEFAULT '',
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
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_uuid ON orders(uuid);

CREATE TABLE IF NOT EXISTS order_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id   INTEGER NOT NULL REFERENCES orders(id),
    old_status TEXT NOT NULL,
    new_status TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS outbox (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    topic      TEXT NOT NULL,
    payload    BLOB NOT NULL,
    msg_type   TEXT NOT NULL DEFAULT '',
    retries    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    sent_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox(sent_at) WHERE sent_at IS NULL;

CREATE TABLE IF NOT EXISTS shifts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL DEFAULT '',
    shift_number INTEGER NOT NULL UNIQUE,
    start_time   TEXT NOT NULL,
    end_time     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL,
    style_id     INTEGER NOT NULL,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date, hour)
);

CREATE TABLE IF NOT EXISTS payload_catalog (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    code         TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    uop_capacity INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS operator_stations (
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

CREATE TABLE IF NOT EXISTS process_nodes (
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

CREATE TABLE IF NOT EXISTS process_node_runtime_states (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_node_id    INTEGER NOT NULL UNIQUE REFERENCES process_nodes(id) ON DELETE CASCADE,
    active_claim_id    INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    remaining_uop      INTEGER NOT NULL DEFAULT 0,
    active_order_id    INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    staged_order_id    INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS style_node_claims (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    style_id                INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    core_node_name          TEXT NOT NULL,
    role                    TEXT NOT NULL DEFAULT 'consume',
    swap_mode               TEXT NOT NULL DEFAULT 'simple',
    payload_code            TEXT NOT NULL DEFAULT '',
    uop_capacity            INTEGER NOT NULL DEFAULT 0,
    reorder_point           INTEGER NOT NULL DEFAULT 0,
    auto_reorder            INTEGER NOT NULL DEFAULT 1,
    inbound_staging         TEXT NOT NULL DEFAULT '',
    outbound_staging        TEXT NOT NULL DEFAULT '',
    keep_staged             INTEGER NOT NULL DEFAULT 0,
    evacuate_on_changeover  INTEGER NOT NULL DEFAULT 0,
    sequence                INTEGER NOT NULL DEFAULT 0,
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(style_id, core_node_name)
);

CREATE TABLE IF NOT EXISTS process_changeovers (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id      INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    from_style_id   INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    to_style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    state           TEXT NOT NULL DEFAULT 'planned',
    called_by       TEXT NOT NULL DEFAULT '',
    notes           TEXT NOT NULL DEFAULT '',
    started_at      TEXT NOT NULL DEFAULT (datetime('now')),
    completed_at    TEXT,
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS changeover_station_tasks (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    operator_station_id   INTEGER NOT NULL REFERENCES operator_stations(id) ON DELETE CASCADE,
    state                 TEXT NOT NULL DEFAULT 'waiting',
    updated_at            TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_changeover_id, operator_station_id)
);

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
`

func (db *DB) migrate() error {
	// Run cleanup migrations first (drop old tables)
	if _, err := db.Exec(schemaMigrations); err != nil {
		return err
	}
	db.Exec("ALTER TABLE orders DROP COLUMN material_id")

	// Rename legacy tables
	db.Exec("ALTER TABLE production_lines RENAME TO processes")
	db.Exec("ALTER TABLE job_styles RENAME TO styles")
	db.Exec("ALTER TABLE location_nodes RENAME TO nodes")

	// Create clean schema
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// --- Column renames via graceful migrations ---

	// processes: active_job_style_id → active_style_id, target_job_style_id → target_style_id
	if err := db.migrateProcessColumns(); err != nil {
		return err
	}

	// styles: line_id → process_id, add is_default
	if err := db.migrateStyleColumns(); err != nil {
		return err
	}

	// reporting_points: job_style_id → style_id
	if err := db.migrateReportingPointColumns(); err != nil {
		return err
	}

	// hourly_counts: line_id → process_id, job_style_id → style_id
	if err := db.migrateHourlyCountColumns(); err != nil {
		return err
	}

	// operator_stations: remove expected_client_type, parent_station_id
	if err := db.migrateOperatorStationColumns(); err != nil {
		return err
	}

	// process_nodes: inline operator_station_id (was junction table)
	if err := db.migrateProcessNodeOwnership(); err != nil {
		return err
	}

	// Strip old columns from process_node_runtime_states if they exist
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

	// Style node claims routing columns
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

	// Migrate queued → pending status
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
	if exists, _ := db.tableExists("process_counter_bindings"); exists {
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

	return nil
}

// migrateProcessColumns renames active_job_style_id → active_style_id
func (db *DB) migrateProcessColumns() error {
	has, err := db.tableHasColumn("processes", "active_job_style_id")
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

// migrateStyleColumns renames line_id → process_id, adds is_default
func (db *DB) migrateStyleColumns() error {
	hasLineID, err := db.tableHasColumn("styles", "line_id")
	if err != nil {
		return err
	}
	if !hasLineID {
		// Fresh DB or already migrated — ensure process_id column
		db.Exec("ALTER TABLE styles ADD COLUMN process_id INTEGER REFERENCES processes(id) ON DELETE CASCADE")
		db.Exec("ALTER TABLE styles ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0")
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
    is_default  INTEGER NOT NULL DEFAULT 0,
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, name)
);
INSERT INTO styles (id, process_id, name, description, is_default, active, created_at)
SELECT id, line_id, name, description, 0, COALESCE(active, 1), created_at
FROM styles_legacy;
DROP TABLE styles_legacy;
`)
	return err
}

// migrateReportingPointColumns renames job_style_id → style_id
func (db *DB) migrateReportingPointColumns() error {
	has, err := db.tableHasColumn("reporting_points", "job_style_id")
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
	has, err := db.tableHasColumn("hourly_counts", "line_id")
	if err != nil || !has {
		return err
	}
	_, err = db.Exec(`
ALTER TABLE hourly_counts RENAME TO hourly_counts_legacy;
CREATE TABLE hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL,
    style_id     INTEGER NOT NULL,
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


// migrateOperatorStationColumns removes parent_station_id and expected_client_type
func (db *DB) migrateOperatorStationColumns() error {
	hasParent, err := db.tableHasColumn("operator_stations", "parent_station_id")
	if err != nil {
		return err
	}
	hasExpected, _ := db.tableHasColumn("operator_stations", "expected_client_type")
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

// migrateProcessNodeOwnership migrates from op_station_nodes + junction table to inline operator_station_id
func (db *DB) migrateProcessNodeOwnership() error {
	// Phase 1: migrate from old op_station_nodes table if it exists
	opStationNodesExist, err := db.tableExists("op_station_nodes")
	if err != nil {
		return err
	}
	if opStationNodesExist {
		if err := db.rebuildFromOpStationNodes(); err != nil {
			return err
		}
	}

	// Phase 2: migrate from junction table to inline FK (if junction table exists)
	junctionExists, err := db.tableExists("operator_station_process_nodes")
	if err != nil {
		return err
	}
	if junctionExists {
		hasInlineFK, err := db.tableHasColumn("process_nodes", "operator_station_id")
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
	hasOldCol, _ := db.tableHasColumn("process_node_runtime_states", "effective_style_id")
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
	hasPositionType, _ := db.tableHasColumn("process_nodes", "position_type")
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

	hasCoreNodeName, _ := db.tableHasColumn("op_station_nodes", "core_node_name")
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
		has, err := db.tableHasColumn(name, "op_node_id")
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
	has, err := db.tableHasColumn("orders", "op_node_id")
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
    pickup_node     TEXT NOT NULL DEFAULT '',
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
    id, uuid, order_type, status, process_node_id, retrieve_empty, quantity, delivery_node, staging_node, pickup_node,
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
	hasOldCol, _ := db.tableHasColumn("changeover_node_tasks", "changeover_station_task_id")
	if !hasOldCol {
		hasOldCol, _ = db.tableHasColumn("changeover_node_tasks", "from_assignment_id")
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

func (db *DB) tableHasColumn(tableName, columnName string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('`+tableName+`') WHERE name = ?`, columnName)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

func (db *DB) tableExists(tableName string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tableName)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}
