// sequential_backfill.go — a sequential position's Order B, the backfill.
//
// A sequential position is served by two robots one after the other. Order A
// drives to the line, waits for the station's RELEASE, lifts the old bin and
// takes it away; Order B brings the new one. B is created when the station's
// RELEASE of A at the line node goes out (ensureSequentialBackfill, from the
// release act's commit), not before: created earlier, it reached the line with
// a full bin and stood behind A until A left, and a pair parked at an idle line
// was never sent back.
//
// A release that went out while the Edge then stopped, before B was created,
// is found again at start (recoverSequentialBackfills).

package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/release"
	"shingoedge/store/orders"
)

// ensureSequentialBackfill creates Order B for a sequential position once its
// Order A, the position's active order, has been released at the line node,
// and only if no B exists yet. Called for each leg whose release went out
// (releaseAct) and, once per start, for each position with an active order and
// no B (recoverSequentialBackfills). Anything else returns with nothing made.
func (e *Engine) ensureSequentialBackfill(orderID int64) {
	order, err := e.db.GetOrder(orderID)
	if err != nil || order.ProcessNodeID == nil || protocol.IsTerminal(order.Status) {
		return
	}
	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil {
		return
	}
	runtime, err := e.db.EnsureProcessNodeRuntime(node.ID)
	if err != nil {
		return
	}

	// Only act on the active order (Order A) for this node
	if runtime.ActiveOrderID == nil || *runtime.ActiveOrderID != order.ID {
		return
	}
	// Don't create backfill if one already exists
	if runtime.StagedOrderID != nil {
		return
	}

	claim := e.claimAtNode(node)
	if claim == nil || claim.SwapMode != protocol.SwapModeSequential {
		return
	}
	planA, ok := e.orderSteps(order)
	if !ok || !releasedAtLine(order, planA, claim.CoreNodeName) {
		return // A has not been released at the line yet
	}

	// ── ONE POSITION, ONE INBOUND CARRIER ─────────────────────────────────────
	//
	// The backfill exists because the steady-state Order A only REMOVES: it
	// lifts the full bin out and brings nothing back, so without Order B the
	// position never gets a fresh carrier. A changeover's Order A is the other
	// shape — it drops the old bin at the outbound destination, fetches the new
	// style's carrier and returns it to this same position. Minting Order B
	// there sends a second robot with a second carrier to a slot that already
	// has one coming, and Order B is two steps against Order A's five: B wins,
	// A returns to an occupied position, and holdForPosition holds forever
	// ("a robot cannot place onto an occupied position"). The carrier B left is
	// claimed by nobody, so nothing reclaims it either. 8 of 8 demo runs across
	// two trees, and the same wedge on 2026-08-28 under a wrong attribution.
	//
	// The guard reads what Order A DOES, not whether a changeover is running.
	// A changeover test would have to be kept in step with the builders by hand
	// and would say nothing about the tooling-evacuate variant, which has the
	// same self-refilling tail.
	//
	// FAIL-CLOSED, deliberately, and the two outcomes are not symmetric: an
	// unreadable plan means "cannot prove Order A brings nothing back", and
	// skipping the mint costs a position that waits for material and an operator
	// who can press REQUEST, while minting costs a deadlock that needed the
	// stack torn down. Same disposition as the cutover gate, for the same reason.
	if e.orderRefillsNodeItself(order, claim.CoreNodeName) {
		log.Printf("sequential backfill: no Order B for node %s — Order A %d already brings %s its replacement",
			node.Name, order.ID, claim.CoreNodeName)
		return
	}

	steps := BuildSequentialBackfillSteps(claim)
	nodeID := node.ID
	// ATTRIBUTED to the demand that produced Order A, whose plan counted this
	// order as its follow-on (SwapDispatch.FollowOns): it carries Order A's own
	// origin (liftingLegOrigin). It joined the cell's open episode, which
	// counted it again as the demand asked a second time: one rerequest on
	// every healthy cycle. See cellEpisodeOrigin for what an unattributed order
	// costs downstream.
	orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, claim.CoreNodeName, claim.CoreNodeName, steps,
		e.liftingLegOrigin(order)) // delivery_node = CoreNodeName → resets UOP
	if err != nil {
		log.Printf("sequential backfill for node %s: %v", node.Name, err)
		return
	}
	if err := e.db.SetProcessNodeRuntimeStagedOrder(nodeID, &orderB.ID); err != nil {
		log.Printf("update runtime orders for node %d: %v", nodeID, err)
	}
	// LinkOrderSiblings is log-and-continue here (unlike the three
	// operator-initiated sites which return-error). Rationale:
	//   - This runs in the OrderStatusChanged event handler loop; one
	//     handler failing must not abort message processing.
	//   - The backfill is opportunistic; if linkage fails, the L1/L2
	//     side-cycle still works for the operator (only the consolidated
	//     swap_ready RELEASE coordinates via siblings, and sequential
	//     mode is excluded from that path by the SwapMode gate).
	if err := e.db.LinkOrderSiblings(order.ID, orderB.ID); err != nil {
		log.Printf("link sequential siblings %d↔%d: %v", order.ID, orderB.ID, err)
	}
	log.Printf("sequential backfill: created Order B %d for node %s (Order A %d released at the line)", orderB.ID, node.Name, order.ID)
}

// releasedAtLine reports whether an order's release of the station wait in
// front of its lift at line has gone out: its sent intent is for that wait or
// a later one, or Core has since staged it at a later station wait (which
// consumes the intent, release.Intent.OnStaged). The wait is the last station
// wait at line before the first lift there; a plan with none answers false.
func releasedAtLine(o *orders.Order, steps []protocol.ComplexOrderStep, line string) bool {
	at, waits := -1, 0
	for _, s := range steps {
		if s.Action == protocol.ActionWait && protocol.IsStationWaitKind(s.WaitKind) {
			if s.Node == line {
				at = waits
			}
			waits++
			continue
		}
		if s.Action == protocol.ActionPickup && s.Node == line {
			break
		}
	}
	if at < 0 {
		return false
	}
	if in, err := release.DecodeIntent(o.ReleaseIntent); err == nil && in != nil && in.Sent() && in.StationWait >= at {
		return true
	}
	return o.StationWait != nil && *o.StationWait > at
}

// recoverSequentialBackfills is ensureSequentialBackfill for every position
// with an active order and no Order B, once at start: the release of a
// sequential Order A went out and the Edge stopped before B was created. Its
// cost is the start's only: the positions, and one runtime row each.
func (e *Engine) recoverSequentialBackfills() {
	nodes, err := e.db.ListProcessNodes()
	if err != nil {
		e.logFn("sequential backfill: read the positions at start: %v", err)
		return
	}
	for i := range nodes {
		rt, err := e.db.GetProcessNodeRuntime(nodes[i].ID)
		if err != nil || rt == nil || rt.ActiveOrderID == nil || rt.StagedOrderID != nil {
			continue
		}
		e.ensureSequentialBackfill(*rt.ActiveOrderID)
	}
}
