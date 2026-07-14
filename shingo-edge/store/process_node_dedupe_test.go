package store

import (
	"testing"
)

// seedDupProcessNodes reproduces the HK 2026-07-14 shape: one Core node
// (PLN_01) modelled by three process_nodes rows under the same process — two
// orphaned (no station), one live on a station. The UNIQUE index the collapse
// migration installs makes this un-insertable, so drop it first; that is exactly
// the pre-migration world this function has to survive.
func seedDupProcessNodes(t *testing.T, db *DB) (station, live, orphanWithBin, orphanBinless int64) {
	t.Helper()
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_process_nodes_process_core_name`); err != nil {
		t.Fatalf("drop unique index: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO processes (id, name) VALUES (1, 'P400')`); err != nil {
		t.Fatalf("seed process: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO operator_stations (id, process_id, name, code) VALUES (5, 1, 'Press 400', 'p400')`); err != nil {
		t.Fatalf("seed station: %v", err)
	}
	// Insert order mirrors HK: pln-01 (orphan, still pointing at a bin),
	// pln-01-2 (orphan, bin-less, hoarding pending ticks), pln-01-3 (live).
	rows := []struct {
		id      int64
		code    string
		station any
	}{
		{1, "pln-01", nil},
		{13, "pln-01-2", nil},
		{17, "pln-01-3", int64(5)},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO process_nodes (id, process_id, operator_station_id, core_node_name, code, name)
			VALUES (?, 1, ?, 'PLN_01', ?, 'PLN_01')`, r.id, r.station, r.code); err != nil {
			t.Fatalf("seed process_node %s: %v", r.code, err)
		}
	}
	// Runtime: the live row holds the real bin; the orphans hold phantom state.
	seedRT := func(pn int64, bin any, uop, pending int, updated string) {
		if _, err := db.Exec(`INSERT INTO process_node_runtime_states
			(process_node_id, active_bin_id, remaining_uop_cached, pending_uop_delta, updated_at)
			VALUES (?, ?, ?, ?, ?)`, pn, bin, uop, pending, updated); err != nil {
			t.Fatalf("seed runtime for %d: %v", pn, err)
		}
	}
	seedRT(1, int64(13), 850, 0, "2026-07-14 13:42:35")
	seedRT(13, nil, 0, 28670, "2026-06-25 11:40:14")
	seedRT(17, int64(4), 850, 0, "2026-07-14 15:47:15")
	return 5, 17, 1, 13
}

// TestCollapseDuplicateProcessNodes_KeepsStationBoundRowAndRepoints pins the
// survivor rule (station-bound wins) and the repointing of every referrer. The
// phantom runtime on the orphans must be discarded, NOT replayed — 28,670 held
// ticks are double-counts of strokes the live row already booked.
func TestCollapseDuplicateProcessNodes_KeepsStationBoundRowAndRepoints(t *testing.T) {
	db := testDB(t)
	_, live, orphanWithBin, orphanBinless := seedDupProcessNodes(t, db)

	// Orders spread across all three rows — the two orphans' orders must survive,
	// repointed at the live row, not be orphaned to NULL.
	for i, pn := range []int64{orphanWithBin, live, orphanBinless} {
		if _, err := db.Exec(`INSERT INTO orders (uuid, order_type, status, quantity, process_node_id)
			VALUES (?, 'complex', 'delivered', 1, ?)`, "uuid-dedupe-"+string(rune('a'+i)), pn); err != nil {
			t.Fatalf("seed order on pn %d: %v", pn, err)
		}
	}

	if err := db.collapseDuplicateProcessNodes(); err != nil {
		t.Fatalf("collapseDuplicateProcessNodes: %v", err)
	}

	// Exactly one PLN_01 row, and it is the station-bound one.
	var count, survivor int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(id),0) FROM process_nodes WHERE core_node_name='PLN_01'`).Scan(&count, &survivor); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("PLN_01 rows = %d, want 1 (duplicates not collapsed)", count)
	}
	if survivor != live {
		t.Errorf("survivor = %d, want %d (the station-bound row must win — it is the one the HMI reads)", survivor, live)
	}

	// Every order repointed at the survivor; none stranded on a deleted row.
	var onSurvivor, stranded int64
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM orders WHERE process_node_id = ?),
		(SELECT COUNT(*) FROM orders WHERE process_node_id IS NULL OR process_node_id IN (?,?))`,
		live, orphanWithBin, orphanBinless).Scan(&onSurvivor, &stranded); err != nil {
		t.Fatalf("order counts: %v", err)
	}
	if onSurvivor != 3 {
		t.Errorf("orders on survivor = %d, want 3 (all repointed)", onSurvivor)
	}
	if stranded != 0 {
		t.Errorf("stranded orders = %d, want 0", stranded)
	}

	// The survivor keeps its OWN runtime — the phantom 28,670 is gone, not merged.
	var bin, uop, pending int64
	if err := db.QueryRow(`SELECT COALESCE(active_bin_id,0), remaining_uop_cached, pending_uop_delta
		FROM process_node_runtime_states WHERE process_node_id = ?`, live).Scan(&bin, &uop, &pending); err != nil {
		t.Fatalf("survivor runtime: %v", err)
	}
	if bin != 4 || uop != 850 {
		t.Errorf("survivor runtime = bin %d/uop %d, want bin 4/uop 850 (live state must be untouched)", bin, uop)
	}
	if pending != 0 {
		t.Errorf("survivor pending_uop_delta = %d, want 0 — the orphans' phantom ticks must be DISCARDED, not replayed onto the live bin", pending)
	}

	// Orphan runtime rows are gone.
	var rtLeft int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_node_runtime_states WHERE process_node_id IN (?,?)`,
		orphanWithBin, orphanBinless).Scan(&rtLeft); err != nil {
		t.Fatalf("orphan runtime count: %v", err)
	}
	if rtLeft != 0 {
		t.Errorf("orphan runtime rows = %d, want 0", rtLeft)
	}
}

