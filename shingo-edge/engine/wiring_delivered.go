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
// authoritative bin value. The reconciler self-heal will rewrite the
// cache to the actual bin count on the next pass once Core is reachable.
// This keeps the slot accounting honest about which bin is present
// (active_bin_id / cached_bin_id) even during a Core blip — better than
// leaving the bin pointers stale-pointing at a previous bin.
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
	cacheValue := deliveredFallbackUOP(claim)
	uop, found, lookupErr := e.coreClient.BinByID(*delivered.BinID)
	switch {
	case lookupErr != nil:
		log.Printf("delivered: bin %d uop lookup failed (Core unreachable): %v — using %s fallback %d",
			*delivered.BinID, lookupErr, claim.Role, cacheValue)
	case found:
		cacheValue = uop
	default:
		// 200 + not found — bin doesn't exist at Core. Treat as empty
		// slot rather than role-default fallback.
		log.Printf("delivered: bin %d not found at Core — using 0 (empty slot)", *delivered.BinID)
		cacheValue = 0
	}
	claimID := claim.ID
	if err := e.db.SetProcessNodeRuntimeForDeliveredBin(node.ID, &claimID, *delivered.BinID, cacheValue); err != nil {
		log.Printf("delivered: set runtime for node %d bin %d: %v", node.ID, *delivered.BinID, err)
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
