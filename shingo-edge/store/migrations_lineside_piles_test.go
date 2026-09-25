package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// migrations_lineside_piles_test.go — the move to one pile identity on a
// database the previous build wrote: synthetic rows in the old shape, then a
// reopen, which runs migrate().

// legacyPileDB opens a fresh database, puts node_lineside_bucket back in the
// previous build's shape (style_id, pair_key, the partial active index) with
// the given rows, and closes it. Returns the path, for the reopen that
// migrates.
func legacyPileDB(t *testing.T, seed func(db *DB, nodeID int64)) (path string, nodeID int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "legacy-piles.db")
	db, err := Open(path)
	testutil.MustNoErr(t, err, "open fresh db")
	procID, err := db.CreateProcess("MIG-PILE-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "MIG-PILE-SEAT", Code: "C1", Name: "MIG-PILE-SEAT", Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	_, err = db.Exec(`
		DROP TABLE node_lineside_bucket;
		CREATE TABLE node_lineside_bucket (
		    id           INTEGER PRIMARY KEY AUTOINCREMENT,
		    node_id      INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
		    pair_key     TEXT NOT NULL DEFAULT '',
		    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
		    payload_code TEXT NOT NULL,
		    qty          INTEGER NOT NULL DEFAULT 0,
		    state        TEXT NOT NULL DEFAULT 'active',
		    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
		    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE UNIQUE INDEX idx_lineside_active_unique
		    ON node_lineside_bucket(node_id, payload_code) WHERE state = 'active';`)
	testutil.MustNoErr(t, err, "restore the legacy pile table")
	seed(db, nodeID)
	testutil.MustNoErr(t, db.Close(), "close legacy db")
	return path, nodeID
}

// Two inactive rows of one (node, payload), left by two earlier styles, fold
// into ONE stranded row with their qty summed and the latest updated_at; the
// active row of the same payload comes across as it is, id included; the
// bucket-scope seq rows and the retired delta subject's unsent and
// dead-lettered outbox rows go, and a bin seq row and a sent delta row stay.
func TestMigrate_LinesidePilesFoldToOneIdentity(t *testing.T) {
	t.Parallel()
	path, nodeID := legacyPileDB(t, func(db *DB, nodeID int64) {
		_, err := db.Exec(`INSERT INTO node_lineside_bucket
			(id, node_id, pair_key, style_id, payload_code, qty, state, updated_at) VALUES
			(10, ?, '', 1, 'SYN-PART-1', 12, 'active',   '2026-09-20 10:00:00'),
			(11, ?, '', 2, 'SYN-PART-1', 30, 'inactive', '2026-09-18 10:00:00'),
			(12, ?, 'P', 3, 'SYN-PART-1', 5, 'inactive', '2026-09-19 10:00:00'),
			(13, ?, '', 3, 'SYN-PART-2', 0, 'inactive', '2026-09-19 10:00:00')`,
			nodeID, nodeID, nodeID, nodeID)
		testutil.MustNoErr(t, err, "seed legacy piles")
		_, err = db.Exec(`INSERT INTO inventory_delta_seq (scope_kind, scope_key, epoch, next_seq, net) VALUES
			('bucket', 'MIG-PILE-SEAT||1|SYN-PART-1', 0, 9, 12),
			('bin', '77', 1, 4, -3)`)
		testutil.MustNoErr(t, err, "seed seq rows")
		_, err = db.Exec(`INSERT INTO outbox (topic, payload, msg_type, retries, sent_at) VALUES
			('t', '{}', 'inventory.lineside_bucket_delta', 0, NULL),
			('t', '{}', 'inventory.lineside_bucket_delta', 99, NULL),
			('t', '{}', 'inventory.lineside_bucket_delta', 0, datetime('now')),
			('t', '{}', 'inventory.bin_uop_delta', 0, NULL)`)
		testutil.MustNoErr(t, err, "seed outbox rows")
	})

	db, err := Open(path)
	testutil.MustNoErr(t, err, "reopen (migrate)")
	t.Cleanup(func() { db.Close() })

	type pile struct {
		id      int64
		qty     int
		updated string
	}
	got := map[string]pile{}
	rows, err := db.Query(`SELECT id, payload_code, state, qty, updated_at FROM node_lineside_bucket WHERE node_id = ?`, nodeID)
	testutil.MustNoErr(t, err, "read piles")
	for rows.Next() {
		var p pile
		var payload, state string
		testutil.MustNoErr(t, rows.Scan(&p.id, &payload, &state, &p.qty, &p.updated), "scan pile")
		got[payload+"/"+state] = p
	}
	rows.Close()
	if len(got) != 2 {
		t.Fatalf("piles = %+v, want exactly SYN-PART-1/active and SYN-PART-1/stranded (the empty inactive row goes)", got)
	}
	if p := got["SYN-PART-1/active"]; p.id != 10 || p.qty != 12 {
		t.Errorf("active pile = %+v, want id 10 qty 12, carried as it was", p)
	}
	if p := got["SYN-PART-1/stranded"]; p.qty != 35 || p.updated != "2026-09-19 10:00:00" {
		t.Errorf("stranded pile = %+v, want qty 35 (30 + 5) and the latest updated_at 2026-09-19 10:00:00", p)
	}

	var bucketSeq, binSeq int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM inventory_delta_seq WHERE scope_kind = 'bucket'`).Scan(&bucketSeq), "count bucket seq")
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM inventory_delta_seq WHERE scope_kind = 'bin'`).Scan(&binSeq), "count bin seq")
	if bucketSeq != 0 || binSeq != 1 {
		t.Errorf("seq rows: bucket=%d bin=%d, want 0 and 1", bucketSeq, binSeq)
	}
	var unsentDelta, sentDelta, binRows int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = 'inventory.lineside_bucket_delta' AND sent_at IS NULL`).Scan(&unsentDelta), "count unsent")
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = 'inventory.lineside_bucket_delta' AND sent_at IS NOT NULL`).Scan(&sentDelta), "count sent")
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = 'inventory.bin_uop_delta'`).Scan(&binRows), "count bin rows")
	if unsentDelta != 0 || sentDelta != 1 || binRows != 1 {
		t.Errorf("outbox: unsent delta=%d sent delta=%d bin=%d, want 0, 1, 1", unsentDelta, sentDelta, binRows)
	}

	// The new shape holds: a second stranded row of the same (node, payload)
	// is refused.
	if _, err := db.Exec(`INSERT INTO node_lineside_bucket (node_id, payload_code, qty, state)
		VALUES (?, 'SYN-PART-1', 1, 'stranded')`, nodeID); err == nil {
		t.Error("a second stranded SYN-PART-1 row was accepted; UNIQUE(node_id, payload_code, state) is missing")
	}
}

