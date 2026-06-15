package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"shingocore/plantspec"
	"shingocore/store/nodes"
)

// edgeDDL mirrors the shingo-edge SQLite schema (the subset seedEdge writes), so
// this test validates the raw INSERTs' column names without importing the edge
// module. The authoritative schema lives in shingo-edge/store/schema; the
// houseserver runtime (make dev-seed) is the final check against the real DB.
const edgeDDL = `
CREATE TABLE processes (
  id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '', active_style_id INTEGER, target_style_id INTEGER,
  production_state TEXT NOT NULL DEFAULT 'active_production',
  counter_plc_name TEXT NOT NULL DEFAULT '', counter_tag_name TEXT NOT NULL DEFAULT '',
  counter_enabled INTEGER NOT NULL DEFAULT 0, auto_cutover_enabled INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE styles (
  id INTEGER PRIMARY KEY AUTOINCREMENT, process_id INTEGER REFERENCES processes(id) ON DELETE CASCADE,
  name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT (datetime('now')), UNIQUE(process_id, name));
CREATE TABLE operator_stations (
  id INTEGER PRIMARY KEY AUTOINCREMENT, process_id INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
  code TEXT NOT NULL, name TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', area_label TEXT NOT NULL DEFAULT '',
  sequence INTEGER NOT NULL DEFAULT 0, controller_node_id TEXT NOT NULL DEFAULT '',
  device_mode TEXT NOT NULL DEFAULT 'touch_hmi', enabled INTEGER NOT NULL DEFAULT 1,
  health_status TEXT NOT NULL DEFAULT 'offline', last_seen_at TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')), updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(process_id, code));
CREATE TABLE process_nodes (
  id INTEGER PRIMARY KEY AUTOINCREMENT, process_id INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
  operator_station_id INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL,
  core_node_name TEXT NOT NULL DEFAULT '', code TEXT NOT NULL, name TEXT NOT NULL,
  sequence INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT (datetime('now')), updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(process_id, code));
CREATE TABLE style_node_claims (
  id INTEGER PRIMARY KEY AUTOINCREMENT, style_id INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
  core_node_name TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'consume', swap_mode TEXT NOT NULL DEFAULT 'simple',
  payload_code TEXT NOT NULL DEFAULT '', uop_capacity INTEGER NOT NULL DEFAULT 0,
  reorder_point INTEGER NOT NULL DEFAULT 0, auto_reorder INTEGER NOT NULL DEFAULT 1,
  inbound_staging TEXT NOT NULL DEFAULT '', outbound_staging TEXT NOT NULL DEFAULT '',
  inbound_source TEXT NOT NULL DEFAULT '', outbound_destination TEXT NOT NULL DEFAULT '',
  allowed_payload_codes TEXT NOT NULL DEFAULT '', auto_request_payload TEXT NOT NULL DEFAULT '',
  keep_staged INTEGER NOT NULL DEFAULT 0, evacuate_on_changeover INTEGER NOT NULL DEFAULT 0,
  paired_core_node TEXT NOT NULL DEFAULT '', auto_confirm INTEGER NOT NULL DEFAULT 0,
  sequence INTEGER NOT NULL DEFAULT 0, lineside_soft_threshold INTEGER NOT NULL DEFAULT 0,
  reuse_compatible_bins INTEGER NOT NULL DEFAULT 0, auto_push INTEGER NOT NULL DEFAULT 0,
  reorder_point_source TEXT NOT NULL DEFAULT 'legacy', created_at TEXT NOT NULL DEFAULT (datetime('now')),
  UNIQUE(style_id, core_node_name));
CREATE TABLE reporting_points (
  id INTEGER PRIMARY KEY AUTOINCREMENT, style_id INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
  plc_name TEXT NOT NULL, tag_name TEXT NOT NULL, last_count INTEGER NOT NULL DEFAULT 0,
  last_poll_at TEXT, enabled INTEGER NOT NULL DEFAULT 1, warlink_managed INTEGER NOT NULL DEFAULT 0,
  UNIQUE(plc_name, tag_name));
CREATE TABLE payload_catalog (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, code TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '', uop_capacity INTEGER NOT NULL DEFAULT 0,
  cycle_seconds REAL NOT NULL DEFAULT 0, updated_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE process_node_runtime_states (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  process_node_id INTEGER NOT NULL UNIQUE REFERENCES process_nodes(id) ON DELETE CASCADE,
  active_claim_id INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
  active_bin_id INTEGER, active_bin_epoch INTEGER NOT NULL DEFAULT 0,
  remaining_uop_cached INTEGER NOT NULL DEFAULT 0, pending_uop_delta INTEGER NOT NULL DEFAULT 0,
  active_order_id INTEGER, staged_order_id INTEGER, active_pull INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL DEFAULT (datetime('now')));
`

