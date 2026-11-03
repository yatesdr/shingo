package testdb

import (
	"slices"
	"testing"

	"shingocore/domain"
	"shingocore/store"
	"shingocore/store/orders"
)

// order_carts.go — fixture writers and readers for an order's carts
// (order_bins), for pins that must state a cart row without going through a
// door. The SQL follows the table's shape; the pins that call these keep their
// text when the shape moves.

// SeedCartRow writes one cart row for orderID: binID lifted at plan step pick
// and set down at plan step drop.
func SeedCartRow(t testing.TB, db *store.DB, orderID, binID int64, pick, drop int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO order_bins (order_id, bin_id, pick_step, drop_step)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (order_id, pick_step) DO UPDATE SET bin_id = EXCLUDED.bin_id, drop_step = EXCLUDED.drop_step`,
		orderID, binID, pick, drop); err != nil {
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

// HoldCart writes the row of the one cart orderID comes to hold, as a door
// writes it: binID lifted at the plan's first pickup (in hand from the start
// when the plan has none) and set down where the walk says (orders.CartRows).
// The order's other rows stand.
func HoldCart(t testing.TB, db *store.DB, orderID, binID int64) {
	t.Helper()
	o, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("testdb.HoldCart(order=%d): read the order: %v", orderID, err)
	}
	pick := domain.FirstPickup(domain.OrderPlan(o))
	if pick < 0 {
		pick = domain.InHand
	}
	for _, r := range orders.CartRows(o, map[int]int64{pick: binID}) {
		if _, err := db.Exec(`INSERT INTO order_bins (order_id, bin_id, pick_step, drop_step)
			VALUES ($1, $2, $3, NULLIF($4::int, -1))
			ON CONFLICT (order_id, pick_step) DO UPDATE SET bin_id = EXCLUDED.bin_id, drop_step = EXCLUDED.drop_step`,
			orderID, r.BinID, r.PickStep, r.DropStep); err != nil {
			t.Fatalf("testdb.HoldCart(order=%d bin=%d): %v", orderID, binID, err)
		}
	}
}

// OnlyCart is the one cart o carries (Order.OnlyBin), nil when it carries
// none. An order with several fails the test: the caller asked "the order's
// cart" of an order that has several, and must ask its rows instead.
func OnlyCart(t testing.TB, o *orders.Order) *int64 {
	t.Helper()
	c, err := o.OnlyBin()
	if err != nil {
		t.Fatalf("testdb.OnlyCart: %v", err)
	}
	return c
}

// Holding is o born holding cart (Order.HoldFromBirth): its row is written
// with it by Create. For fixtures that build the order in one expression.
func Holding(o *orders.Order, cart int64) *orders.Order {
	o.HoldFromBirth(cart)
	return o
}

// SeedCarts writes orderID's rows as the allocator does for the carts it
// claimed by plan step (claims[step] = bin): the plan walk over the order's
// stored plan (orders.CartRows), relays and open lifts included.
func SeedCarts(t testing.TB, db *store.DB, orderID int64, claims map[int]int64) {
	t.Helper()
	o, err := db.GetOrder(orderID)
	if err != nil {
		t.Fatalf("testdb.SeedCarts(order=%d): read the order: %v", orderID, err)
	}
	if err := db.ReplaceOrderBins(orderID, orders.CartRows(o, claims)); err != nil {
		t.Fatalf("testdb.SeedCarts(order=%d): %v", orderID, err)
	}
}
