package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenMigratesLegacyOrdersWithoutProcessNodeID(t *testing.T) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer raw.Close()

	legacySchema := `
CREATE TABLE orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid            TEXT NOT NULL UNIQUE,
    order_type      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
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
CREATE INDEX idx_orders_status ON orders(status);
CREATE INDEX idx_orders_uuid ON orders(uuid);
`
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('orders') WHERE name='process_node_id'`).Scan(&count); err != nil {
		t.Fatalf("check process_node_id column: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected process_node_id column to exist after migration, count=%d", count)
	}

	if _, err := db.Exec(`INSERT INTO orders (uuid, order_type, process_node_id) VALUES ('uuid-test', 'retrieve', NULL)`); err != nil {
		t.Fatalf("insert migrated order with process_node_id column: %v", err)
	}
}

func TestOpenRebuildsOperatorStationsWithoutParentHierarchy(t *testing.T) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "legacy-stations.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer raw.Close()

	legacySchema := `
CREATE TABLE processes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    active_job_style_id INTEGER,
    target_job_style_id INTEGER,
    production_state TEXT NOT NULL DEFAULT 'active_production',
    cutover_mode TEXT NOT NULL DEFAULT 'manual',
    changeover_flow_json TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE operator_stations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id INTEGER NOT NULL,
    parent_station_id INTEGER,
    code TEXT NOT NULL,
    name TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    area_label TEXT NOT NULL DEFAULT '',
    sequence INTEGER NOT NULL DEFAULT 0,
    controller_node_id TEXT NOT NULL DEFAULT '',
    device_mode TEXT NOT NULL DEFAULT 'fixed_hmi',
    expected_client_type TEXT NOT NULL DEFAULT 'touch_hmi',
    enabled INTEGER NOT NULL DEFAULT 1,
    health_status TEXT NOT NULL DEFAULT 'offline',
    last_seen_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);
INSERT INTO processes (id, name) VALUES (1, 'Process A');
INSERT INTO operator_stations (id, process_id, parent_station_id, code, name, note, sequence)
VALUES (1, 1, NULL, 'op-10', 'OP10', 'Legacy note', 7);
`
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("seed legacy station schema: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('operator_stations') WHERE name='parent_station_id'`).Scan(&count); err != nil {
		t.Fatalf("check parent_station_id column: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected parent_station_id column to be removed after migration, count=%d", count)
	}

	station, err := db.GetOperatorStation(1)
	if err != nil {
		t.Fatalf("load migrated station: %v", err)
	}
	if station.Name != "OP10" || station.Note != "Legacy note" || station.Sequence != 7 {
		t.Fatalf("unexpected migrated station: %+v", station)
	}
}

func TestOpenMigratesLegacyProcessNodeTablesIntoNewProcessOwnershipModel(t *testing.T) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "legacy-process-nodes.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer raw.Close()

	legacySchema := `
CREATE TABLE processes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    active_job_style_id INTEGER,
    target_job_style_id INTEGER,
    production_state TEXT NOT NULL DEFAULT 'active_production',
    cutover_mode TEXT NOT NULL DEFAULT 'manual',
    changeover_flow_json TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE styles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    active INTEGER NOT NULL DEFAULT 1,
    line_id INTEGER,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE operator_stations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id INTEGER NOT NULL,
    code TEXT NOT NULL,
    name TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    area_label TEXT NOT NULL DEFAULT '',
    sequence INTEGER NOT NULL DEFAULT 0,
    controller_node_id TEXT NOT NULL DEFAULT '',
    device_mode TEXT NOT NULL DEFAULT 'fixed_hmi',
    expected_client_type TEXT NOT NULL DEFAULT 'touch_hmi',
    enabled INTEGER NOT NULL DEFAULT 1,
    health_status TEXT NOT NULL DEFAULT 'offline',
    last_seen_at TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);
CREATE TABLE op_station_nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    operator_station_id INTEGER NOT NULL,
    code TEXT NOT NULL,
    core_node_name TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    position_type TEXT NOT NULL DEFAULT 'consume',
    sequence INTEGER NOT NULL DEFAULT 0,
    delivery_node TEXT NOT NULL DEFAULT '',
    staging_node TEXT NOT NULL DEFAULT '',
    secondary_staging_node TEXT NOT NULL DEFAULT '',
    staging_node_group TEXT NOT NULL DEFAULT '',
    secondary_node_group TEXT NOT NULL DEFAULT '',
    full_pickup_node TEXT NOT NULL DEFAULT '',
    full_pickup_node_group TEXT NOT NULL DEFAULT '',
    outgoing_node TEXT NOT NULL DEFAULT '',
    outgoing_node_group TEXT NOT NULL DEFAULT '',
    allows_reorder INTEGER NOT NULL DEFAULT 1,
    allows_empty_release INTEGER NOT NULL DEFAULT 1,
    allows_partial_release INTEGER NOT NULL DEFAULT 1,
    allows_manifest_confirm INTEGER NOT NULL DEFAULT 1,
    allows_station_change INTEGER NOT NULL DEFAULT 1,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(operator_station_id, code)
);
CREATE TABLE op_node_style_assignments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    op_node_id INTEGER NOT NULL,
    style_id INTEGER NOT NULL,
    payload_code TEXT NOT NULL DEFAULT '',
    payload_description TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL DEFAULT 'consume',
    uop_capacity INTEGER NOT NULL DEFAULT 0,
    reorder_point INTEGER NOT NULL DEFAULT 0,
    auto_reorder_enabled INTEGER NOT NULL DEFAULT 1,
    cycle_mode TEXT NOT NULL DEFAULT 'simple',
    retrieve_empty INTEGER NOT NULL DEFAULT 0,
    requires_manifest_confirmation INTEGER NOT NULL DEFAULT 0,
    allows_partial_return INTEGER NOT NULL DEFAULT 1,
    changeover_group TEXT NOT NULL DEFAULT '',
    changeover_sequence INTEGER NOT NULL DEFAULT 0,
    changeover_policy TEXT NOT NULL DEFAULT 'manual_station_change',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(op_node_id, style_id)
);
INSERT INTO processes (id, name) VALUES (1, 'Process A');
INSERT INTO styles (id, name, line_id) VALUES (1, 'Style A', 1);
INSERT INTO operator_stations (id, process_id, code, name) VALUES (1, 1, 'op-10', 'OP10');
INSERT INTO op_station_nodes (id, operator_station_id, code, core_node_name, name, delivery_node) VALUES (1, 1, 'node-1', 'CORE-A', 'Node A', 'LINE-A');
INSERT INTO op_node_style_assignments (id, op_node_id, style_id, payload_code, payload_description) VALUES (1, 1, 1, 'P-100', 'Payload 100');
`
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("seed legacy process node schema: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer db.Close()

	var nodeCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_nodes`).Scan(&nodeCount); err != nil {
		t.Fatalf("count process_nodes: %v", err)
	}
	if nodeCount != 1 {
		t.Fatalf("expected one migrated process node, count=%d", nodeCount)
	}

	var assignmentCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_node_style_assignments`).Scan(&assignmentCount); err != nil {
		t.Fatalf("count process_node_style_assignments: %v", err)
	}
	if assignmentCount != 1 {
		t.Fatalf("expected one migrated process node assignment, count=%d", assignmentCount)
	}

	var delegationCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM operator_station_process_nodes`).Scan(&delegationCount); err != nil {
		t.Fatalf("count operator_station_process_nodes: %v", err)
	}
	if delegationCount != 1 {
		t.Fatalf("expected one migrated station delegation, count=%d", delegationCount)
	}
}
