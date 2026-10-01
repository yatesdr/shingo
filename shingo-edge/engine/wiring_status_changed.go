// wiring_status_changed.go — handlers subscribed to EventOrderStatusChanged.
//
// Two handlers:
//   handleSequentialBackfill    – auto-create Order B (backfill) when
//                                 Order A enters in_transit on a sequential
//                                 swap-mode node.
//   handleSiblingReleaseRefire  – fire a two-robot swap leg's release the
//                                 moment it reaches staged, when that leg was
//                                 deferred by ReleaseStagedOrders (Core would
//                                 have refused it) while its sibling released
//                                 on the same operator click (hop A4-ii). Falls
//                                 through to releaseSurvivorOfFinishedPartner
//                                 when the in-memory deferral is not there to
//                                 find — the durable half of the same question.
//
// Wired by wireEventHandlers (wiring.go).
//
// History: handleAutoReleaseOnStaged was removed 2026-04-27 along with the
// auto-release coordination layer, on the theory that ReleaseStagedOrders
// fanning out to both legs unconditionally made a late-sibling auto-release
// unnecessary. The Hopkinsville press-index hang (2026-07-23) showed that
// unconditional fan-out desyncs a not-yet-releasable leg instead; hop A4-i
// makes the fan-out skip such a leg, and handleSiblingReleaseRefire is the
// TARGETED revival of the removed hook — scoped to a leg whose sibling already
// released, it re-fires (never cancels, never re-plans, no timer).

package engine

import (
	"log"

	"shingo/protocol"
)

// handleSequentialBackfill watches for sequential Order A going in_transit
// and auto-creates Order B (backfill) to deliver replacement material.
func (e *Engine) handleSequentialBackfill(changed OrderStatusChangedEvent) {
	if changed.NewStatus != string(protocol.StatusInTransit) || changed.ProcessNodeID == nil {
		return
	}
	order, err := e.db.GetOrder(changed.OrderID)
	if err != nil || order.ProcessNodeID == nil {
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
	// ATTRIBUTED, and it was not. This order is the plant continuing to serve the
	// demand that produced Order A, so it belongs to that demand's episode — see
	// cellEpisodeOrigin for what an unattributed one costs downstream. It JOINS
	// and never mints: a backfill is never itself the origin of a demand.
	orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, claim.CoreNodeName, claim.CoreNodeName, steps,
		e.cellEpisodeOrigin(node, claim)) // delivery_node = CoreNodeName → resets UOP
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
	log.Printf("sequential backfill: created Order B %d for node %s (Order A %d in_transit)", orderB.ID, node.Name, order.ID)
}
