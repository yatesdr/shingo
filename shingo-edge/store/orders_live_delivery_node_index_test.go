package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/store/orders"
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

// TestLiveOrderPlacing_ReadsTheLiveRowsThroughTheLiveIndex — the produce hold's
// question, "is a live order placing a carrier at this node", is one statement
// that walks only the live rows: through idx_orders_live_process_node, the
// partial index whose WHERE is the terminal list, never a scan of every order
// the Edge has held. The statement is taken from the counting driver as issued.
func TestLiveOrderPlacing_ReadsTheLiveRowsThroughTheLiveIndex(t *testing.T) {
	t.Parallel()
	db, counter, err := OpenCounting(filepath.Join(t.TempDir(), "placing.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	counter.Reset()
	if _, err := db.LiveOrderPlacing("A"); err != nil {
		t.Fatalf("placing: %v", err)
	}
	stmts := counter.Statements()
	if len(stmts) != 1 {
		t.Fatalf("statements = %q, want exactly one", stmts)
	}
	rows, err := db.DB.Query(`EXPLAIN QUERY PLAN `+stmts[0], "A")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_orders_live_process_node") || strings.Contains(joined, "SCAN o\n") || strings.HasSuffix(joined, "SCAN o") {
		t.Errorf("the placing read does not walk the live rows through idx_orders_live_process_node. Plan:\n%s", joined)
	}
}

// TestLiveOrderPlacing_Answers — what the statement answers, row by row, at
// node A. A leg's places come from its persisted release facts; a plain order
// (no steps) places at its delivery node; a leg with steps and no facts (a row
// older than the column) reads as placing; a leg whose steps are none ("null"
// or "[]", as legFacts reads them) places nowhere; a terminal row is not live;
// and live facts that do not parse are unreadable, which outranks nothing
// placing and is outranked by any row that is.
func TestLiveOrderPlacing_Answers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		rows []string // status | steps_json | release_facts ("NULL" for none) | delivery_node
		want orders.Placing
	}{
		{"no orders", nil, orders.PlacingNone},
		{"a leg placing at A", []string{`staged|[{}]|{"places":["A"]}|`}, orders.PlacingCarrier},
		{"a leg placing elsewhere", []string{`staged|[{}]|{"places":["B"]}|A`}, orders.PlacingNone},
		{"a leg placing nowhere", []string{`staged|[{}]|{"role":"departing"}|A`}, orders.PlacingNone},
		{"a terminal leg placing at A", []string{`confirmed|[{}]|{"places":["A"]}|`}, orders.PlacingNone},
		{"a plain order to A", []string{`in_transit|||A`}, orders.PlacingCarrier},
		{"a plain order elsewhere", []string{`in_transit|||B`}, orders.PlacingNone},
		{"a pre-facts leg", []string{`in_transit|[{}]|NULL|`}, orders.PlacingCarrier},
		{"a leg with no steps", []string{`in_transit|null|NULL|A`, `in_transit|[]|NULL|A`}, orders.PlacingNone},
		{"unreadable facts", []string{`staged|[{}]|{"places":|`}, orders.PlacingUnreadable},
		{"unreadable facts beside a placing leg", []string{`staged|[{}]|{"places":|`, `staged|[{}]|{"places":["A"]}|`}, orders.PlacingCarrier},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, err := Open(filepath.Join(t.TempDir(), "placing.db"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer db.Close()
			for i, r := range tc.rows {
				f := strings.Split(r, "|")
				var facts any = f[2]
				if f[2] == "NULL" {
					facts = nil
				}
				if _, err := db.DB.Exec(`INSERT INTO orders (uuid, order_type, status, steps_json, release_facts, delivery_node)
					VALUES (?, 'complex', ?, ?, ?, ?)`, fmt.Sprintf("u%d", i), f[0], f[1], facts, f[3]); err != nil {
					t.Fatalf("insert %q: %v", r, err)
				}
			}
			got, err := db.LiveOrderPlacing("A")
			if err != nil {
				t.Fatalf("placing: %v", err)
			}
			if got != tc.want {
				t.Errorf("placing at A = %v, want %v", got, tc.want)
			}
		})
	}
}
