package store

import (
	"path/filepath"
	"strings"
	"testing"

	"shingoedge/store/orders"
)

// orders_spot_read_index_test.go — the keep-staged keeper's read of a line
// (orders.ListKeptSpotRows) returns the line's live orders plus, in any status,
// its newest refill to the spot, its newest return off the spot (only one newer
// than that refill), and its newest other order; and reads them without
// walking the line's history.

// TestKeptSpotRows_LiveRowsAndTheThreeNewest pins what the read returns, in
// created_at order, and nothing else.
func TestKeptSpotRows_LiveRowsAndTheThreeNewest(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "spot.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	type row struct {
		uuid, typ, status, source, delivery, created string
		node                                         int64
	}
	seed := []row{
		{"old-refill", "retrieve_empty", "confirmed", "SRC", "SPOT", "2026-01-01 00:00:01", 1},
		{"old-return", "move", "cancelled", "SPOT", "SRC", "2026-01-01 00:00:02", 1},
		{"live-move", "move", "queued", "SRC", "LINE", "2026-01-01 00:00:03", 1},
		{"live-swap", "complex", "in_transit", "", "LINE", "2026-01-01 00:00:04", 1},
		{"newest-refill", "retrieve", "cancelled", "SRC", "SPOT", "2026-01-01 00:00:05", 1},
		{"newest-return", "move", "confirmed", "SPOT", "SRC", "2026-01-01 00:00:06", 1},
		{"spot-to-line", "move", "confirmed", "SPOT", "LINE", "2026-01-01 00:00:07", 1},
		{"other-node-live", "retrieve", "queued", "SRC", "SPOT", "2026-01-01 00:00:08", 2},
		{"other-node-newest", "complex", "confirmed", "", "LINE", "2026-01-01 00:00:09", 2},
	}
	for _, r := range seed {
		if _, err := db.Exec(`INSERT INTO orders(uuid, order_type, status, process_node_id, source_node, delivery_node, created_at)
			VALUES (?,?,?,?,?,?,?)`, r.uuid, r.typ, r.status, r.node, r.source, r.delivery, r.created); err != nil {
			t.Fatalf("seed %s: %v", r.uuid, err)
		}
	}

	uuids := func(spot string) string {
		got, err := orders.ListKeptSpotRows(db.DB, 1, spot, "LINE")
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var u []string
		for _, o := range got {
			u = append(u, o.UUID)
		}
		return strings.Join(u, ",")
	}
	// spot-to-line is the newest OTHER order: a move off the spot to the line is
	// a delivery to the line, not a return.
	if got, want := uuids("SPOT"), "live-move,live-swap,newest-refill,newest-return,spot-to-line"; got != want {
		t.Fatalf("the read returned %s, want %s", got, want)
	}
	// A line that keeps no refill history at a spot: the live rows and the
	// newest other order.
	if got, want := uuids("NO-SPOT"), "live-move,live-swap,spot-to-line"; got != want {
		t.Fatalf("without a spot history the read returned %s, want %s", got, want)
	}
}

// TestKeptSpotRows_ReadsNoHistory asserts the plan: no full scan of orders.
// The return search is bounded below by the newest refill's id and the newest
// other order stops at the first row, so neither walks the line's history.
func TestKeptSpotRows_ReadsNoHistory(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "spotplan.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	q, args := orders.KeptSpotRowsQuery(1, "SPOT", "LINE")
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	plan := strings.Join(lines, "\n")
	for _, must := range []string{"idx_orders_live_process_node", "idx_orders_process_node_delivery_id"} {
		if !strings.Contains(plan, must) {
			t.Errorf("the keeper's read does not use %s; plan:\n%s", must, plan)
		}
	}
	// The return search is bounded below by the newest refill's id; unbounded it
	// would walk every order the line has ever had, every sweep.
	if !strings.Contains(plan, "rowid>?") {
		t.Errorf("the return search is not bounded by the newest refill; plan:\n%s", plan)
	}
	// A SCAN of a one-row derived table (the LIMIT 1 subqueries) reads nothing;
	// a SCAN of orders reads the whole table.
	for _, line := range strings.Split(plan, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "SCAN ") && !strings.HasPrefix(l, "SCAN (subquery-") {
			t.Errorf("the keeper's read scans a table: %q; plan:\n%s", line, plan)
		}
	}
}