// The migration strands, once, every active pile whose part the process's
// active style does not claim at that node (owner decision, 2026-09-25): such
// a pile has already crossed a cutover, and the old code only never stranded
// it. The seat's active style claims SYN-CLAIMED as its payload and
// SYN-ALLOWED among its allowed payloads; both piles stay active. The
// unclaimed SYN-OTHER pile ends stranded, folded into the inactive SYN-OTHER
// row the old shape already had (9 + 4). A second seat the active style does
// not claim at all keeps its SYN-OTHER pile active, as Core's old count did.
func TestMigrate_LinesidePilesStrandsWhatTheActiveStyleDoesNotClaim(t *testing.T) {
	t.Parallel()
	var idleNodeID int64
	path, _ := legacyPileDB(t, func(db *DB, nodeID int64) {
		var procID int64
		testutil.MustNoErr(t, db.QueryRow(`SELECT process_id FROM process_nodes WHERE id = ?`, nodeID).Scan(&procID), "read process")
		styleID, err := db.CreateStyle("MIG-PILE-STYLE", "", procID)
		testutil.MustNoErr(t, err, "create style")
		testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "set active style")
		idleNodeID, err = db.CreateProcessNode(processes.NodeInput{
			ProcessID: procID, CoreNodeName: "MIG-PILE-IDLE", Code: "C2", Name: "MIG-PILE-IDLE", Enabled: true,
		})
		testutil.MustNoErr(t, err, "create unclaimed node")
		_, err = db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, swap_mode, payload_code, allowed_payload_codes)
			VALUES (?, 'MIG-PILE-SEAT', 'simple', 'SYN-CLAIMED', '["SYN-ALLOWED"]')`, styleID)
		testutil.MustNoErr(t, err, "seed the active style's claim")
		_, err = db.Exec(`INSERT INTO node_lineside_bucket
			(node_id, pair_key, style_id, payload_code, qty, state) VALUES
			(?, '', ?, 'SYN-CLAIMED', 7, 'active'),
			(?, '', ?, 'SYN-ALLOWED', 3, 'active'),
			(?, '', ?, 'SYN-OTHER',   9, 'active'),
			(?, '', 1, 'SYN-OTHER',   4, 'inactive'),
			(?, '', ?, 'SYN-OTHER',   5, 'active')`,
			nodeID, styleID, nodeID, styleID, nodeID, styleID, nodeID, idleNodeID, styleID)
		testutil.MustNoErr(t, err, "seed legacy piles")
	})

	db, err := Open(path)
	testutil.MustNoErr(t, err, "reopen (migrate)")
	t.Cleanup(func() { db.Close() })

	got := map[string]int{}
	rows, err := db.Query(`SELECT node_id, payload_code, state, qty FROM node_lineside_bucket`)
	testutil.MustNoErr(t, err, "read piles")
	for rows.Next() {
		var node int64
		var payload, state string
		var qty int
		testutil.MustNoErr(t, rows.Scan(&node, &payload, &state, &qty), "scan pile")
		seat := "seat"
		if node == idleNodeID {
			seat = "idle"
		}
		got[seat+"/"+payload+"/"+state] = qty
	}
	rows.Close()
	want := map[string]int{
		"seat/SYN-CLAIMED/active": 7,
		"seat/SYN-ALLOWED/active": 3,
		"seat/SYN-OTHER/stranded": 13,
		"idle/SYN-OTHER/active":   5,
	}
	if len(got) != len(want) {
		t.Errorf("piles = %v, want %v", got, want)
	}
	for k, q := range want {
		if got[k] != q {
			t.Errorf("%s = %d, want %d (all piles: %v)", k, got[k], q, got)
		}
	}
}