func openSeededEdge(t *testing.T) (*sql.DB, string, *plantspec.Plant) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "edge.db")
	db, err := sql.Open("sqlite", path+"?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open edge sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(edgeDDL); err != nil {
		t.Fatalf("apply edge DDL: %v", err)
	}
	plant, err := plantspec.Load("../../../plants/demo.yaml")
	if err != nil {
		t.Fatalf("load demo plant: %v", err)
	}
	if err := plant.Validate(); err != nil {
		t.Fatalf("validate demo plant: %v", err)
	}
	if err := seedEdgeDB(db, plant, fakeBinIDs(plant)); err != nil {
		t.Fatalf("seedEdgeDB: %v", err)
	}
	return db, path, plant
}

// fakeBinIDs assigns synthetic core bin ids per at-node slot (seedCore would
// supply real ones) so the runtime-state seeding has bins to bind.
func fakeBinIDs(p *plantspec.Plant) map[string]int64 {
	m := make(map[string]int64)
	for i, b := range p.Bins {
		m[b.Slot] = int64(i + 1)
	}
	return m
}

// Asserts the dev-seed WIRING for the demo plant — swap-mode claims, the
// multi-window loader (PLK_LOADER synthetic identity + its three windows on
// per-window operator stations), and runtime-state binding. Plant-size row totals
// (processes / styles / payloads / …) are deliberately NOT pinned: the dev plant
// grows often, so a raw COUNT(*) pin is pure churn with no real signal.

