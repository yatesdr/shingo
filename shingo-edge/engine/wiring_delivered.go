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
// orders (DeliveryNode is the supermarket) no-op out. Single-bin orders gate
// on DeliveryNode / steps finalDropoff == CoreNodeName; multi-tote deliveries
// (F1b) carry Core's per-bin BinDestNode and gate on that. A multi-bin
// delivery with no per-bin id resolved (BinID nil) hits the backstop alarm.
//
// This is the "physics → cache" half of the runtime UOP binding split.
// Operator-semantic events (StatusConfirmed) no longer touch the cache;
// the four completion-time SetProcessNodeRuntimeWithBin callsites in
// wiring_completion.go are removed alongside this handler's wiring.

package engine

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// handleNodeOrderDelivered binds the runtime cache to the just-arrived
// bin's authoritative uop_remaining. Gates on:
//
//   - ProcessNodeID present and resolvable.
//   - BinID present. For single-bin orders Core always carries it by delivery;
//     for multi-tote orders (F1b) Core selects the bin destined for the
//     consuming node and carries it plus BinDestNode. BinID nil on a multi-bin
//     delivery means Core resolved no bin to this node — the backstop alarm.
//   - The carried bin landed at this node: BinDestNode == CoreNodeName for
//     multi-tote; steps finalDropoff / DeliveryNode == CoreNodeName otherwise
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
		switch {
		case delivered.ProcessNodeID == nil && delivered.BinID != nil && delivered.DeliveryNode != "":
			// Core-admin order (no Edge order row): ProcessNodeID is nil but a bin
			// and destination are present. Resolve the node from DeliveryNode and
			// bind — the legitimate fallback path, not a silent skip.
			e.handleFallbackDelivered(delivered)
		case delivered.ProcessNodeID != nil && delivered.BinID == nil:
			// Multi-bin delivery whose envelope carries no per-bin id. Post-F1b
			// this is the BACKSTOP, not the primary path: Core now selects the bin
			// destined for the consuming node and ships its id + BinDestNode (bound
			// below). BinID is nil here only when Core resolved NO order_bin to this
			// process node — a genuine gap worth naming, not a routine multi-tote
			// delivery. Alarm, bind nothing.
			e.raiseDeliveredNotBound(delivered, "", "multi-bin delivery carried no bin id — no bin resolved to this node (F1b backstop)")
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
	switch {
	case delivered.BinDestNode != "":
		// F1b multi-tote: Core already selected the one bin destined for the
		// consuming node and shipped its landing node here. Trust that per-bin
		// resolution — for a multi-dropoff swap the steps finalDropoff names the
		// LAST leg (the supermarket for the evac tote), not where THIS bin came to
		// rest, so consulting it would wrongly no-op the supply bin (the SNF3
		// stranding). Bind iff the carried bin landed at the node we own.
		deliveredHere = delivered.BinDestNode == node.CoreNodeName
	case order.OrderType == protocol.OrderTypeComplex:
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
	default:
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
		// The bin landed at a node we own but there is no active claim to bind it
		// to (unpublished/mid-changeover style, orphaned node). Pre-fix this was a
		// silent no-op and the bin's ticks stranded; now it names the bin + node so
		// the operator can correct it through the front door.
		e.raiseDeliveredNotBound(delivered, node.CoreNodeName, "no active claim at node")
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

// raiseDeliveredNotBound surfaces a delivery that arrived at one of our nodes
// but did NOT bind the runtime — the silent detachment behind the SNF3
// stranding. It never changes binding behavior; it makes the skip loud. It
// writes one greppable audit line naming the exact bin/order + node + reason +
// the operator's front-door fix, and emits a structured EventDeliveredNotBound
// for the operator tile (C8) / SSE.
//
// coreNodeName may be "" (the multi-bin early return has no resolved node); in
// that case it is resolved from the envelope's ProcessNodeID, then DeliveryNode.
// The front-door instruction is uniform — "Record Count on the bin tab" — which
// P2-C5 makes actually bind a staged, unbound bin.
func (e *Engine) raiseDeliveredNotBound(delivered OrderDeliveredEvent, coreNodeName, reason string) {
	node := coreNodeName
	if node == "" && delivered.ProcessNodeID != nil {
		if n, err := e.db.GetProcessNode(*delivered.ProcessNodeID); err == nil && n != nil {
			node = n.CoreNodeName
		}
	}
	if node == "" {
		node = delivered.DeliveryNode
	}
	if node == "" {
		node = "unknown node"
	}

	// Name the subject: the carrier when we have a bin id, else the order.
	var subject string
	switch {
	case delivered.BinID != nil:
		subject = fmt.Sprintf("bin %d", *delivered.BinID)
	case delivered.OrderUUID != "":
		subject = fmt.Sprintf("order %s", delivered.OrderUUID)
	default:
		subject = fmt.Sprintf("order %d", delivered.OrderID)
	}

	const instruction = "Record Count on the bin tab to bind it"
	log.Printf("delivered but NOT bound: %s at %s — %s. %s", subject, node, reason, instruction)

	e.Events.Emit(Event{Type: EventDeliveredNotBound, Payload: DeliveredNotBoundEvent{
		OrderID:      delivered.OrderID,
		OrderUUID:    delivered.OrderUUID,
		CoreNodeName: node,
		BinID:        delivered.BinID,
		Reason:       reason,
		Instruction:  instruction,
	}})
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
