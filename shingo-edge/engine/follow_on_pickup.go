package engine

import (
	"slices"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/orders"
)

// followOnOnPickup is Robot 3's one trigger (owner ruling 3): a leg of a pair
// has lifted the carrier off a position behind an index press's front that no
// leg of the pair sets down on, so that position is owed its next carrier, and
// this makes it (carrierToPosition: a plain retrieve naming the press, confirmed
// by its placement), attached to the lifting leg's own origin, which the pair's
// plan counted (FollowOns).
//
// THE STEPS DECIDE, NOT THE SETTING. Whether the back is owed is read from the
// pair's stored steps (followOnPositions over the lifting leg and its sibling),
// so a refill setting edited between the dispatch and the lift cannot fill the
// back twice or starve it. Only a multi-step leg and only a position behind the
// front: a clear's lift, SEND BACK's move and a sequential removal (its front
// has its backfill) are none of this.
//
// ONCE. Under the cell's prime lock, and only when nothing already places a
// carrier there (PlacingAt; unreadable counts as placing), so a second pickup
// message or a REQUEST's prime cannot double it. The lock is tried, never
// waited for (keepSpot's reason: this runs in the message handler, and a held
// lock is a request, a start or a cancel deciding the cell now). A miss is
// logged, and the back is primed by the next REQUEST, as a cancelled or failed
// follow-on is.
//
// The claim is read target first, as an order that does not exist yet is
// (orders.Manager): mid-changeover the carrier is the incoming style's.
func (e *Engine) followOnOnPickup(order *domain.Order, location string) {
	if order == nil || order.ProcessNodeID == nil || location == "" || order.OrderType != protocol.OrderTypeComplex {
		return
	}
	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil || node == nil {
		return
	}
	process, err := e.db.GetProcess(node.ProcessID)
	if err != nil || process == nil {
		return
	}
	claim := e.db.ResolveNodeClaim(process, node, store.TargetStyleFirst)
	if claim == nil || !slices.Contains(claim.DependentPositions(), location) {
		return
	}
	legs := e.pairSteps(order)
	if len(followOnPositions([]string{location}, legs...)) == 0 {
		return
	}
	mu := e.primeNodeLock(claim)
	if !mu.TryLock() {
		e.logFn("follow-on: order %d lifted %s, but %s's cell is being decided (its lock is held) — "+
			"no carrier made; the next REQUEST primes %s", order.ID, location, claim.CoreNodeName, location)
		return
	}
	defer mu.Unlock()
	placing, err := e.db.LiveOrderPlacing(location)
	if err != nil {
		e.logFn("follow-on: order %d lifted %s: what is coming there cannot be read (%v) — no carrier made",
			order.ID, location, err)
		return
	}
	if placing[location] != orders.PlacingNone {
		e.debugFn("follow-on: order %d lifted %s; a carrier is already coming there", order.ID, location)
		return
	}
	made, err := e.carrierToPosition(node.ID, claim, location, 1, e.liftingLegOrigin(order))
	if err != nil {
		e.logFn("follow-on: order %d lifted %s: the next carrier could not be ordered: %v", order.ID, location, err)
		return
	}
	e.logFn("follow-on: order %d lifted %s at %s; order %d brings it its next carrier (Robot %d)",
		order.ID, location, claim.CoreNodeName, made.ID, claim.RefillBy.RobotNumber())
}

// pairSteps is a leg's stored steps and its sibling's, the pair whose set-downs
// followOnPositions reads. A leg or sibling whose steps cannot be read adds
// none: a set-down it might have made is then unseen, and only a position no
// readable leg places on is owed.
func (e *Engine) pairSteps(order *domain.Order) [][]protocol.ComplexOrderStep {
	ids := []int64{order.ID}
	if order.SiblingOrderID != nil {
		ids = append(ids, *order.SiblingOrderID)
	}
	var legs [][]protocol.ComplexOrderStep
	for _, id := range ids {
		raw, err := e.db.GetOrderStepsJSON(id)
		if err != nil {
			continue
		}
		if steps, err := decodeSteps(raw); err == nil {
			legs = append(legs, steps)
		}
	}
	return legs
}
