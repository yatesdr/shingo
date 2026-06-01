// wiring_delivered.go — OrderDelivered handler.
//
// Subscribed via wireEventHandlers (wiring.go) on EventOrderDelivered,
// which fires the moment an order transitions to StatusDelivered (a bin
// physically arrived at its destination node — robot has dropped it).
//
// One handler, one rule, no role/mode dispatch. When the destination
// matches a process_node we own, the runtime cache (active_bin_id,
// cached_bin_id, remaining_uop_cached) flips to the delivered bin's
// authoritative UOP from Core. Removal-shaped orders (DeliveryNode is
// the supermarket) and multi-bin orders (BinID nil at the envelope)
// no-op out — DeliveryNode == CoreNodeName is the gate.
//
// This is the "physics → cache" half of the runtime UOP binding split.
// Operator-semantic events (StatusConfirmed) no longer touch the cache;
// the four completion-time SetProcessNodeRuntimeWithBin callsites in
// wiring_completion.go are removed alongside this handler's wiring.

package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// handleNodeOrderDelivered binds the runtime cache to the just-arrived
// bin's authoritative uop_remaining. Gates on:
//
//   - ProcessNodeID present and resolvable.
//   - BinID present (multi-bin orders defer to the bucket-delta path).
//   - Order's DeliveryNode equals the process node's CoreNodeName
//     (removal-shaped orders flow through this event too — Order B in
//     two-robot consume delivers to the supermarket — but their slot
//     accounting is owned by the supply leg's delivery, not theirs).
//
// Core-unreachable fallback: the cache + bin pointers still get written,
// but with claim.UOPCapacity (consume) / 0 (produce) instead of the
// looked-up bin value. Post-flip (6d226d1) Edge is authoritative for
// at-node bins; there is no reconciler to rewrite the fallback value
// when Core comes back. The fallback is bounded — subsequent PLC ticks
// emit signed deltas that Core applies to whatever value its row holds,
// so arithmetic stays consistent even if Edge's initial cache value
// disagreed with Core. Operator UI may display the fallback value
// briefly; this is the accepted bias (see Risk: Gap A in the refactor
// plan / architecture doc).
func (e *Engine) handleNodeOrderDelivered(delivered OrderDeliveredEvent) {
	if delivered.ProcessNodeID == nil || delivered.BinID == nil {
		return
	}
	order, err := e.db.GetOrder(delivered.OrderID)
	if err != nil {
		return
	}
	node, err := e.db.GetProcessNode(*delivered.ProcessNodeID)
	if err != nil {
		return
	}
	if order.DeliveryNode != node.CoreNodeName {
		return
	}
	if _, err := e.db.EnsureProcessNodeRuntime(node.ID); err != nil {
		return
	}
	claim := findActiveClaim(e.db, node)
	if claim == nil {
		return
	}
	// Seed the runtime cache + epoch from the snapshot Core stamped on the
	// OrderDelivered envelope (taken at the bin's arrival, carried on the
	// same Kafka message). No HTTP pull — the seed and epoch ride the
	// delivery event itself, so this works even when Core's HTTP API is
	// momentarily unreachable. BinUOP nil means an older Core didn't send
	// a snapshot; fall back to the role default.
	cacheValue := deliveredFallbackUOP(claim)
	if delivered.BinUOP != nil {
		cacheValue = *delivered.BinUOP
	} else {
		log.Printf("delivered: bin %d — no uop snapshot on envelope (older Core?), using %s fallback %d",
			*delivered.BinID, claim.Role, cacheValue)
	}
	claimID := claim.ID
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.OnDelivered(node.ID, &claimID, *delivered.BinID, delivered.BinEpoch, cacheValue); err != nil {
			log.Printf("delivered: set runtime for node %d bin %d: %v", node.ID, *delivered.BinID, err)
		}
	}
}

// deliveredFallbackUOP returns the cache value to use when Core is
// unreachable: produce nodes start at 0 (filling up), other roles
// fall back to claim capacity (full bin assumption). Mirrors the
// pre-refactor resolveReplenishUOP defaults.
func deliveredFallbackUOP(claim *processes.NodeClaim) int {
	if claim.Role == protocol.ClaimRoleProduce {
		return 0
	}
	return claim.UOPCapacity
}