// TestCollapseDuplicateProcessNodes_EnforcesUniqueAfterwards proves the collapse
// installs the constraint that stops this recurring — a second PLN_01 under the
// same process must now be rejected by the database itself, not just by SetNodes.
func TestCollapseDuplicateProcessNodes_EnforcesUniqueAfterwards(t *testing.T) {
	db := testDB(t)
	seedDupProcessNodes(t, db)

	if err := db.collapseDuplicateProcessNodes(); err != nil {
		t.Fatalf("collapseDuplicateProcessNodes: %v", err)
	}
	_, err := db.Exec(`INSERT INTO process_nodes (process_id, core_node_name, code, name)
		VALUES (1, 'PLN_01', 'pln-01-4', 'PLN_01')`)
	if err == nil {
		t.Fatal("inserting a second PLN_01 under process 1 succeeded — UNIQUE(process_id, core_node_name) was not enforced")
	}
}

// TestCollapseDuplicateProcessNodes_NoopWhenClean guards the common case: on a
// database with no duplicates the migration must change nothing and still leave
// the constraint in place. It runs on every startup.
func TestCollapseDuplicateProcessNodes_NoopWhenClean(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO processes (id, name) VALUES (1, 'P400')`); err != nil {
		t.Fatalf("seed process: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO process_nodes (id, process_id, core_node_name, code, name)
		VALUES (9, 1, 'PLN_09', 'pln-09', 'PLN_09')`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := db.collapseDuplicateProcessNodes(); err != nil {
		t.Fatalf("collapse on clean db: %v", err)
	}
	// Idempotent — a second pass must also be clean.
	if err := db.collapseDuplicateProcessNodes(); err != nil {
		t.Fatalf("second collapse pass: %v", err)
	}
	var count int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_nodes WHERE core_node_name='PLN_09'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("PLN_09 rows = %d, want 1 (clean db must be untouched)", count)
	}
}

