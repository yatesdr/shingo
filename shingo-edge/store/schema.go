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
    op_node_id      INTEGER REFERENCES op_station_nodes(id) ON DELETE SET NULL,
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
    parent_station_id  INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL,
    code               TEXT NOT NULL,
    name               TEXT NOT NULL,
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

CREATE TABLE IF NOT EXISTS op_station_nodes (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    operator_station_id      INTEGER NOT NULL REFERENCES operator_stations(id) ON DELETE CASCADE,
    code                     TEXT NOT NULL,
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
    UNIQUE(operator_station_id, code)
);

CREATE TABLE IF NOT EXISTS op_node_style_assignments (
    id                           INTEGER PRIMARY KEY AUTOINCREMENT,
    op_node_id                   INTEGER NOT NULL REFERENCES op_station_nodes(id) ON DELETE CASCADE,
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
    UNIQUE(op_node_id, style_id)
);

CREATE TABLE IF NOT EXISTS op_node_runtime_states (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    op_node_id                 INTEGER NOT NULL UNIQUE REFERENCES op_station_nodes(id) ON DELETE CASCADE,
    effective_style_id         INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    active_assignment_id       INTEGER REFERENCES op_node_style_assignments(id) ON DELETE SET NULL,
    staged_assignment_id       INTEGER REFERENCES op_node_style_assignments(id) ON DELETE SET NULL,
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
    changeover_station_task_id INTEGER NOT NULL REFERENCES changeover_station_tasks(id) ON DELETE CASCADE,
    op_node_id                 INTEGER NOT NULL REFERENCES op_station_nodes(id) ON DELETE CASCADE,
    from_assignment_id         INTEGER REFERENCES op_node_style_assignments(id) ON DELETE SET NULL,
    to_assignment_id           INTEGER REFERENCES op_node_style_assignments(id) ON DELETE SET NULL,
    state                      TEXT NOT NULL DEFAULT 'unchanged',
    old_material_release_required INTEGER NOT NULL DEFAULT 0,
    next_material_order_id     INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    old_material_release_order_id INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(changeover_station_task_id, op_node_id)
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
	db.Exec("ALTER TABLE operator_stations ADD COLUMN parent_station_id INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL")
	db.Exec("ALTER TABLE operator_stations ADD COLUMN controller_node_id TEXT NOT NULL DEFAULT ''")
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
	db.Exec("ALTER TABLE orders ADD COLUMN op_node_id INTEGER REFERENCES op_station_nodes(id) ON DELETE SET NULL")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_orders_op_node_id ON orders(op_node_id)")

	return nil
}
