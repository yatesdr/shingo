package dispatch

import (
	"shingocore/dispatch/plan"
	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
)

// binsAtStep is the slot book's first read for this order (plan.BinsAtStep):
// what this plan finds on a node when it reaches step `at`. The question, its
// four askers and which bin an earlier pickup takes are documented there, once.
func (a *Allocator) binsAtStep(order *orders.Order, steps []resolvedStep, at int, node string) (
	remaining []*binsstore.Bin, taken []int64, err error) {
	return plan.BinsAtStep(a.db, order.ID, steps, at, node)
}
