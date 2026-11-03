package dispatch

import (
	"encoding/json"
	"fmt"

	"shingocore/domain"
	"shingocore/store/orders"
)

// recordCartsForPlan writes a single-cart order's rows against plan, the plan
// it is about to persist and run, by the walk (domain.Lifts): its cart lifted at
// the plan's first pickup, or in hand from the start when the plan has none (a
// recovery's unload-only plan). One statement, and none when the rows already
// say so. An order that carries no cart writes nothing; one that carries several
// is refused (OnlyBin): its rows are the allocator's, and a splice moves them
// (reindexCartsForSplice).
func (d *Dispatcher) recordCartsForPlan(order *orders.Order, plan []resolvedStep) error {
	bin, err := order.OnlyBin()
	if err != nil {
		return err
	}
	if bin == nil {
		return nil
	}
	j, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("order %d: marshal its plan for its carts: %w", order.ID, err)
	}
	planned := *order
	planned.StepsJSON = string(j)
	pick := domain.FirstPickup(domain.OrderPlan(&planned))
	if pick < 0 {
		pick = domain.InHand
	}
	rows := orders.CartRows(&planned, map[int]int64{pick: *bin})
	if sameCarts(order.Carts, rows) {
		return nil
	}
	if err := d.db.ReplaceOrderBins(order.ID, rows); err != nil {
		return err
	}
	order.Carts = cartsOf(rows)
	return nil
}

// sameCarts reports whether an order's carts are already rows.
func sameCarts(carts []domain.Cart, rows []orders.OrderBinRow) bool {
	if len(carts) != len(rows) {
		return false
	}
	for i, r := range rows {
		c := carts[i]
		drop := orders.OpenLift
		if c.DropStep != nil {
			drop = *c.DropStep
		}
		if c.BinID != r.BinID || c.PickStep == nil || *c.PickStep != r.PickStep || drop != r.DropStep {
			return false
		}
	}
	return true
}

// cartsOf is rows as the order carries them in memory.
func cartsOf(rows []orders.OrderBinRow) []domain.Cart {
	out := make([]domain.Cart, 0, len(rows))
	for _, r := range rows {
		p, d := r.PickStep, r.DropStep
		c := domain.Cart{BinID: r.BinID, PickStep: &p}
		if d != orders.OpenLift {
			c.DropStep = &d
		}
		out = append(out, c)
	}
	return out
}

// materialCart is the cart whose payload is the order's material: its one cart,
// or of several the one its plan sets down last at its process node, else the
// first it lifts. nil when it carries none.
func materialCart(order *orders.Order) *int64 {
	finals := order.FinalDrops()
	if len(finals) == 0 {
		if only, err := order.OnlyBin(); err == nil {
			return only
		}
		return nil
	}
	for _, c := range finals {
		if order.ProcessNode != "" && domain.StepNode(order, *c.DropStep) == order.ProcessNode {
			b := c.BinID
			return &b
		}
	}
	b := finals[0].BinID
	return &b
}

// cartLiftedAt is the cart the order lifts at node (its row whose pick step is
// there), nil when its plan lifts none there.
func cartLiftedAt(order *orders.Order, node string) *int64 {
	for _, c := range order.Carts {
		if c.PickStep != nil && *c.PickStep >= 0 && node != "" && domain.StepNode(order, *c.PickStep) == node {
			b := c.BinID
			return &b
		}
	}
	return nil
}
