package store

import (
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
)

// orders_live_delivery_node_index_test.go — the read of live orders bound for a
// node goes through idx_orders_live_delivery_node, not a scan of every order the
// Edge has ever held.
//
// The index is partial (WHERE status NOT IN terminal), and SQLite only chooses
// a partial index when the query carries the same term. The queries spell their
// term with protocol.TerminalStatusSQLList; the migration spells it as a
// literal. This test is what notices the two drifting apart: a status added to
// or dropped from the terminal set leaves the index unused and the read back to
// a full scan, with no error anywhere else.

// TestLiveDeliveryNodeIndex_ServesTheBoundForLineRead asserts the PLAN for the
// two shapes orders.ListActiveByDeliveryNodeSet and
// orders.ListActiveByDeliveryNode issue.
func TestLiveDeliveryNodeIndex_ServesTheBoundForLineRead(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	live := ` AND o.status NOT IN (` + protocol.TerminalStatusSQLList() + `)`
	from := `SELECT o.id FROM orders o LEFT JOIN process_nodes n ON n.id = o.process_node_id`
	for _, tc := range []struct {
		name  string
		query string
		args  []any
	}{
		{"set", from + ` WHERE o.delivery_node IN (?,?)` + live + ` ORDER BY o.created_at`, []any{"A", "B"}},
		{"one", from + ` WHERE o.delivery_node = ?` + live + ` ORDER BY o.created_at`, []any{"A"}},
	} {
		rows, err := db.DB.Query(`EXPLAIN QUERY PLAN `+tc.query, tc.args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", tc.name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				t.Fatalf("%s: scan plan: %v", tc.name, err)
			}
			plan = append(plan, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: plan rows: %v", tc.name, err)
		}
		rows.Close()

		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "idx_orders_live_delivery_node") {
			t.Errorf("%s: the live-orders-by-delivery-node read does not use idx_orders_live_delivery_node. Plan:\n%s\n\n"+
				"Without it the read is `SCAN o`, every order the Edge holds. If the terminal status list "+
				"changed, add a migration that recreates the index with the new list.", tc.name, joined)
		}
		for _, line := range plan {
			if line == "SCAN o" {
				t.Errorf("%s: the read still scans the orders table: %q", tc.name, line)
			}
		}
	}
}
