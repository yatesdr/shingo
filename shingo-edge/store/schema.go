package store

const schemaMigrations = `
DROP TABLE IF EXISTS bom_entries;
DROP TABLE IF EXISTS inventory;
DROP TABLE IF EXISTS materials;
DROP TABLE IF EXISTS kanban_templates;
`

const schema = `
CREATE TABLE IF NOT EXISTS admin_users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS job_styles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS payloads (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    job_style_id     INTEGER NOT NULL REFERENCES job_styles(id) ON DELETE CASCADE,
    location         TEXT NOT NULL,
    staging_node     TEXT NOT NULL DEFAULT '',
    description      TEXT NOT NULL DEFAULT '',
    manifest         TEXT NOT NULL DEFAULT '{}',
    multiplier       REAL NOT NULL DEFAULT 1,
    production_units INTEGER NOT NULL DEFAULT 0,
    remaining        INTEGER NOT NULL DEFAULT 0,
    reorder_point    INTEGER NOT NULL DEFAULT 0,
    reorder_qty      INTEGER NOT NULL DEFAULT 1,
    retrieve_empty   INTEGER NOT NULL DEFAULT 1,
    status           TEXT NOT NULL DEFAULT 'active',
    payload_code     TEXT NOT NULL DEFAULT '',
    auto_reorder     INTEGER NOT NULL DEFAULT 1,
    role             TEXT NOT NULL DEFAULT 'consume',
    auto_remove_empties INTEGER NOT NULL DEFAULT 0,
    auto_order_empties  INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at       TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(job_style_id, location)
);

CREATE TABLE IF NOT EXISTS reporting_points (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    plc_name     TEXT NOT NULL,
    tag_name     TEXT NOT NULL,
    job_style_id INTEGER NOT NULL REFERENCES job_styles(id) ON DELETE CASCADE,
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
    payload_id      INTEGER REFERENCES payloads(id),
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

CREATE TABLE IF NOT EXISTS location_nodes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id     TEXT NOT NULL UNIQUE,
    line_id     INTEGER REFERENCES production_lines(id) ON DELETE CASCADE,
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

CREATE TABLE IF NOT EXISTS production_lines (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    active_job_style_id INTEGER REFERENCES job_styles(id) ON DELETE SET NULL,
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
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

CREATE TABLE IF NOT EXISTS operator_screens (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL UNIQUE,
    layout     TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
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
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}
	// Graceful migrations for existing DBs
	db.Exec("ALTER TABLE payloads ADD COLUMN has_description TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE payloads ADD COLUMN blueprint_code TEXT NOT NULL DEFAULT ''")
	// Migrate has_description → drop (SQLite may not support DROP COLUMN on older versions)
	db.Exec("ALTER TABLE payloads DROP COLUMN has_description")
	// Rename style_catalog → blueprint_catalog → payload_catalog and drop form_factor
	db.Exec("ALTER TABLE style_catalog RENAME TO blueprint_catalog")
	db.Exec("ALTER TABLE blueprint_catalog DROP COLUMN form_factor")
	db.Exec("ALTER TABLE blueprint_catalog RENAME TO payload_catalog")
	// Rename blueprint_code → payload_code on payloads table
	db.Exec("ALTER TABLE payloads RENAME COLUMN blueprint_code TO payload_code")
	db.Exec("ALTER TABLE payloads ADD COLUMN auto_reorder INTEGER NOT NULL DEFAULT 1")
	db.Exec("ALTER TABLE location_nodes RENAME COLUMN node_type TO process")

	// Production lines migrations
	db.Exec("ALTER TABLE job_styles ADD COLUMN line_id INTEGER REFERENCES production_lines(id) ON DELETE CASCADE")
	db.Exec("ALTER TABLE reporting_points ADD COLUMN line_id INTEGER REFERENCES production_lines(id) ON DELETE CASCADE")
	db.Exec("ALTER TABLE changeover_log ADD COLUMN line_id INTEGER")
	db.Exec("ALTER TABLE job_styles ADD COLUMN cat_id TEXT NOT NULL DEFAULT ''")

	// Auto-create default line if job_styles exist but no lines do
	var lineCount int
	db.QueryRow("SELECT COUNT(*) FROM production_lines").Scan(&lineCount)
	if lineCount == 0 {
		var jsCount int
		db.QueryRow("SELECT COUNT(*) FROM job_styles").Scan(&jsCount)
		if jsCount > 0 {
			db.Exec("INSERT INTO production_lines (name, description) VALUES ('Line 1', 'Default production line')")
			// Assign orphaned job styles to the default line
			db.Exec("UPDATE job_styles SET line_id = (SELECT id FROM production_lines WHERE name = 'Line 1') WHERE line_id IS NULL")
			// Assign orphaned reporting points to the default line
			db.Exec("UPDATE reporting_points SET line_id = (SELECT id FROM production_lines WHERE name = 'Line 1') WHERE line_id IS NULL")
		}
	}

	// WarLink tag management tracking
	db.Exec("ALTER TABLE reporting_points ADD COLUMN warlink_managed INTEGER NOT NULL DEFAULT 0")

	// Location nodes: migrate process text → line_id FK
	db.Exec("ALTER TABLE location_nodes ADD COLUMN line_id INTEGER REFERENCES production_lines(id) ON DELETE CASCADE")
	db.Exec("UPDATE location_nodes SET line_id = (SELECT id FROM production_lines WHERE name = location_nodes.process) WHERE line_id IS NULL AND process != ''")

	// Migrate queued -> pending status
	db.Exec("UPDATE orders SET status='pending' WHERE status='queued'")

	// Complex order steps
	db.Exec("ALTER TABLE orders ADD COLUMN steps_json TEXT NOT NULL DEFAULT ''")

	// Payload role + automation flags for produce/consume workflows
	db.Exec("ALTER TABLE payloads ADD COLUMN role TEXT NOT NULL DEFAULT 'consume'")
	db.Exec("ALTER TABLE payloads ADD COLUMN auto_remove_empties INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE payloads ADD COLUMN auto_order_empties INTEGER NOT NULL DEFAULT 0")

	// Staged bin expiry visibility
	db.Exec("ALTER TABLE orders ADD COLUMN staged_expire_at TEXT")

	return nil
}
