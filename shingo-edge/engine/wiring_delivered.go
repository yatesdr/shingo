// wiring_delivered.go — OrderDelivered handler.
//
// Subscribed via wireEventHandlers (wiring.go) on EventOrderDelivered,
// which fires the moment an order transitions to StatusDelivered (a bin
// physically arrived at its destination node — robot has dropped it).
//
// One handler, one rule, no role/mode dispatch. When the destination
// matches a process_node we own, the runtime cache (active_bin_id,
// active_bin_epoch, remaining_uop_cached) flips to the delivered bin's
// authoritative UOP carried on the OrderDelivered envelope. Removal-shaped
// orders (DeliveryNode is the supermarket) and multi-bin orders (BinID
// nil at the envelope) no-op out — DeliveryNode == CoreNodeName is the gate.
//
// This is the "physics → cache" half of the runtime UOP binding split.
// Operator-semantic events (StatusConfirmed) no longer touch the cache;
// the four completion-time SetProcessNodeRuntimeWithBin callsites in
// wiring_completion.go are removed alongside this handler's wiring.

package engine

import (
	"encoding/json"
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
		// Fallback for Core-admin orders (no Edge order row): ProcessNodeID is nil.
		// Resolve the process node from DeliveryNode if present.
		if delivered.BinID != nil && delivered.DeliveryNode != "" {
			e.handleFallbackDelivered(delivered)
		}
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
	// Did the bin actually land at this process node? Simple orders carry the
	// destination in DeliveryNode. Complex orders (press swaps, etc.) leave it
	// blank — their per-leg destinations live in steps_json — so for a complex
	// order we check that its final dropoff step targets this node. This keeps
	// the removal-shaped filter intact (a leg ending at a supermarket has a
	// final dropoff != this node, so it still no-ops) while letting a swap that
	// delivers a fresh bin to a producing cell bind the active bin. Without it,
	// every complex delivery to a producing node failed to bind active_bin_id
	// and PLC ticks parked in pending_uop_delta forever (HK PLN_04, 2026-06-17).
	deliveredHere := order.DeliveryNode == node.CoreNodeName
	if !deliveredHere && order.DeliveryNode == "" && order.OrderType == protocol.OrderTypeComplex {
		if stepsJSON, sErr := e.db.GetOrderStepsJSON(order.ID); sErr != nil {
			log.Printf("delivered: order %d — cannot load steps to resolve complex destination: %v", order.ID, sErr)
		} else {
			deliveredHere = finalDropoffNode(stepsJSON) == node.CoreNodeName
		}
	}
	if !deliveredHere {
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

	// Auto-clear: if this was a pull-from-market delivery, zero the bin UOP
	// immediately so the operator doesn't need to hit a separate Clear Bin button.
	e.marketPullbacksMu.Lock()
	_, isPullback := e.marketPullbacks[order.UUID]
	if isPullback {
		delete(e.marketPullbacks, order.UUID)
	}
	e.marketPullbacksMu.Unlock()
	if isPullback {
		if err := e.coreClient.ClearBin(node.CoreNodeName, ""); err != nil {
			log.Printf("market_pullback: auto-clear bin at %s: %v", node.CoreNodeName, err)
		} else {
			log.Printf("market_pullback: auto-cleared bin at %s on delivery", node.CoreNodeName)
			if e.inventoryDelta != nil {
				_ = e.inventoryDelta.SetClaimAndCount(node.ID, &claimID, 0)
			}
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

// handleFallbackDelivered binds the runtime cache for Core-admin orders that
// have no Edge row (ProcessNodeID is nil). The delivery node is looked up by
// Core dot-name; if it maps to an Edge process node that has an active claim,
// the cache and active_bin_id are updated exactly as for a normal delivery.
func (e *Engine) handleFallbackDelivered(delivered OrderDeliveredEvent) {
	node, err := e.db.GetProcessNodeByCoreNodeName(delivered.DeliveryNode)
	if err != nil || node == nil {
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
	if delivered.BinUOP != nil {
		cacheValue = *delivered.BinUOP
	}
	claimID := claim.ID
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.OnDelivered(node.ID, &claimID, *delivered.BinID, delivered.BinEpoch, cacheValue); err != nil {
			log.Printf("delivered fallback: node %s bin %d: %v", delivered.DeliveryNode, *delivered.BinID, err)
		}
	}
}

// finalDropoffNode returns the node of the last "dropoff" step in a complex
// order's step list, or "" if the steps can't be parsed or contain no dropoff.
// Complex orders don't populate Order.DeliveryNode (their destinations live in
// steps_json), so the final dropoff is how the delivery handler learns where
// the bin actually came to rest.
func finalDropoffNode(stepsJSON string) string {
	if stepsJSON == "" {
		return ""
	}
	var steps []protocol.ComplexOrderStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return ""
	}
	dest := ""
	for _, s := range steps {
		if s.Action == protocol.ActionDropoff && s.Node != "" {
			dest = s.Node
		}
	}
	return dest
}
