package domain

import "time"

// OrderBin is one order_bins row as a reader of the table reads it: an order's
// cart, the plan step that lifts it (PickStep) and the plan step that sets it
// down (DropStep), -1 for a step the row does not know (an open lift's drop; a
// row carried over from orders.bin_id for an order that ended before the rows
// existed, v173). Every order that carries a cart has its rows, one per lift
// (order_carts.go); where a step happens is the plan's (StepNode). An order's
// own rows come with it as Order.Carts.
type OrderBin struct {
	ID        int64     `json:"id"`
	OrderID   int64     `json:"order_id"`
	BinID     int64     `json:"bin_id"`
	PickStep  int       `json:"pick_step"`
	DropStep  int       `json:"drop_step"`
	CreatedAt time.Time `json:"created_at"`
}