// TestCollapseDuplicateProcessNodes_UnboundNodesAreNotDuplicates guards the
// sharpest edge of this migration. core_node_name is NOT NULL DEFAULT ” and
// nothing validates it — CreateNode only trims, and apiCreateProcessNode decodes
// straight into it — so a process can legitimately hold several UNBOUND nodes.
//
// They all share the empty string, but they are NOT duplicates of one another.
// Grouping them would delete real nodes, discard their runtime and repoint their
// orders onto an unrelated survivor. The group query excludes them and the unique
// index is partial on the same predicate, so unbound nodes stay possible.
func TestCollapseDuplicateProcessNodes_UnboundNodesAreNotDuplicates(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO processes (id, name) VALUES (1, 'P400')`); err != nil {
		t.Fatalf("seed process: %v", err)
	}
	// Two DISTINCT nodes that merely have no Core node bound yet.
	for _, c := range []struct {
		id   int64
		code string
	}{{101, "unbound-a"}, {102, "unbound-b"}} {
		if _, err := db.Exec(`INSERT INTO process_nodes (id, process_id, core_node_name, code, name)
			VALUES (?, 1, '', ?, ?)`, c.id, c.code, c.code); err != nil {
			t.Fatalf("seed unbound node %s: %v", c.code, err)
		}
		if _, err := db.Exec(`INSERT INTO process_node_runtime_states (process_node_id, remaining_uop_cached)
			VALUES (?, 42)`, c.id); err != nil {
			t.Fatalf("seed runtime %s: %v", c.code, err)
		}
	}

	if err := db.collapseDuplicateProcessNodes(); err != nil {
		t.Fatalf("collapseDuplicateProcessNodes: %v", err)
	}

	var count int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_nodes WHERE core_node_name = ''`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("unbound nodes = %d, want 2 — two nodes with no Core node bound are DISTINCT, not duplicates; collapsing them destroys real rows", count)
	}
	var rt int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_node_runtime_states WHERE process_node_id IN (101,102)`).Scan(&rt); err != nil {
		t.Fatalf("runtime count: %v", err)
	}
	if rt != 2 {
		t.Errorf("unbound runtime rows = %d, want 2 — their state must not be discarded", rt)
	}

	// And the partial index must still permit a THIRD unbound node.
	if _, err := db.Exec(`INSERT INTO process_nodes (process_id, core_node_name, code, name)
		VALUES (1, '', 'unbound-c', 'unbound-c')`); err != nil {
		t.Fatalf("inserting a third unbound node was rejected — the unique index must be PARTIAL (WHERE core_node_name <> ''): %v", err)
	}
}

// TestCollapseDuplicateProcessNodes_MigratesInactiveLinesideBuckets pins the
// collision guard's scope.
//
// The unique index on node_lineside_bucket is PARTIAL — UNIQUE(node_id,
// part_number) WHERE state='active' — so only ACTIVE buckets can collide. The
// guard used to test just the survivor's side: "does the survivor hold an active
// bucket for this part?" If it did, EVERY bucket on the dead row for that part
// was refused the move and then deleted, including inactive ones that could never
// have collided with anything. Those carry closed-out operator part counts.
//
// Active collision → dropped (the survivor's row is the live one). Inactive →
// migrated, always.
func TestCollapseDuplicateProcessNodes_MigratesInactiveLinesideBuckets(t *testing.T) {
	db := testDB(t)
	_, live, orphanWithBin, _ := seedDupProcessNodes(t, db)

	if _, err := db.Exec(`INSERT INTO styles (id, process_id, name) VALUES (1, 1, 'STYLE-A')`); err != nil {
		t.Fatalf("seed style: %v", err)
	}
	mkBucket := func(id, nodeID int64, part, state string, qty int) {
		if _, err := db.Exec(`INSERT INTO node_lineside_bucket (id, node_id, style_id, part_number, qty, state)
			VALUES (?, ?, 1, ?, ?, ?)`, id, nodeID, part, qty, state); err != nil {
			t.Fatalf("seed bucket %d: %v", id, err)
		}
	}
	// The survivor already holds the live count for PART-A.
	mkBucket(100, live, "PART-A", "active", 40)
	// The orphan holds a colliding ACTIVE bucket for the same part (must drop) …
	mkBucket(101, orphanWithBin, "PART-A", "active", 7)
	// … and a CLOSED one for the same part, which collides with nothing (must move).
	mkBucket(102, orphanWithBin, "PART-A", "captured", 25)

	if err := db.collapseDuplicateProcessNodes(); err != nil {
		t.Fatalf("collapse: %v", err)
	}

	var node int64
	if err := db.QueryRow(`SELECT node_id FROM node_lineside_bucket WHERE id = 102`).Scan(&node); err != nil {
		t.Fatalf("the INACTIVE bucket was deleted — it is partial-index-exempt and could never have collided: %v", err)
	}
	if node != live {
		t.Errorf("inactive bucket node_id = %d, want %d (the survivor)", node, live)
	}

	// The active collision is gone, and the survivor's own row is untouched.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_lineside_bucket WHERE id = 101`).Scan(&n); err != nil {
		t.Fatalf("count colliding bucket: %v", err)
	}
	if n != 0 {
		t.Error("the colliding ACTIVE bucket should have been dropped — the survivor's row is the live count")
	}
	var qty int
	if err := db.QueryRow(`SELECT qty FROM node_lineside_bucket WHERE id = 100`).Scan(&qty); err != nil {
		t.Fatalf("survivor bucket missing: %v", err)
	}
	if qty != 40 {
		t.Errorf("survivor bucket qty = %d, want 40 (untouched)", qty)
	}
}

// A collapse that cannot finish must leave the database exactly as it was. The
// group's repoints and deletes are one transaction, because this runs unattended
// at edge startup against a live plant DB and a half-collapsed node is a shape
// nobody has reasoned about.
func TestCollapseDuplicateProcessNodes_GroupIsAtomic(t *testing.T) {
	db := testDB(t)
	_, _, orphanWithBin, _ := seedDupProcessNodes(t, db)

	// Make the very last statement of the group's collapse fail: a trigger that
	// rejects the DELETE of the process_nodes row itself. Everything before it
	// (orders/changeover/lineside repoints, runtime delete) will have run.
	if _, err := db.Exec(`CREATE TRIGGER block_pn_delete BEFORE DELETE ON process_nodes
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	if err := db.collapseDuplicateProcessNodes(); err == nil {
		t.Fatal("collapse should have failed — the trigger aborts the node delete")
	}

	// The orphan's runtime row was deleted BEFORE the failing statement. If the
	// group weren't transactional it would be gone for good, and its 28,670 held
	// ticks with it, while the node it belonged to still sat there.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM process_node_runtime_states WHERE process_node_id = ?`, orphanWithBin).Scan(&n); err != nil {
		t.Fatalf("count runtime: %v", err)
	}
	if n != 1 {
		t.Errorf("orphan runtime rows = %d, want 1 — a failed collapse must roll back whole, not leave the node stripped of its state", n)
	}
}
