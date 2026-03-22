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

CREATE TABLE IF NOT EXISTS styles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS reporting_points (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    plc_name     TEXT NOT NULL,
    tag_name     TEXT NOT NULL,
    job_style_id INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    last_count   INTEGER NOT NULL DEFAULT 0,
    last_poll_at TEXT,
    enabled      INTEGER NOT NULL DEFAULT 1,
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

CREATE TABLE IF NOT EXISTS nodes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id     TEXT NOT NULL UNIQUE,
    line_id     INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS changeover_log (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    from_job_style TEXT NOT NULL DEFAULT '',
    to_job_style   TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL,
    detail         TEXT NOT NULL DEFAULT '',
    operator       TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS processes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    active_job_style_id INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    target_job_style_id INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    production_state    TEXT NOT NULL DEFAULT 'active_production',
    cutover_mode        TEXT NOT NULL DEFAULT 'manual',
    changeover_flow_json TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS process_counter_bindings (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id        INTEGER NOT NULL UNIQUE REFERENCES processes(id) ON DELETE CASCADE,
    reporting_point_id INTEGER REFERENCES reporting_points(id) ON DELETE SET NULL,
    plc_name          TEXT NOT NULL DEFAULT '',
    tag_name          TEXT NOT NULL DEFAULT '',
    enabled           INTEGER NOT NULL DEFAULT 1,
    warlink_managed   INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS shifts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL DEFAULT '',
    shift_number INTEGER NOT NULL UNIQUE,
    start_time   TEXT NOT NULL,
    end_time     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    line_id      INTEGER NOT NULL,
    job_style_id INTEGER NOT NULL,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(line_id, job_style_id, count_date, hour)
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
    device_mode        TEXT NOT NULL DEFAULT 'fixed_hmi',
    expected_client_type TEXT NOT NULL DEFAULT 'touch_hmi',
    enabled            INTEGER NOT NULL DEFAULT 1,
    health_status      TEXT NOT NULL DEFAULT 'offline',
    last_seen_at       TEXT,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);

CREATE TABLE IF NOT EXISTS process_nodes (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id               INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    code                     TEXT NOT NULL,
    core_node_name           TEXT NOT NULL DEFAULT '',
    name                     TEXT NOT NULL,
    position_type            TEXT NOT NULL DEFAULT 'consume',
    sequence                 INTEGER NOT NULL DEFAULT 0,
    delivery_node            TEXT NOT NULL DEFAULT '',
    staging_node             TEXT NOT NULL DEFAULT '',
    secondary_staging_node   TEXT NOT NULL DEFAULT '',
    staging_node_group       TEXT NOT NULL DEFAULT '',
    secondary_node_group     TEXT NOT NULL DEFAULT '',
    full_pickup_node         TEXT NOT NULL DEFAULT '',
    full_pickup_node_group   TEXT NOT NULL DEFAULT '',
    outgoing_node            TEXT NOT NULL DEFAULT '',
    outgoing_node_group      TEXT NOT NULL DEFAULT '',
    allows_reorder           INTEGER NOT NULL DEFAULT 1,
    allows_empty_release     INTEGER NOT NULL DEFAULT 1,
    allows_partial_release   INTEGER NOT NULL DEFAULT 1,
    allows_manifest_confirm  INTEGER NOT NULL DEFAULT 1,
    allows_station_change    INTEGER NOT NULL DEFAULT 1,
    enabled                  INTEGER NOT NULL DEFAULT 1,
    created_at               TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at               TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);

CREATE TABLE IF NOT EXISTS operator_station_process_nodes (
    operator_station_id      INTEGER NOT NULL REFERENCES operator_stations(id) ON DELETE CASCADE,
    process_node_id          INTEGER NOT NULL UNIQUE REFERENCES process_nodes(id) ON DELETE CASCADE,
    PRIMARY KEY (operator_station_id, process_node_id)
);

CREATE TABLE IF NOT EXISTS process_node_style_assignments (
    id                           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_node_id              INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    style_id                     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    payload_code                 TEXT NOT NULL DEFAULT '',
    payload_description          TEXT NOT NULL DEFAULT '',
    role                         TEXT NOT NULL DEFAULT 'consume',
    uop_capacity                 INTEGER NOT NULL DEFAULT 0,
    reorder_point                INTEGER NOT NULL DEFAULT 0,
    auto_reorder_enabled         INTEGER NOT NULL DEFAULT 1,
    cycle_mode                   TEXT NOT NULL DEFAULT 'simple',
    retrieve_empty               INTEGER NOT NULL DEFAULT 0,
    requires_manifest_confirmation INTEGER NOT NULL DEFAULT 0,
    allows_partial_return        INTEGER NOT NULL DEFAULT 1,
    changeover_group             TEXT NOT NULL DEFAULT '',
    changeover_sequence          INTEGER NOT NULL DEFAULT 0,
    changeover_policy            TEXT NOT NULL DEFAULT 'manual_station_change',
    created_at                   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at                   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_node_id, style_id)
);

CREATE TABLE IF NOT EXISTS process_node_runtime_states (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    process_node_id            INTEGER NOT NULL UNIQUE REFERENCES process_nodes(id) ON DELETE CASCADE,
    effective_style_id         INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    active_assignment_id       INTEGER REFERENCES process_node_style_assignments(id) ON DELETE SET NULL,
    staged_assignment_id       INTEGER REFERENCES process_node_style_assignments(id) ON DELETE SET NULL,
    loaded_payload_code        TEXT NOT NULL DEFAULT '',
    material_status            TEXT NOT NULL DEFAULT 'empty',
    remaining_uop              INTEGER NOT NULL DEFAULT 0,
    manifest_status            TEXT NOT NULL DEFAULT 'unknown',
    active_order_id            INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    staged_order_id            INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    loaded_bin_label           TEXT NOT NULL DEFAULT '',
    loaded_at                  TEXT,
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS process_changeovers (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id      INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    from_style_id   INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    to_style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    state           TEXT NOT NULL DEFAULT 'planned',
    phase           TEXT NOT NULL DEFAULT 'runout',
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
    current_phase         TEXT NOT NULL DEFAULT 'runout',
    transition_mode       TEXT NOT NULL DEFAULT 'rolling_local',
    ready_for_local_change INTEGER NOT NULL DEFAULT 0,
    switched_at           TEXT,
    verified_at           TEXT,
    blocked_reason        TEXT NOT NULL DEFAULT '',
    updated_at            TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_changeover_id, operator_station_id)
);

CREATE TABLE IF NOT EXISTS changeover_node_tasks (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id      INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    operator_station_id        INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL,
    process_node_id            INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    from_assignment_id         INTEGER REFERENCES process_node_style_assignments(id) ON DELETE SET NULL,
    to_assignment_id           INTEGER REFERENCES process_node_style_assignments(id) ON DELETE SET NULL,
    state                      TEXT NOT NULL DEFAULT 'unchanged',
    old_material_release_required INTEGER NOT NULL DEFAULT 0,
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
	// Also drop the old material_id column from orders if it exists
	// SQLite doesn't support DROP COLUMN before 3.35, so we handle this gracefully
	db.Exec("ALTER TABLE orders DROP COLUMN material_id")

	// Rename tables from old names to new names (safe for existing DBs)
	db.Exec("ALTER TABLE production_lines RENAME TO processes")
	db.Exec("ALTER TABLE job_styles RENAME TO styles")
	db.Exec("ALTER TABLE location_nodes RENAME TO nodes")

	_, err := db.Exec(schema)
	if err != nil {
		return err
	}
	// Graceful migrations for existing DBs
	// Rename style_catalog → blueprint_catalog → payload_catalog and drop form_factor
	db.Exec("ALTER TABLE style_catalog RENAME TO blueprint_catalog")
	db.Exec("ALTER TABLE blueprint_catalog DROP COLUMN form_factor")
	db.Exec("ALTER TABLE blueprint_catalog RENAME TO payload_catalog")
	db.Exec("ALTER TABLE nodes RENAME COLUMN node_type TO process")

	// Production lines migrations
	db.Exec("ALTER TABLE styles ADD COLUMN line_id INTEGER REFERENCES processes(id) ON DELETE CASCADE")
	db.Exec("ALTER TABLE reporting_points ADD COLUMN line_id INTEGER REFERENCES processes(id) ON DELETE CASCADE")
	db.Exec("ALTER TABLE changeover_log ADD COLUMN line_id INTEGER")
	db.Exec("ALTER TABLE styles ADD COLUMN cat_id TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE processes ADD COLUMN target_job_style_id INTEGER REFERENCES styles(id) ON DELETE SET NULL")
	db.Exec("ALTER TABLE processes ADD COLUMN production_state TEXT NOT NULL DEFAULT 'active_production'")
	db.Exec("ALTER TABLE processes ADD COLUMN cutover_mode TEXT NOT NULL DEFAULT 'manual'")
	db.Exec("ALTER TABLE processes ADD COLUMN changeover_flow_json TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE operator_stations ADD COLUMN note TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE operator_stations ADD COLUMN controller_node_id TEXT NOT NULL DEFAULT ''")
	if err := db.rebuildOperatorStationsWithoutParent(); err != nil {
		return err
	}
	db.Exec("ALTER TABLE op_station_nodes ADD COLUMN core_node_name TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE process_changeovers ADD COLUMN phase TEXT NOT NULL DEFAULT 'runout'")
	db.Exec("ALTER TABLE changeover_station_tasks ADD COLUMN current_phase TEXT NOT NULL DEFAULT 'runout'")
	db.Exec(`CREATE TABLE IF NOT EXISTS process_counter_bindings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		process_id INTEGER NOT NULL UNIQUE REFERENCES processes(id) ON DELETE CASCADE,
		reporting_point_id INTEGER REFERENCES reporting_points(id) ON DELETE SET NULL,
		plc_name TEXT NOT NULL DEFAULT '',
		tag_name TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		warlink_managed INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`)

	// Auto-create default line if styles exist but no processes do
	var lineCount int
	db.QueryRow("SELECT COUNT(*) FROM processes").Scan(&lineCount)
	if lineCount == 0 {
		var jsCount int
		db.QueryRow("SELECT COUNT(*) FROM styles").Scan(&jsCount)
		if jsCount > 0 {
			db.Exec("INSERT INTO processes (name, description) VALUES ('Line 1', 'Default production line')")
			// Assign orphaned styles to the default process
			db.Exec("UPDATE styles SET line_id = (SELECT id FROM processes WHERE name = 'Line 1') WHERE line_id IS NULL")
			// Assign orphaned reporting points to the default process
			db.Exec("UPDATE reporting_points SET line_id = (SELECT id FROM processes WHERE name = 'Line 1') WHERE line_id IS NULL")
		}
	}

	// WarLink tag management tracking
	db.Exec("ALTER TABLE reporting_points ADD COLUMN warlink_managed INTEGER NOT NULL DEFAULT 0")

	// Location nodes: migrate process text → line_id FK
	db.Exec("ALTER TABLE nodes ADD COLUMN line_id INTEGER REFERENCES processes(id) ON DELETE CASCADE")
	db.Exec("UPDATE nodes SET line_id = (SELECT id FROM processes WHERE name = nodes.process) WHERE line_id IS NULL AND process != ''")

	// Migrate queued -> pending status
	db.Exec("UPDATE orders SET status='pending' WHERE status='queued'")

	// Complex order steps
	db.Exec("ALTER TABLE orders ADD COLUMN steps_json TEXT NOT NULL DEFAULT ''")

	// Staged bin expiry visibility
	db.Exec("ALTER TABLE orders ADD COLUMN staged_expire_at TEXT")
	db.Exec("ALTER TABLE orders ADD COLUMN process_node_id INTEGER REFERENCES process_nodes(id) ON DELETE SET NULL")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_process_node_id ON orders(process_node_id)")
	if err := db.migrateProcessNodeOwnership(); err != nil {
		return err
	}

	return nil
}

func (db *DB) migrateProcessNodeOwnership() error {
	exists, err := db.tableExists("op_station_nodes")
	if err != nil || !exists {
		return err
	}
	if err := db.rebuildProcessNodesFromStationNodes(); err != nil {
		return err
	}
	if err := db.rebuildProcessNodeAssignments(); err != nil {
		return err
	}
	if err := db.rebuildProcessNodeRuntimeStates(); err != nil {
		return err
	}
	if err := db.rebuildOrdersWithProcessNodes(); err != nil {
		return err
	}
	if err := db.rebuildChangeoverNodeTasks(); err != nil {
		return err
	}
	_, err = db.Exec(`DROP TABLE IF EXISTS op_station_nodes`)
	return err
}

func (db *DB) rebuildOperatorStationsWithoutParent() error {
	hasParent, err := db.tableHasColumn("operator_stations", "parent_station_id")
	if err != nil || !hasParent {
		return err
	}
	_, err = db.Exec(`
ALTER TABLE operator_stations RENAME TO operator_stations_legacy_parent;
CREATE TABLE operator_stations (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id         INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    code               TEXT NOT NULL,
    name               TEXT NOT NULL,
    note               TEXT NOT NULL DEFAULT '',
    area_label         TEXT NOT NULL DEFAULT '',
    sequence           INTEGER NOT NULL DEFAULT 0,
    controller_node_id TEXT NOT NULL DEFAULT '',
    device_mode        TEXT NOT NULL DEFAULT 'fixed_hmi',
    expected_client_type TEXT NOT NULL DEFAULT 'touch_hmi',
    enabled            INTEGER NOT NULL DEFAULT 1,
    health_status      TEXT NOT NULL DEFAULT 'offline',
    last_seen_at       TEXT,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);
INSERT INTO operator_stations (
    id, process_id, code, name, note, area_label, sequence, controller_node_id,
    device_mode, expected_client_type, enabled, health_status, last_seen_at, created_at, updated_at
)
SELECT
    id, process_id, code, name, COALESCE(note, ''), COALESCE(area_label, ''), COALESCE(sequence, 0), COALESCE(controller_node_id, ''),
    COALESCE(device_mode, 'fixed_hmi'), COALESCE(expected_client_type, 'touch_hmi'),
    COALESCE(enabled, 1), COALESCE(health_status, 'offline'), last_seen_at, created_at, updated_at
FROM operator_stations_legacy_parent;
DROP TABLE operator_stations_legacy_parent;
`)
	return err
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

func (db *DB) rebuildProcessNodesFromStationNodes() error {
	hasLegacyProcessNodes, err := db.tableHasColumn("process_nodes", "process_id")
	if err != nil || !hasLegacyProcessNodes {
		return err
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_nodes`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		_, err = db.Exec(`
INSERT OR IGNORE INTO operator_station_process_nodes (operator_station_id, process_node_id)
SELECT operator_station_id, id FROM op_station_nodes;
`)
		return err
	}
	_, err = db.Exec(`
INSERT INTO process_nodes (
    id, process_id, code, core_node_name, name, position_type, sequence,
    delivery_node, staging_node, secondary_staging_node, staging_node_group,
    secondary_node_group, full_pickup_node, full_pickup_node_group, outgoing_node,
    outgoing_node_group, allows_reorder, allows_empty_release, allows_partial_release,
    allows_manifest_confirm, allows_station_change, enabled, created_at, updated_at
)
SELECT
    n.id, s.process_id, n.code, COALESCE(n.core_node_name, ''), n.name, n.position_type, COALESCE(n.sequence, 0),
    COALESCE(n.delivery_node, ''), COALESCE(n.staging_node, ''), COALESCE(n.secondary_staging_node, ''), COALESCE(n.staging_node_group, ''),
    COALESCE(n.secondary_node_group, ''), COALESCE(n.full_pickup_node, ''), COALESCE(n.full_pickup_node_group, ''), COALESCE(n.outgoing_node, ''),
    COALESCE(n.outgoing_node_group, ''), COALESCE(n.allows_reorder, 1), COALESCE(n.allows_empty_release, 1), COALESCE(n.allows_partial_release, 1),
    COALESCE(n.allows_manifest_confirm, 1), COALESCE(n.allows_station_change, 1), COALESCE(n.enabled, 1), n.created_at, n.updated_at
FROM op_station_nodes n
JOIN operator_stations s ON s.id = n.operator_station_id;

INSERT OR IGNORE INTO operator_station_process_nodes (operator_station_id, process_node_id)
SELECT operator_station_id, id FROM op_station_nodes;
`)
	return err
}

func (db *DB) rebuildProcessNodeAssignments() error {
	sourceTable := ""
	legacyExists, err := db.tableExists("op_node_style_assignments_legacy")
	if err != nil {
		return err
	}
	if legacyExists {
		sourceTable = "op_node_style_assignments_legacy"
	} else {
		hasOldColumn, err := db.tableHasColumn("op_node_style_assignments", "op_node_id")
		if err != nil || !hasOldColumn {
			return err
		}
		if _, err := db.Exec(`ALTER TABLE op_node_style_assignments RENAME TO op_node_style_assignments_legacy`); err != nil {
			return err
		}
		sourceTable = "op_node_style_assignments_legacy"
	}
	_, err = db.Exec(`INSERT OR IGNORE INTO process_node_style_assignments (
    id, process_node_id, style_id, payload_code, payload_description, role, uop_capacity, reorder_point,
    auto_reorder_enabled, cycle_mode, retrieve_empty, requires_manifest_confirmation,
    allows_partial_return, changeover_group, changeover_sequence, changeover_policy, created_at, updated_at
)
SELECT
    id, op_node_id, style_id, payload_code, payload_description, role, uop_capacity, reorder_point,
    auto_reorder_enabled, cycle_mode, retrieve_empty, requires_manifest_confirmation,
    allows_partial_return, changeover_group, changeover_sequence, changeover_policy, created_at, updated_at
FROM ` + sourceTable)
	if err != nil {
		return err
	}
	_, err = db.Exec(`DROP TABLE IF EXISTS ` + sourceTable)
	return err
}

func (db *DB) rebuildProcessNodeRuntimeStates() error {
	sourceTable := ""
	legacyExists, err := db.tableExists("op_node_runtime_states_legacy")
	if err != nil {
		return err
	}
	if legacyExists {
		sourceTable = "op_node_runtime_states_legacy"
	} else {
		hasOldColumn, err := db.tableHasColumn("op_node_runtime_states", "op_node_id")
		if err != nil || !hasOldColumn {
			return err
		}
		if _, err := db.Exec(`ALTER TABLE op_node_runtime_states RENAME TO op_node_runtime_states_legacy`); err != nil {
			return err
		}
		sourceTable = "op_node_runtime_states_legacy"
	}
	_, err = db.Exec(`INSERT OR IGNORE INTO process_node_runtime_states (
    id, process_node_id, effective_style_id, active_assignment_id, staged_assignment_id,
    loaded_payload_code, material_status, remaining_uop, manifest_status,
    active_order_id, staged_order_id, loaded_bin_label, loaded_at, updated_at
)
SELECT
    id, op_node_id, effective_style_id, active_assignment_id, staged_assignment_id,
    loaded_payload_code, material_status, remaining_uop, manifest_status,
    active_order_id, staged_order_id, loaded_bin_label, loaded_at, updated_at
FROM ` + sourceTable)
	if err != nil {
		return err
	}
	_, err = db.Exec(`DROP TABLE IF EXISTS ` + sourceTable)
	return err
}

func (db *DB) rebuildOrdersWithProcessNodes() error {
	hasOldColumn, err := db.tableHasColumn("orders", "op_node_id")
	if err != nil || !hasOldColumn {
		return err
	}
	_, err = db.Exec(`
ALTER TABLE orders RENAME TO orders_legacy_process_nodes;
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
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
    steps_json      TEXT NOT NULL DEFAULT '',
    staged_expire_at TEXT
);
INSERT INTO orders (
    id, uuid, order_type, status, process_node_id, retrieve_empty, quantity, delivery_node, staging_node, pickup_node,
    load_type, waybill_id, external_ref, final_count, count_confirmed, eta, auto_confirm, created_at, updated_at, steps_json, staged_expire_at
)
SELECT
    id, uuid, order_type, status, op_node_id, retrieve_empty, quantity, delivery_node, staging_node, pickup_node,
    load_type, waybill_id, external_ref, final_count, count_confirmed, eta, auto_confirm, created_at, updated_at,
    COALESCE(steps_json, ''), staged_expire_at
FROM orders_legacy_process_nodes;
DROP TABLE orders_legacy_process_nodes;
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_uuid ON orders(uuid);
CREATE INDEX IF NOT EXISTS idx_orders_process_node_id ON orders(process_node_id);
`)
	return err
}

func (db *DB) rebuildChangeoverNodeTasks() error {
	sourceTable := ""
	legacyExists, err := db.tableExists("changeover_node_tasks_legacy")
	if err != nil {
		return err
	}
	if legacyExists {
		sourceTable = "changeover_node_tasks_legacy"
	} else {
		hasOldColumn, err := db.tableHasColumn("changeover_node_tasks", "op_node_id")
		if err != nil || !hasOldColumn {
			return err
		}
		if _, err := db.Exec(`ALTER TABLE changeover_node_tasks RENAME TO changeover_node_tasks_legacy`); err != nil {
			return err
		}
		sourceTable = "changeover_node_tasks_legacy"
	}
	targetExists, err := db.tableHasColumn("changeover_node_tasks", "process_node_id")
	if err != nil {
		return err
	}
	if !targetExists {
		if _, err := db.Exec(`
CREATE TABLE changeover_node_tasks (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id      INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    operator_station_id        INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL,
    process_node_id            INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    from_assignment_id         INTEGER REFERENCES process_node_style_assignments(id) ON DELETE SET NULL,
    to_assignment_id           INTEGER REFERENCES process_node_style_assignments(id) ON DELETE SET NULL,
    state                      TEXT NOT NULL DEFAULT 'unchanged',
    old_material_release_required INTEGER NOT NULL DEFAULT 0,
    next_material_order_id     INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    old_material_release_order_id INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_changeover_id, process_node_id)
);`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`INSERT OR IGNORE INTO changeover_node_tasks (
    id, process_changeover_id, operator_station_id, process_node_id, from_assignment_id, to_assignment_id, state,
    old_material_release_required, next_material_order_id, old_material_release_order_id, updated_at
)
SELECT
    t.id, st.process_changeover_id, st.operator_station_id, t.op_node_id, t.from_assignment_id, t.to_assignment_id, t.state,
    t.old_material_release_required, t.next_material_order_id, t.old_material_release_order_id, t.updated_at
FROM ` + sourceTable + ` t
JOIN changeover_station_tasks st ON st.id = t.changeover_station_task_id`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`DROP TABLE IF EXISTS ` + sourceTable)
	return err
}
