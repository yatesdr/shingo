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
	// Did the bin actually land at this process node?
	//
	// Only single-bin orders reach here (BinID nil returned above), so a complex
	// order's ONE bin ends at its LAST dropoff step — unambiguous, and the only
	// thing that answers "where did this bin land". Resolve it from steps_json.
	//
	// order.DeliveryNode is NOT usable for a complex order and must not be
	// consulted. A complex order has many dropoffs, so a single per-order
	// destination field is lossy by construction; worse, Edge stamps swap legs
	// with the order's PROCESS node (swap_dispatch.go DeliveryNodeA), which for a
	// press-index R1 leg names the press while the bin it carries is staged at the
	// paired index node. This gate used to short-circuit on that field and only
	// fall back to steps_json when it was blank — which the swap path guarantees it
	// is not, making the correct branch dead code. At HK on 2026-07-14 that bound
	// an EMPTY tote (0 UOP, landed at PLN_02) to PLN_01's runtime, and the press
	// tile read 0/10560 while the bin physically on it held 850.
	//
	// The removal-shaped filter is preserved: a leg ending at a supermarket has a
	// final dropoff != this node, so it still no-ops.
	var deliveredHere bool
	if order.OrderType == protocol.OrderTypeComplex {
		stepsJSON, sErr := e.db.GetOrderStepsJSON(order.ID)
		if sErr != nil {
			log.Printf("delivered: order %d — cannot load steps to resolve complex destination: %v", order.ID, sErr)
			return
		}
		dest := finalDropoffNode(stepsJSON)
		if dest == "" {
			// createComplexOrder always persists steps, so this is unreachable in
			// practice — say so rather than silently never binding the node's bin,
			// because the symptom (ticks piling up in pending_uop_delta) is miles
			// from the cause.
			log.Printf("delivered: order %d (complex) has no resolvable final dropoff — steps missing or dropoff-less; runtime cache NOT bound for node %s", order.ID, node.CoreNodeName)
			return
		}
		deliveredHere = dest == node.CoreNodeName
	} else {
		deliveredHere = order.DeliveryNode == node.CoreNodeName
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
// A complex order carries its destinations in steps_json, and only single-bin
// orders reach the delivery gate — so the final dropoff is exactly where that
// bin came to rest. Decodes and defers to finalDropoff, the same helper the
// swap-dispatch producer uses, so the two can't drift apart on what a leg's
// destination means.
func finalDropoffNode(stepsJSON string) string {
	if stepsJSON == "" {
		return ""
	}
	var steps []protocol.ComplexOrderStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return ""
	}
	return finalDropoff(steps)
}
