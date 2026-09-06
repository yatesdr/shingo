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
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// legLeftNoBinHereByDesign reports whether the delivered leg's own steps say it
// was never going to leave a bin at the process node the order names. That is
// the ordinary shape of a press-index R1 — it carries two bins and sets both
// down elsewhere — and a nil BinID on such a leg is the plan working, not a
// gap.
//
// EVERY UNCERTAIN ANSWER IS false, so the alarm still fires. A node we cannot
// read, steps we cannot load or decode, and an order whose steps are absent or
// empty (a simple order, where a missing bin id IS the SNF3-shaped gap) all
// mean we do not know whether a bin was due here — and a check that cannot
// tell must not report "nothing to see". The empty-list case is the sharp one:
// `null` and `[]` decode without error and legPlacesBinAt answers false for
// them, which is indistinguishable from a real evac leg unless it is caught
// here.
func (e *Engine) legLeftNoBinHereByDesign(delivered OrderDeliveredEvent) bool {
	node, err := e.db.GetProcessNode(*delivered.ProcessNodeID)
	if err != nil {
		return false
	}
	stepsJSON, err := e.db.GetOrderStepsJSON(delivered.OrderID)
	if err != nil {
		return false
	}
	steps, err := decodeSteps(stepsJSON)
	if err != nil || len(steps) == 0 {
		// An EMPTY step list is not a leg that places nothing — it is a leg we
		// know nothing about. `null` and `[]` both decode without error, and
		// legPlacesBinAt answers false for them, which reads identically to a
		// genuine evac leg. Alarm.
		return false
	}
	if legPlacesBinAt(steps, node.CoreNodeName) {
		return false
	}
	e.debugFn("delivered: order %d leg places no bin at %s (steps say so) — nil bin id is the plan, not a gap; no alarm",
		delivered.OrderID, node.CoreNodeName)
	return true
}

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
// Countless delivery: the cache + bin pointers still get written, with 0.
// This used to write claim.UOPCapacity for a consume claim — a full bin
// nobody had counted — and the direction is the whole point: a seeded
// capacity reads as a fed node and suppresses replenishment, which is the
// ALN_007 failure, while a zero costs at most one ask the next tick
// withdraws. Post-flip (6d226d1) Edge is authoritative for at-node bins and
// no reconciler rewrites the seed, so the correction comes from the first
// PLC tick or operator count. Subsequent ticks emit signed deltas that Core
// applies to whatever its own row holds, so the arithmetic stays consistent
// either way. See blindDeliverySeed for the reader this does NOT fix.
func (e *Engine) handleNodeOrderDelivered(delivered OrderDeliveredEvent) {
	if delivered.ProcessNodeID == nil || delivered.BinID == nil {
		switch {
		case delivered.ProcessNodeID == nil && delivered.BinID != nil && delivered.DeliveryNode != "":
			// No Edge process node on the order, but a bin and a destination name
			// are present. Two shapes land here: a Core-admin order with no Edge
			// order row at all, and an Edge order whose row simply carries no
			// process_node_id (a manual move order is created against a Core node
			// name). Both resolve by name — the legitimate fallback, not a skip.
			e.handleFallbackDelivered(delivered)
		case delivered.ProcessNodeID != nil && delivered.BinID == nil:
			// Multi-bin delivery whose envelope carries no per-bin id. Post-F1b
			// this is the BACKSTOP, not the primary path: Core now selects the bin
			// destined for the consuming node and ships its id + BinDestNode (bound
			// below). BinID is nil here only when Core resolved NO order_bin to this
			// process node — a genuine gap worth naming, not a routine multi-tote
			// delivery. Alarm, bind nothing.
			//
			// UNLESS THE LEG WAS NEVER GOING TO LEAVE A BIN HERE. A press-index R1
			// picks the full tote off the press and sets its two carried bins down
			// at OUT and at the index position — neither of them at the process
			// node. Core is right to ship BinID nil, and this alarm then fires on
			// every produce swap the plant runs, telling the operator to "Record
			// Count to bind" a bin that was never coming. Ask the steps whether a
			// bin was due here at all, and alarm only when one was and none
			// resolved — the shape the backstop was actually written for.
			if e.legLeftNoBinHereByDesign(delivered) {
				return
			}
			e.raiseDeliveredNotBound(delivered, "", "multi-bin delivery carried no bin id — no bin resolved to this node (F1b backstop)")
		default:
			// NOTHING matched, and before this arm existed that meant a silent
			// return: the bin bound nowhere and not one line was written about it.
			// That is exactly how HK 2026-07-28 happened — two move orders with no
			// process_node_id and (then) no DeliveryNode matched neither case, so
			// two presses counted into pending_uop_delta against no bin with zero
			// diagnostics. Any unmatched combination is a real gap; say so.
			e.raiseDeliveredNotBound(delivered, "",
				"delivery matched no bind path (process_node_id and delivery_node both absent, or bin id missing) — nothing bound")
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
	claim := requestedClaimAtNode(e.db, node)
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
	// momentarily unreachable.
	cacheValue := 0
	if delivered.BinUOP != nil {
		cacheValue = *delivered.BinUOP
	} else {
		cacheValue = blindDeliverySeed(e, delivered, node.CoreNodeName, claim.Role)
	}
	claimID := claim.ID
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.OnDelivered(node.ID, &claimID, *delivered.BinID, delivered.BinEpoch, cacheValue); err != nil {
			log.Printf("delivered: set runtime for node %d bin %d: %v", node.ID, *delivered.BinID, err)
		}
	}
	// WHAT THIS CARRIER IS, from Core, alongside the claim that says what was
	// wanted. The claim id above is the requested identity — it comes from
	// requestedClaimAtNode, which reads the process's active style — and it is right
	// only while the two agree. This is the fact itself.
	//
	// nil means an older Core sent no payload. Leaving the previous value
	// standing would be worse than the gap: a stale identity is a confident
	// wrong answer, and this field's readers fail open on an empty one.
	e.recordDeliveredCarrier(node, delivered)

	// Auto-clear: if this was a pull-from-market delivery, zero the bin UOP
	// immediately so the operator doesn't need to hit a separate Clear Bin button.
	e.marketPullbacksMu.Lock()
	_, isPullback := e.marketPullbacks[order.UUID]
	if isPullback {
		delete(e.marketPullbacks, order.UUID)
	}
	e.marketPullbacksMu.Unlock()
	if isPullback {
		cleared, err := e.coreClient.ClearBin(node.CoreNodeName, "")
		if err != nil {
			log.Printf("market_pullback: auto-clear bin at %s: %v", node.CoreNodeName, err)
		} else {
			log.Printf("market_pullback: auto-cleared bin at %s on delivery", node.CoreNodeName)
			if e.inventoryDelta != nil {
				// The stamp too: the auto-clear started this carrier's next life.
				_ = e.inventoryDelta.SetClaimCountAndEpoch(node.ID, &claimID, 0, cleared.BinID, cleared.DeltaEpoch)
			}
		}
	}
}

// recordDeliveredCarrier tells the node what Core says just landed on it.
//
// A nil payload is an older Core that does not send one, and it records as an
// UNKNOWN carrier rather than being skipped — leaving the previous occupant's
// identity standing would be a confident wrong answer about this one.
func (e *Engine) recordDeliveredCarrier(node *processes.Node, delivered OrderDeliveredEvent) {
	carrier := domain.UnknownCarrier()
	if delivered.BinPayloadCode != nil {
		carrier = domain.KnownCarrier(domain.LinesidePayloadCode(*delivered.BinPayloadCode))
	}
	e.recordLinesideCarrier(node.ID, node.CoreNodeName, carrier, domain.CarrierFromDelivery)
}

// BlindDeliveryMarker prefixes the SHOULD-BE-ZERO line: a bin arrived and the
// envelope carried no count for it, so the Edge seated 0 without knowing what
// is in the carrier.
//
// Named here so the emitter and whatever counts it share one definition rather
// than two string literals that drift — the same reason service.BurialBypassMarker
// exists. A counter must not quote the marker in its own summary line, or it
// counts itself.
const BlindDeliveryMarker = "BLIND DELIVERY"

// blindDeliverySeed is the count for a carrier that arrived with none: zero,
// and a loud line saying so.
//
// IT USED TO INVENT A FULL BIN. For a consume claim this returned
// claim.UOPCapacity — a policy number written into the field Core reads as a
// measurement, which is the same shape as the changeover capacity seed that was
// deleted from SwitchNode for saying a starved node was full. This one says it
// about a carrier nobody has counted at all.
//
// THE FAILURE DIRECTION IS CHOSEN, NOT INHERITED. Neither number is true, so
// the question is which way to be wrong. A capacity seed reads as a full node
// and SUPPRESSES replenishment — the ALN_007 direction, where the line runs dry
// while the system believes it is fed. Zero reads as a starved node and costs
// at most one replenishment ask that the next tick or count withdraws. An extra
// ask is recoverable; a suppressed one is the incident.
//
// WHAT THIS DOES NOT FIX: zero is still a value standing in for "nobody told
// me", and one reader spends it — evacDispositionForTask sends a changeover
// evac as release_empty on a zero count, clearing the manifest. The count has
// no known-bit the way the carrier's identity does (LinesidePayloadKnown), so
// an unmeasured carrier and a measured-empty one are the same integer here. The
// log line below is the only thing that tells them apart today.
//
// Reached only when Core named a bin and then could not read its row — Core
// logs that failure too ("uop/epoch lookup failed"), so the pair brackets the
// window from both ends.
//
// THIS LINE HAS A READER. It is counted by the post-integration data pass,
// which is the only way we learn whether the exposure above is theoretical: a
// blind delivery FOLLOWED BY a changeover evac on the same node is the one
// sequence that turns an unknown count into a physical mistake, and neither
// half is visible without this marker. The expected count is zero. A non-zero
// count is not itself a fault — it means Core's bin reads are failing and the
// window is live, which is a different investigation with a known first step.
func blindDeliverySeed(e *Engine, delivered OrderDeliveredEvent, coreNodeName string, role protocol.ClaimRole) int {
	binID := int64(0)
	if delivered.BinID != nil {
		binID = *delivered.BinID
	}
	e.logFn("%s: bin %d at %s (%s) — Core sent NO count on the envelope; "+
		"seating 0 rather than assuming a full carrier. The node will read starved "+
		"until the first PLC tick or operator count corrects it, and may ask for "+
		"material it does not need in the meantime.",
		BlindDeliveryMarker, binID, coreNodeName, role)
	return 0
}

// handleFallbackDelivered binds the runtime cache for Core-admin orders that
// have no Edge row (ProcessNodeID is nil). The delivery node is looked up by
// Core dot-name; if it maps to an Edge process node that has an active claim,
// the cache and active_bin_id are updated exactly as for a normal delivery.
//
// EXACTLY AS FOR A NORMAL DELIVERY INCLUDES THE CARRIER'S IDENTITY, and it did
// not. This path bound a claim resolved from the process's active style and
// recorded nothing about what actually landed, so a Core-admin straight-drop
// during a changeover produced the incident's mechanism plus no identity record
// to correct it with. The envelope carried the payload the whole time; the
// fallback emit dropped it on the way through the orders package.
func (e *Engine) handleFallbackDelivered(delivered OrderDeliveredEvent) {
	node, err := e.db.GetProcessNodeByCoreNodeName(delivered.DeliveryNode)
	if err != nil || node == nil {
		// NOT an alarm. Most deliveries that reach here land at a supermarket,
		// staging, or empty-tote node that is not an Edge process node at all —
		// "we don't own this destination" is the common, correct answer and
		// alarming on it would bury the real ones. Debug only.
		e.logFn("delivered fallback: %s is not an Edge process node — nothing to bind", delivered.DeliveryNode)
		return
	}
	// Past this point the bin landed at a node we DO own, so every remaining exit
	// is a genuine failure to bind and must be loud. These were silent returns
	// until 2026-07-28, which is why a press counting into nothing produced no
	// evidence at all.
	if _, err := e.db.EnsureProcessNodeRuntime(node.ID); err != nil {
		e.raiseDeliveredNotBound(delivered, node.CoreNodeName,
			fmt.Sprintf("could not open runtime row for the node: %v", err))
		return
	}
	claim := requestedClaimAtNode(e.db, node)
	if claim == nil {
		e.raiseDeliveredNotBound(delivered, node.CoreNodeName, "no active claim at node")
		return
	}
	cacheValue := 0
	if delivered.BinUOP != nil {
		cacheValue = *delivered.BinUOP
	} else {
		cacheValue = blindDeliverySeed(e, delivered, node.CoreNodeName, claim.Role)
	}
	claimID := claim.ID
	if e.inventoryDelta == nil {
		e.raiseDeliveredNotBound(delivered, node.CoreNodeName, "inventory delta sink not wired")
		return
	}
	if err := e.inventoryDelta.OnDelivered(node.ID, &claimID, *delivered.BinID, delivered.BinEpoch, cacheValue); err != nil {
		e.raiseDeliveredNotBound(delivered, node.CoreNodeName,
			fmt.Sprintf("runtime write failed: %v", err))
		return
	}
	e.recordDeliveredCarrier(node, delivered)
	log.Printf("delivered fallback: bound bin %d to node %s (remaining=%d epoch=%d) via delivery-node resolution",
		*delivered.BinID, node.CoreNodeName, cacheValue, delivered.BinEpoch)
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
