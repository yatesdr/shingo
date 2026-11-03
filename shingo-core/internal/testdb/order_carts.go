package testdb

import (
	"slices"
	"testing"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store"
)

// order_carts.go — fixture writers and readers for an order's carts
// (order_bins), for pins that must state a cart row without going through a
// door. The SQL follows the table's shape; the pins that call these keep their
// text when the shape moves.

// SeedCartRow writes one cart row for orderID: binID lifted at plan step pick
// and set down at plan step drop. The junction's node copies (until v174) are
// filled from the order's plan.
func SeedCartRow(t testing.TB, db *store.DB, orderID, binID int64, pick, drop int) {
	t.Helper()
	o, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("testdb.SeedCartRow(order=%d): read the order: %v", orderID, err)
	}
	if _, err := db.Exec(`INSERT INTO order_bins (order_id, bin_id, pick_step, drop_step, action, node_name, dest_node)
		VALUES ($1, $2, $3, $4, 'pickup', $5, $6)
		ON CONFLICT (order_id, pick_step) DO UPDATE SET bin_id = EXCLUDED.bin_id, drop_step = EXCLUDED.drop_step,
			node_name = EXCLUDED.node_name, dest_node = EXCLUDED.dest_node`,
		orderID, binID, pick, drop, domain.StepNode(o, pick), domain.StepNode(o, drop)); err != nil {
		t.Fatalf("testdb.SeedCartRow(order=%d bin=%d pick=%d drop=%d): %v", orderID, binID, pick, drop, err)
	}
}

// CartDropNodes is, for each of orderID's cart rows, the node the row says its
// cart is set down at — its drop step in the order's plan — keyed by bin (a
// relayed cart has one per lift).
func CartDropNodes(t testing.TB, db *store.DB, orderID int64) map[int64][]string {
	t.Helper()
	o, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("testdb.CartDropNodes(order=%d): %v", orderID, err)
	}
	out := map[int64][]string{}
	for _, c := range o.Carts {
		node := ""
		if c.DropStep != nil {
			node = domain.StepNode(o, *c.DropStep)
		}
		out[c.BinID] = append(out[c.BinID], node)
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

// CartLift is one of an order's cart rows as nodes: the cart, where its pick
// step lifts it (From) and where its drop step sets it down (To), read through
// the order's plan (domain.StepNode); "" for a step the row does not know.
type CartLift struct {
	BinID    int64
	From, To string
}

// CartLifts is orderID's cart rows as nodes, in row order.
func CartLifts(t testing.TB, db *store.DB, orderID int64) []CartLift {
	t.Helper()
	o, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("testdb.CartLifts(order=%d): %v", orderID, err)
	}
	out := make([]CartLift, 0, len(o.Carts))
	for _, c := range o.Carts {
		l := CartLift{BinID: c.BinID}
		if c.PickStep != nil {
			l.From = domain.StepNode(o, *c.PickStep)
		}
		if c.DropStep != nil {
			l.To = domain.StepNode(o, *c.DropStep)
		}
		out = append(out, l)
	}
	return out
}

// SeedLift writes orderID's row for binID lifted at plan step pick and set down
// at the plan's next dropoff after it (SeedCartRow): the lift as the plan walk
// (domain.Lifts) writes a claim at pick.
func SeedLift(t testing.TB, db *store.DB, orderID, binID int64, pick int) {
	t.Helper()
	o, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("testdb.SeedLift(order=%d): %v", orderID, err)
	}
	steps := domain.OrderPlan(o)
	for i := pick + 1; i < len(steps); i++ {
		if steps[i].Action == protocol.ActionDropoff {
			SeedCartRow(t, db, orderID, binID, pick, i)
			return
		}
	}
	t.Fatalf("testdb.SeedLift(order=%d bin=%d pick=%d): the plan sets nothing down after the pick", orderID, binID, pick)
}
