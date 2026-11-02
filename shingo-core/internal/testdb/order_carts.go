package testdb

import (
	"slices"
	"testing"

	"shingocore/store"
)

// order_carts.go — fixture writers and readers for an order's carts
// (order_bins), for pins that must state a cart row without going through a
// door. The SQL follows the table's shape; the pins that call these keep their
// text when the shape moves.

// SeedCartRow writes one cart row for orderID: binID lifted at plan step pick
// and set down at plan step drop. The junction's node copies are filled from
// the order's plan (steps_json), else its source and delivery nodes.
func SeedCartRow(t testing.TB, db *store.DB, orderID, binID int64, pick, drop int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO order_bins (order_id, bin_id, step_index, action, node_name, dest_node)
		SELECT o.id, $2, $3, 'pickup',
		       COALESCE(CASE WHEN o.steps_json LIKE '[%' THEN o.steps_json::jsonb->($3::int)->>'node' END, o.source_node, ''),
		       COALESCE(CASE WHEN o.steps_json LIKE '[%' THEN o.steps_json::jsonb->($4::int)->>'node' END, o.delivery_node, '')
		  FROM orders o WHERE o.id = $1`, orderID, binID, pick, drop); err != nil {
		t.Fatalf("testdb.SeedCartRow(order=%d bin=%d pick=%d drop=%d): %v", orderID, binID, pick, drop, err)
	}
}

// CartDropNodes is, for each of orderID's cart rows, the node the row says its
// cart is set down at, keyed by bin (a relayed cart has one per lift).
func CartDropNodes(t testing.TB, db *store.DB, orderID int64) map[int64][]string {
	t.Helper()
	rows, err := db.Query(`SELECT bin_id, dest_node FROM order_bins WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		t.Fatalf("testdb.CartDropNodes(order=%d): %v", orderID, err)
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var node string
		if err := rows.Scan(&id, &node); err != nil {
			t.Fatalf("testdb.CartDropNodes(order=%d): scan: %v", orderID, err)
		}
		out[id] = append(out[id], node)
	}
	return out
}

// CartRowBins is the bin of each of orderID's cart rows, in row order.
func CartRowBins(t testing.TB, db *store.DB, orderID int64) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT bin_id FROM order_bins WHERE order_id=$1 ORDER BY id`, orderID)
	if err != nil {
		t.Fatalf("testdb.CartRowBins(order=%d): %v", orderID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("testdb.CartRowBins(order=%d): scan: %v", orderID, err)
		}
		out = append(out, id)
	}
	return out
}

// HasCartRow reports whether orderID has a cart row naming binID.
func HasCartRow(t testing.TB, db *store.DB, orderID, binID int64) bool {
	t.Helper()
	return slices.Contains(CartRowBins(t, db, orderID), binID)
}