func TestSeedEdge_DemoPlant(t *testing.T) {
	db, _, _ := openSeededEdge(t)

	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	// Composite cell: WELD-1 (two_robot) has 2 claims (1 consume + 1 produce) under one style.
	if n := count(`SELECT COUNT(*) FROM style_node_claims WHERE core_node_name IN ('ALN_001','ALN_002')`); n != 2 {
		t.Fatalf("WELD-1 claims: want 2, got %d", n)
	}
	// press_index: PRESS-1 has ONE claim on the front node (paired PLN_002 carries no own claim).
	if n := count(`SELECT COUNT(*) FROM style_node_claims WHERE core_node_name IN ('PLN_001','PLN_002')`); n != 1 {
		t.Fatalf("PRESS-1 claims: want 1, got %d", n)
	}
	// A/B (sequential): PRESS-2 has 2 paired claims; the parked side carries active_pull=0.
	if n := count(`SELECT COUNT(*) FROM style_node_claims WHERE core_node_name IN ('PLN_003','PLN_004')`); n != 2 {
		t.Fatalf("PRESS-2 A/B claims: want 2, got %d", n)
	}
	// active_pull is a runtime-state column (not on the claim); the parked A/B side seeds active_pull=0.
	if n := count(`SELECT COUNT(*) FROM process_node_runtime_states r JOIN process_nodes pn ON r.process_node_id=pn.id WHERE pn.core_node_name='PLN_004' AND r.active_pull=0`); n != 1 {
		t.Fatalf("PRESS-2 parked side active_pull=0: want 1, got %d", n)
	}
	// A manual_swap loader-WINDOW claim round-trips correctly (PLK_W1 is one window
	// of the multi-window PLK_LOADER; the loader has no anchor node — its windows
	// carry the claim config).
	var role, swap, payload, outDst string
	var cap, autoReorder int
	if err := db.QueryRow(`SELECT role, swap_mode, payload_code, uop_capacity, auto_reorder, outbound_destination
		FROM style_node_claims WHERE core_node_name='PLK_W1'`).
		Scan(&role, &swap, &payload, &cap, &autoReorder, &outDst); err != nil {
		t.Fatalf("PLK_W1 (LOADER-COMP window) claim: %v", err)
	}
	if role != "produce" || swap != "manual_swap" || payload != "BRKT" || cap != 20 || autoReorder != 1 || outDst != "SYN_SM_Comp" {
		t.Fatalf("LOADER-COMP window claim fields wrong: role=%s swap=%s payload=%s cap=%d autoReorder=%d outDst=%s",
			role, swap, payload, cap, autoReorder, outDst)
	}
	// The synthetic loader identity PLK_LOADER is NOT a physical node — no anchor
	// process_node or claim exists for it (the windows are its only nodes).
	if n := count(`SELECT COUNT(*) FROM style_node_claims WHERE core_node_name='PLK_LOADER'`); n != 0 {
		t.Fatalf("PLK_LOADER should have no claim (synthetic identity), got %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM process_nodes WHERE core_node_name='PLK_LOADER'`); n != 0 {
		t.Fatalf("PLK_LOADER should have no process_node (synthetic identity), got %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM process_nodes WHERE core_node_name IN ('PLK_W1','PLK_W2','PLK_W3')`); n != 3 {
		t.Fatalf("three window process_nodes expected, got %d", n)
	}
	// Per-window HMI: each window's process_node is on its OWN operator station,
	// so each window is a separate physical screen (no shared board, no misload).
	if n := count(`SELECT COUNT(DISTINCT operator_station_id) FROM process_nodes
		WHERE core_node_name IN ('PLK_W1','PLK_W2','PLK_W3') AND operator_station_id IS NOT NULL`); n != 3 {
		t.Fatalf("windows should be on 3 distinct operator stations, got %d", n)
	}
	for _, st := range []string{"PLK_W1-OPS", "PLK_W2-OPS", "PLK_W3-OPS"} {
		if n := count(`SELECT COUNT(*) FROM operator_stations WHERE code=?`, st); n != 1 {
			t.Fatalf("per-window operator station %s: want 1, got %d", st, n)
		}
	}
	// process_nodes per (process, core_node): WELD-1 (two_robot) has 2.
	if n := count(`SELECT COUNT(*) FROM process_nodes WHERE core_node_name IN ('ALN_001','ALN_002')`); n != 2 {
		t.Fatalf("WELD-1 process_nodes: want 2, got %d", n)
	}
	// PRESS-1 has 1 process_node (front; the paired back node has no claim).
	if n := count(`SELECT COUNT(*) FROM process_nodes WHERE core_node_name IN ('PLN_001','PLN_002')`); n != 1 {
		t.Fatalf("PRESS-1 process_nodes: want 1, got %d", n)
	}

	// Consume node's cached count = its seeded bin's UOP (BIN-ACT-W1-LH, uop:15
	// in demo.yaml) so it drains to reorder.
	var w1LhUOP int
	if err := db.QueryRow(`SELECT r.remaining_uop_cached FROM process_node_runtime_states r
		JOIN process_nodes pn ON r.process_node_id=pn.id WHERE pn.core_node_name='ALN_001'`).Scan(&w1LhUOP); err != nil {
		t.Fatalf("ALN_001 (WELD-1 consume LH) cached uop: %v", err)
	}
	if w1LhUOP != 15 {
		t.Fatalf("ALN_001 remaining_uop_cached: want 15, got %d", w1LhUOP)
	}
	// Produce node's cached count = 0 (empty bin at seed).
	var p1LhUOP int
	if err := db.QueryRow(`SELECT r.remaining_uop_cached FROM process_node_runtime_states r
		JOIN process_nodes pn ON r.process_node_id=pn.id WHERE pn.core_node_name='PLN_001'`).Scan(&p1LhUOP); err != nil {
		t.Fatalf("PLN_001 (PRESS-1 LH output) cached uop: %v", err)
	}
	if p1LhUOP != 0 {
		t.Fatalf("PLN_001 remaining_uop_cached: want 0, got %d", p1LhUOP)
	}
}

func TestSeedEdge_Idempotent(t *testing.T) {
	db, _, plant := openSeededEdge(t)
	before := func() int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM style_node_claims`).Scan(&n)
		return n
	}
	n1 := before()
	if err := seedEdgeDB(db, plant, fakeBinIDs(plant)); err != nil { // re-run
		t.Fatalf("re-run: %v", err)
	}
	if n2 := before(); n2 != n1 {
		t.Fatalf("re-run changed claim count: %d → %d", n1, n2)
	}
}

type fakeChecker struct{ resolve bool }

func (f fakeChecker) GetNodeByName(name string) (*nodes.Node, error) {
	if f.resolve {
		return &nodes.Node{Name: name}, nil
	}
	return nil, sql.ErrNoRows
}

func TestCrossValidate(t *testing.T) {
	_, path, _ := openSeededEdge(t)
	if err := crossValidate(fakeChecker{resolve: true}, path); err != nil {
		t.Fatalf("crossValidate (all resolve) should pass: %v", err)
	}
	if err := crossValidate(fakeChecker{resolve: false}, path); err == nil {
		t.Fatal("crossValidate (none resolve) should report mismatches")
	}
}
