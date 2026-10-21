// wiring_delivered.go — OrderDelivered handler.
//
// Subscribed via wireEventHandlers (wiring.go) on EventOrderDelivered,
// which fires the moment an order transitions to StatusDelivered.
//
// ORDER.DELIVERED BINDS NOTHING (Lane H, H.5). The carrier Core placed is
// bound by its landing (transit.bin_landed, handler_bin_landed.go), written in
// the transaction that moved the bin. The delivered closes the order and keeps
// ONE check: delivered to a seat of mine whose bound carrier is not the
// order's bin → said once, by name, with no write. It used to bind here too,
// WHERE node only, with Core's arrival count, so a delivered arriving after
// the line had counted wrote the ticks away (DESIGN-H §1 A7, §6).
//
// An older Core sends no landing, and says so by leaving
// protocol.OrderDelivered.LandingSent unset. For that Core, for one release,
// the delivered's carrier fields still bind, through the landing verb with no
// placement stamp: into an empty slot or onto the same bin only, never over a
// different bound carrier (H.7; landFromOldCore). The pullback auto-clear
// stays: it is a declared clear, not a bind.

package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	storeorders "shingoedge/store/orders"
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

// handleNodeOrderDelivered closes the delivered side of an order: the check
// (or, for an older Core, the bind through the landing verb), and the
// pullback auto-clear. Gates on:
//
//   - ProcessNodeID present and resolvable (else the by-name fallback, or the
//     multi-bin backstop alarm, as before);
//   - the carried bin landed at this node: BinDestNode == CoreNodeName for
//     multi-tote; steps finalDropoff / DeliveryNode == CoreNodeName otherwise.
//     Removal-shaped orders (Order B delivers to the supermarket) are not
//     about this seat and no-op out. This resolution decides only whether the
//     check is about this seat; it binds nothing.
func (e *Engine) handleNodeOrderDelivered(delivered OrderDeliveredEvent) {
	if delivered.ProcessNodeID == nil || delivered.BinID == nil {
		switch {
		case delivered.ProcessNodeID == nil && delivered.BinID != nil && delivered.DeliveryNode != "":
			// No Edge process node on the order, but a bin and a destination name
			// are present: a Core-admin order with no Edge order row, or an Edge
			// order whose row carries no process_node_id (a manual move order is
			// created against a Core node name). Both resolve by name.
			e.handleFallbackDelivered(delivered)
		case delivered.ProcessNodeID != nil && delivered.BinID == nil:
			// Multi-bin delivery whose envelope carries no per-bin id: Core
			// resolved NO order_bin to this process node — a genuine gap worth
			// naming, unless the leg was never going to leave a bin here (a
			// press-index R1 sets its two carried bins down elsewhere, and Core
			// is right to ship nil). Alarm, bind nothing.
			if e.legLeftNoBinHereByDesign(delivered) {
				return
			}
			e.raiseDeliveredNotBound(delivered, "", "multi-bin delivery carried no bin id — no bin resolved to this node (F1b backstop)")
		default:
			if delivered.LandingSent {
				// The landing is the bind; a delivered with nothing to check
				// against is just a closed order.
				return
			}
			// An older Core, and nothing matched: HK 2026-07-28 was two move
			// orders matching no bind path with zero diagnostics. Say so.
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
	if !e.deliveredAtNode(order, node, delivered) {
		return
	}
	if delivered.LandingSent {
		e.checkDeliveredCarrier(node, delivered)
	} else if !e.landFromOldCore(node, delivered) {
		return
	}

	// Auto-clear: if this was a pull-from-market delivery, zero the bin UOP
	// immediately so the operator doesn't need to hit a separate Clear Bin button.
	// The intent is read off the order row, so it fires after an Edge restart
	// too; the take is exactly-once, so a replayed delivery does not clear twice.
	if _, isPullback := e.takeOrderIntent(order.ID, intentPullback); isPullback {
		// The auto-clear is for the carrier this delivery brought (0 from a Core
		// that sent no bin id): Core refuses it if another stands there.
		var deliveredBin int64
		if delivered.BinID != nil {
			deliveredBin = *delivered.BinID
		}
		req := protocol.BinClearRequest{NodeName: node.CoreNodeName, BinID: deliveredBin}
		answered := func(cleared *BinClearResponse, err error) {
			if err != nil {
				log.Printf("market_pullback: auto-clear bin at %s: %v", node.CoreNodeName, e.refused(err))
				return
			}
			log.Printf("market_pullback: auto-cleared bin at %s on delivery", node.CoreNodeName)
			// The stamp and the identity: the auto-clear started this carrier's
			// next life, and it is known-empty now — the clear is the
			// operator-visible event the Clear button's shape records. The claim
			// is the seat's own (the landing does not move it).
			var claimID *int64
			if rt, rerr := e.db.GetProcessNodeRuntime(node.ID); rerr == nil && rt != nil {
				claimID = rt.ActiveClaimID
			}
			if err := e.carrierClearedInPlace(node.ID, node.CoreNodeName, claimID, cleared.BinID, cleared.DeltaEpoch); err != nil {
				log.Printf("market_pullback: record the clear at %s: %v", node.CoreNodeName, err)
			}
		}
		// An ask when Core answers bin.clear, the bin-clear door otherwise; an
		// answer after the wait is taken the same way when it comes.
		cleared, err := askOrDoorLate(e, protocol.AskBinClear, req,
			func() (*BinClearResponse, error) { return e.coreClient.ClearBin(req) }, answered)
		if errors.Is(err, ErrAskPending) {
			log.Printf("market_pullback: auto-clear at %s sent to Core, not answered yet", node.CoreNodeName)
			return
		}
		answered(cleared, err)
	}
}

// deliveredAtNode reports whether the order's carried bin came to rest at
// node, for the check and the older Core's bind. Only single-bin orders and
// F1b multi-tote orders with a resolved bin reach here.
//
// order.DeliveryNode is NOT usable for a complex order: a complex order has
// many dropoffs, and Edge stamps swap legs with the order's PROCESS node, which
// for a press-index R1 leg names the press while the bin it carries is staged
// at the paired index node (HK 2026-07-14 bound an EMPTY tote to the press
// that way). The steps' final dropoff is where its one bin came to rest.
func (e *Engine) deliveredAtNode(order *storeorders.Order, node *processes.Node, delivered OrderDeliveredEvent) bool {
	switch {
	case delivered.BinDestNode != "":
		// F1b multi-tote: Core selected the one bin destined for the consuming
		// node and shipped its landing node here.
		return delivered.BinDestNode == node.CoreNodeName
	case order.OrderType == protocol.OrderTypeComplex:
		stepsJSON, sErr := e.db.GetOrderStepsJSON(order.ID)
		if sErr != nil {
			log.Printf("delivered: order %d — cannot load steps to resolve complex destination: %v", order.ID, sErr)
			return false
		}
		dest := finalDropoffNode(stepsJSON)
		if dest == "" {
			log.Printf("delivered: order %d (complex) has no resolvable final dropoff — steps missing or dropoff-less; node %s not checked", order.ID, node.CoreNodeName)
			return false
		}
		return dest == node.CoreNodeName
	default:
		return order.DeliveryNode == node.CoreNodeName
	}
}

// checkDeliveredCarrier is the one check H.5 keeps: the order delivered its
// carrier to this seat, the landing should have bound it, and the seat holds
// something else (or nothing). Said once, by name; nothing is written. The
// operator's front door (Record Count on the bin tab) binds it.
//
// One read, of a row the delivered handler used to write.
func (e *Engine) checkDeliveredCarrier(node *processes.Node, delivered OrderDeliveredEvent) {
	rt, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil || rt == nil {
		e.raiseDeliveredNotBound(delivered, node.CoreNodeName, "delivered here, and the seat has no runtime row — the landing bound nothing")
		return
	}
	if rt.ActiveBinID != nil && *rt.ActiveBinID == *delivered.BinID {
		return
	}
	bound := "nothing"
	if rt.ActiveBinID != nil {
		bound = fmt.Sprintf("bin %d", *rt.ActiveBinID)
	}
	e.raiseDeliveredNotBound(delivered, node.CoreNodeName,
		fmt.Sprintf("delivered here, but the seat holds %s — its landing has not bound it", bound))
}

// landFromOldCore is the older Core's delivered, for one release (Lane H,
// H.7): its carrier fields bind through the landing verb with no placement
// stamp, into an empty slot or onto the same bin only, never over a different
// bound carrier. The same bin already bound at an equal or newer generation
// keeps its count (H.4), so a delivered arriving after ticks no longer writes
// them away. Reports whether the seat now holds the order's carrier.
//
// A nil count is a carrier Core could not read: nothing is bound (it used to
// seat 0 and say BLIND DELIVERY), and the check names it.
func (e *Engine) landFromOldCore(node *processes.Node, delivered OrderDeliveredEvent) bool {
	if e.claimAtNode(node) == nil {
		// The bin landed at a node we own but there is no active claim to bind
		// it under (unpublished/mid-changeover style, orphaned node). A
		// Core-owned loader window is NOT this case: claimAtNode synthesizes
		// its claim.
		e.raiseDeliveredNotBound(delivered, node.CoreNodeName, "no active claim at node")
		return false
	}
	if delivered.BinUOP == nil {
		e.raiseDeliveredNotBound(delivered, node.CoreNodeName, "Core sent no count for the carrier — nothing bound")
		return false
	}
	carrier := domain.UnknownCarrier()
	if delivered.BinPayloadCode != nil {
		carrier = domain.KnownCarrier(domain.LinesidePayloadCode(*delivered.BinPayloadCode))
	}
	if e.landCarrier(node, processes.LandingBind{
		BinID: *delivered.BinID, Epoch: delivered.BinEpoch, Count: *delivered.BinUOP,
	}, carrier, "an older Core's delivered, order "+delivered.OrderUUID) {
		return true
	}
	e.checkDeliveredCarrier(node, delivered)
	return false
}

// handleFallbackDelivered is the delivered of an order with no Edge process
// node (a Core-admin order with no Edge row, or a manual move order created
// against a Core node name): the node is resolved by its Core dot-name. With
// a landing, it is the check; for an older Core, the bind through the landing
// verb (landFromOldCore).
func (e *Engine) handleFallbackDelivered(delivered OrderDeliveredEvent) {
	node, err := e.db.GetProcessNodeByCoreNodeName(delivered.DeliveryNode)
	if err != nil || node == nil {
		// NOT an alarm. Most deliveries that reach here land at a supermarket,
		// staging, or empty-tote node that is not an Edge process node at all.
		e.logFn("delivered fallback: %s is not an Edge process node — nothing to check", delivered.DeliveryNode)
		return
	}
	if delivered.LandingSent {
		e.checkDeliveredCarrier(node, delivered)
		return
	}
	if e.landFromOldCore(node, delivered) {
		log.Printf("delivered fallback: bound bin %d to node %s (remaining=%d epoch=%d) via delivery-node resolution",
			*delivered.BinID, node.CoreNodeName, *delivered.BinUOP, delivered.BinEpoch)
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
//
// TODO(2026-09-10): study a better shape.
// This alarm is the INSTRUMENT, not the fix, and its `reason` strings are
// separate root causes wearing one label. "No active claim at node" is an order
// that should not have been created, or a claim-resolution gap. "Multi-bin
// delivery carried no bin id" is the PRODUCER violating facts-carried-not-
// derived: the delivery event should always name the bin it delivered.
// The better shape is to read this alarm as a histogram BY REASON and fix the
// top reason at its source until it stops firing. The operator's "Record Count"
// is the backstop, not the design.
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
