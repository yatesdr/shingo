package dispatch

import (
	"shingocore/dispatch/plan"
	binsstore "shingocore/store/bins"
	"shingocore/store/orders"
)

// binsAtStep is the slot book's first read for this order (plan.BinsAtStep):
// what this plan finds on a node when it reaches step `at`. The question, its
// four askers and which bin an earlier pickup takes are documented there, once.
// r reads the node by name: the store, or a complex attempt's one read of its
// plan (attemptNodes); what stands on it and what the order holds are read from
// the store (plan.BinsAtStepOver).
func (a *Allocator) binsAtStep(r nodeReads, order *orders.Order, steps []resolvedStep, at int, node string) (
	remaining []*binsstore.Bin, taken []int64, err error) {
	return plan.BinsAtStepOver(a.db, r, order.ID, steps, at, node)
}
