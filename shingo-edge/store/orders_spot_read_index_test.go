package store

import (
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/store/orders"
)

// orders_spot_read_index_test.go — the level keeper's read of a keep-staged
// line (orders.ListActiveByProcessNodeWithLatestTo) returns the node's live
// orders plus the newest refill to the spot in any status, and reads them
// through the v15 indexes instead of the node's whole history.

// TestSpotRead_LiveRowsAndTheNewestRefill pins what the read returns: the
// node's live orders and its newest retrieve to the spot whatever its status,
// in created_at order, and nothing else — not an older refill, not another
// node's orders, not a newer order of another type.
func TestSpotRead_LiveRowsAndTheNewestRefill(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	type row struct {
		uuid, typ, status, delivery, created string
		node                                 int64
	}
	seed := []row{
		{"old-refill", "retrieve_empty", "confirmed", "SPOT", "2026-01-01 00:00:01", 1},
		{"newest-refill", "retrieve", "failed", "SPOT", "2026-01-01 00:00:05", 1},
		{"newer-store", "store", "confirmed", "SPOT", "2026-01-01 00:00:06", 1},
		{"live-swap", "complex", "in_transit", "LINE", "2026-01-01 00:00:03", 1},
		{"live-move", "move", "queued", "LINE", "2026-01-01 00:00:02", 1},
		{"ended-swap", "complex", "confirmed", "LINE", "2026-01-01 00:00:04", 1},
		{"other-node-live", "retrieve", "queued", "SPOT", "2026-01-01 00:00:07", 2},
		{"other-node-refill", "retrieve", "confirmed", "SPOT", "2026-01-01 00:00:08", 2},
	}
	for _, r := range seed {
		if _, err := db.Exec(`INSERT INTO orders(uuid, order_type, status, process_node_id, delivery_node, created_at)
			VALUES (?,?,?,?,?,?)`, r.uuid, r.typ, r.status, r.node, r.delivery, r.created); err != nil {
			t.Fatalf("seed %s: %v", r.uuid, err)
		}
	}

	got, err := orders.ListActiveByProcessNodeWithLatestTo(db.DB, 1, "SPOT")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var uuids []string
	for _, o := range got {
		uuids = append(uuids, o.UUID)
	}
	want := "live-move,live-swap,newest-refill"
	if strings.Join(uuids, ",") != want {
		t.Fatalf("the spot read returned %v, want %s (live rows plus the newest refill, in created_at order)",
			uuids, want)
	}

	// A node with no refill to the spot: the live rows alone.
	got, err = orders.ListActiveByProcessNodeWithLatestTo(db.DB, 1, "NO-SPOT")
	if err != nil {
		t.Fatalf("read without a refill: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("without a refill the read returned %d rows, want the 2 live ones", len(got))
	}
}

// TestSpotRead_ReadsNoHistory asserts the plan: no scan of orders and no walk of
// the node's history through idx_orders_process_node_id. The live index is
// partial on the same terminal list as v14, so a change to that list fails here
// and needs a new migration.
func TestSpotRead_ReadsNoHistory(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "spotplan.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	q := `SELECT o.id FROM orders o WHERE o.id IN (
		SELECT l.id FROM orders l WHERE l.process_node_id = ? AND l.status NOT IN (` + protocol.TerminalStatusSQLList() + `)
		UNION ALL
		SELECT MAX(r.id) FROM orders r WHERE r.process_node_id = ? AND r.delivery_node = ? AND r.order_type IN (?, ?))
		ORDER BY o.created_at`
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+q, 1, 1, "SPOT", "retrieve", "retrieve_empty")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	all := strings.Join(plan, "\n")
	for _, must := range []string{"idx_orders_live_process_node", "idx_orders_process_node_delivery_id"} {
		if !strings.Contains(all, must) {
			t.Errorf("the spot read does not use %s; plan:\n%s", must, all)
		}
	}
	for _, mustNot := range []string{"SCAN o", "SCAN l", "SCAN r", "idx_orders_process_node_id"} {
		if strings.Contains(all, mustNot) {
			t.Errorf("the spot read's plan has %q, so it reads history; plan:\n%s", mustNot, all)
		}
	}
}
