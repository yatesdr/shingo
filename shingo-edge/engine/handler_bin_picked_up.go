// handler_bin_picked_up.go — Edge handler for SubjectBinPickedUp.
//
// HandleBinPickedUp processes Core's notification. It
// fires when Core observes that a robot has physically picked up a
// bin from the source location (driven by the rds.Poller's per-block
// FINISHED transition through wiring_block_completed.go on Core).
//
// SEND PARTIAL BACK is the motivating flow:
//
//  1. Operator clicks RELEASE PARTIAL on a bin that still has UOP
//     remaining. Edge marks the order in_transit, sets the bin's
//     manifest to the partial count, and fires the order to the
//     fleet.
//  2. The cell keeps cycling — PLC ticks continue against the
//     released bin's slot until the robot arrives. Each tick
//     attributes to the released bin (runtime.ActiveBinID still
//     points at it).
//  3. Robot arrives, grabs the bin. Core's rds.Poller sees the
//     pickup-block FINISH and publishes BinPickedUp to Edge.
//  4. Edge flushes the inventory delta accumulator for the released
//     bin so any in-flight ticks ship before the pointer moves, then
//     clears ActiveBinID so subsequent ticks hold until whatever
//     lands next binds.
//
// SME-accepted small bias: if Edge crashes during the pickup window,
// a tick or two recorded after the physical pickup but before the
// flush may attribute to a bin that's no longer at the slot. Post-flip
// (commit 6d226d1) no reconciler exists to heal this; the miscount is
// bounded by the pickup-to-restart window, and a Core-side delta rejection
// it causes is recorded at Core as a refused-delta ledger row.
package engine

import (
	"log"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// HandleBinPickedUp processes a Core BinPickedUp notification.
// Best-effort — failures log and continue rather than rejecting the
// envelope. Post-flip there is no reconciler to heal silent failures;
// attribution that goes wrong shows at Core as refused-delta ledger rows,
// not here.
//
// location is the Core node name where the pickup occurred (BinPickedUp.Location
// on the wire envelope). It decides which seat the carrier left: the seat at
// that place that holds the lifted carrier (leaveSeatsAtLift), whichever
// order lifted it. A supply leg's pickup at a remote source (the supermarket)
// names a place and a carrier no seat of ours holds, so it leaves nothing.
func (e *Engine) HandleBinPickedUp(orderUUID string, binID int64, location string) {
	e.HandleBinPickedUpMessage(protocol.BinPickedUp{OrderUUID: orderUUID, BinID: binID, Location: location})
}

// HandleBinPickedUpMessage is HandleBinPickedUp with the whole message: the
// lift's generation (Epoch) and time (PickedUpAt) that Lane H's mechanisms
// read (DECISIONS D11). A message without an Epoch (a Core that predates it)
// is handled as before.
func (e *Engine) HandleBinPickedUpMessage(bp protocol.BinPickedUp) {
	orderUUID, location := bp.OrderUUID, bp.Location
	order, err := e.db.GetOrderByUUID(orderUUID)
	if err != nil || order == nil {
		// Unknown order — Edge may have GC'd a terminal order, or the
		// envelope is for a different station. Log and move on.
		e.logFn("bin_picked_up: order uuid=%s not found", orderUUID)
		return
	}
	// A lift anywhere is a held placer's wake (G7), before every filter below.
	e.onPickupForIntents(order)

	// === The order's own work: it reads the order and the claim ===
	//
	// None of these is seat work. The departure stamp is not: single_robot
	// departs at OutboundStaging, and a 3-position press's cell pickups can be
	// at the middle or back index position, so matching it to one node would
	// stamp exactly the legs that do not need it and skip the ones that do. It
	// writes one column on the order row and touches no runtime slot.
	e.stampDepartureIfLeftCell(order, location)
	// And the auto_push unloader's re-pull, for the same reason it sits here: it
	// reads the order and the window's claim and touches no runtime slot.
	e.rePullOnEmptyOutPickup(order, location)
	// And the keep-staged keeper, when the lift was at the line's spot: the
	// refill behind the spare is ordered now, not at the swap's end.
	e.keepSpotOnPickup(order, location)
	e.followOnOnPickup(order, location)
	// And a changeover evac's drop task, when the evac lifted at its own node.
	e.advanceEvacDropOnPickup(order, location)

	// === The seat's work: the carrier leaves the seat it was lifted from ===
	e.leaveSeatsAtLift(order, bp)
}

// pickupLocationIs reports whether a pickup's location names the node coreNodeName.
// An empty location names nothing: a pickup Core could not place is not
// matched to any node (the gate fails closed, as it always has).
//
// LOAD-BEARING TRIM POLICY. Core's ev.Location (wiring_block_completed.go) is
// the un-normalized RDS vendor string; Edge's ProcessNode.CoreNodeName is
// trimmed on write (store/processes/processes.go CreateNode, UpdateNode).
//   - TrimSpace(location)     — LOAD-BEARING. Stray whitespace on the vendor
//     side would otherwise silently dead-code every match.
//   - TrimSpace(coreNodeName) — defense-in-depth, against a future write path
//     that skips the canonical trim.
//
// Trim only — NOT case-fold. A case mismatch is a real config error and the
// loud byte-mismatch is a useful failure, not noise to mask. If either side
// ever adds case-folding, prefix-stripping or any other transform, this match
// can flip to "never" and the pickup's seat work becomes silent dead code. Do
// not weaken it without replacing the proof end-to-end on the wire path.
func pickupLocationIs(location, coreNodeName string) bool {
	here := strings.TrimSpace(location)
	return here != "" && here == strings.TrimSpace(coreNodeName)
}

// advanceEvacDropOnPickup advances a changeover evac's drop task when the evac
// lifts its carrier at its own node. Drop tasks with the evacuate marker stay
// non-terminal until the line is physically clear — the operator opted in to
// "wait for this node to be evacuated before cutover." The pickup is that
// moment; the carrier's onward trip to the supermarket is a logistics move and
// doesn't gate cutover. Advancing the task to line_cleared (terminal for drop,
// per IsNodeTaskStateTerminal) unblocks the cutover guard and the rollups now
// rather than at the order's completion. Non-evac drops were stamped terminal
// at plan time, so the terminal guard makes this a no-op for them. (The evac's
// supply is no longer released here: the lift is a wake for the supply's own
// intent — onPickupForIntents — S5.)
//
// Keyed on the ORDER, unlike the seat's work: the task belongs to this evac,
// and the lift that clears the line is the one at the evac's own node.
func (e *Engine) advanceEvacDropOnPickup(order *domain.Order, location string) {
	if order.ProcessNodeID == nil {
		return
	}
	task, err := e.db.GetChangeoverNodeTaskByEvacOrderID(order.ID)
	if err != nil || task == nil || task.Situation != "drop" ||
		domain.IsNodeTaskStateTerminal(task.State, task.Situation) {
		return
	}
	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil || node == nil {
		e.logFn("bin_picked_up: order=%s — couldn't resolve process node id=%v (err=%v); drop task %d not advanced",
			order.UUID, *order.ProcessNodeID, err, task.ID)
		return
	}
	if !pickupLocationIs(location, node.CoreNodeName) {
		return
	}
	if err := e.db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskLineCleared); err != nil {
		e.logFn("bin_picked_up: advance drop task %d to line_cleared: %v", task.ID, err)
	}
	e.pokeCutover(node.ProcessID)
}

// leaveSeatsAtLift is the pickup's seat work: the carrier Core lifted leaves
// every seat at the place of the lift that holds it (leaveSeatAtLift).
//
// ── THE SEAT IS FOUND BY PLACE AND CARRIER, AS THE LANDING FINDS IT ───────
//
// The landing binds by place: a carrier is bound at the node it came to rest
// on, whichever order brought it there (HandleBinLanded). The pickup used to
// find its seat by ORDER instead — the order's own process node, and only when
// the lift was there — so a carrier that landed at a stop on its way was bound
// at the stop and never let go: the pickup there was "not our slot". The seat
// went on naming a carrier that had left, and the changeover planner, which
// reads the seat (nodeStateAt), planned a pickup of it. Hopkinsville
// 2026-11-11: a swap's empties waited at PLN_02 and PLN_05, the press's back
// positions, on their way to PLN_03 and PLN_06; both stayed recorded at both
// places, and the changeover to the next style sent Core two orders to lift
// carriers from PLN_02 and PLN_05 that Core knew were not there.
//
// ── WHAT THE OLD ORDER-AND-PLACE GATE PROTECTED STILL HOLDS ────────────────
//
// That gate (Springfield ALN_001, 2026-05-18) stopped a supply leg's pickup at
// the supermarket from clearing our slot, where the old carrier still stood.
// Place and identity together keep that: a seat is written only when it holds
// the one carrier lifted, at the one place it was lifted from. A seat holding
// another carrier is never written, and nor is the same carrier recorded at
// another place — that record is ended by the lift at THAT place, which Core
// announces in the lift's own transaction (BinService.Lift), not guessed at
// here. A threshold-replenishment order (no process node) is no longer skipped:
// what a seat holds is a fact about the place, not about whose order lifted it.
//
// Pi cost: one read for the seats holding the carrier (normally none or one,
// on a table of a few dozen rows), then, per seat at the place, the runtime
// read, flush and leave the old path made for the order's node. A lift at a
// place no seat holds it costs the one read; it used to cost the node read.
func (e *Engine) leaveSeatsAtLift(order *domain.Order, bp protocol.BinPickedUp) {
	if e.inventoryDelta == nil {
		// Nothing wired to flush or leave through (test contexts).
		return
	}
	if strings.TrimSpace(bp.Location) == "" {
		e.logFn("bin_picked_up: order=%s bin=%d — empty Location from Core; no seat left (fails closed: "+
			"Springfield-class regression path)", order.UUID, bp.BinID)
		return
	}
	seats, err := e.db.ProcessNodesHoldingBin(bp.BinID)
	if err != nil {
		e.logFn("bin_picked_up: order=%s — seats holding bin %d unreadable, none left: %v", order.UUID, bp.BinID, err)
		return
	}
	for i := range seats {
		if pickupLocationIs(bp.Location, seats[i].CoreNodeName) {
			e.leaveSeatAtLift(&seats[i], order, bp)
		}
	}
}

// leaveSeatAtLift lets one seat's carrier go at the lift, gated on BIN
// IDENTITY under countMu: the seat is re-read under the lock, and a seat that
// let the carrier go between the read and the lock is left alone. Identity,
// not the order, because an order-gated clear missed departed carriers twice:
// a two-robot swap's EVAC carries the old bin out but is usually the staged,
// not the active, order; and a changeover abort (cancelProcessChangeover)
// nulls the active-order ref before the evac's pickup lands. Springfield
// 2026-06-02: ALN_003's bin 18 left mid-swap, active_bin_id stayed 18, and
// consume ticks drained it while it sat in the supermarket.
//
// Under countMu from the read to the leave, so no tick lands between taking
// the after-lift windows out and the seat letting the carrier go (lock order
// countMu, then the flush lock, as everywhere):
//
//  1. takeTicksAfterLift takes out the windows counted after the lift (Lane H
//     R4, D11) — it needs the generation the seat holds, so it reads first;
//  2. the flush ships what was charged to the carrier while it was bound,
//     before the pointer moves;
//  3. the leave (carrierLeftAtPickup, carrier_left.go): pointer, count and
//     identity go together, with the lift's generation stamped and a produce
//     seat's after-lift ticks held for its next carrier, in one statement.
//
// ActiveOrderID IS DELIBERATELY LEFT ALONE. It is the cell-busy pointer the
// admission guards read, and no tick, delta or UOP path has read it since the
// bin-as-truth flip moved attribution to ActiveBinID (binAtNode,
// wiring_counter_delta.go). Clearing it at the press pickup declared the cell
// free five steps early (single_robot's 4→8 window); orderWorksTheCell
// (leg_departure.go) frees the cell honestly, at terminal or departure.
func (e *Engine) leaveSeatAtLift(node *processes.Node, order *domain.Order, bp protocol.BinPickedUp) {
	e.countMu.Lock()
	defer e.countMu.Unlock()
	rt, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil || rt == nil || rt.ActiveBinID == nil || *rt.ActiveBinID != bp.BinID {
		return
	}
	hold, holdPart := e.takeTicksAfterLift(node, rt, bp)
	if err := e.inventoryDelta.OnBinPickedUp(&node.ID); err != nil {
		e.logFn("bin_picked_up: flush failed: %v", err)
	}
	if err := e.carrierLeftAtPickup(node.ID, node.CoreNodeName, bp.Epoch, hold, holdPart); err != nil {
		e.logFn("bin_picked_up: bin %d leaving %s: %v", bp.BinID, node.CoreNodeName, err)
		return
	}
	e.logFn("bin_picked_up: bin %d left %s — flushed and cleared (order=%s status=%s)",
		bp.BinID, node.CoreNodeName, order.UUID, order.Status)
}

// takeTicksAfterLift is the Edge half of the late-lift rule (Lane H R4,
// DECISIONS D11 (ii)). Core ended the carrier's generation at the lift, at
// time T = bp.PickedUpAt, and refuses every window of that generation that
// started at or after T as after_lift. The same windows are taken out of the
// Edge's books here, BEFORE the pickup's flush: the unflushed one is never
// sent, and the flushed ones are summed (uop.Mutator.TakeAfterLift). Those m
// ticks were charged to the carrier because it was still bound when they were
// counted; they belong to what the line drew from after the robot took it.
// So they go back the way a tick goes when no carrier takes it: on a consume
// seat the pile first (drainLinesideFirst), then the ledger (decision 55: no
// cart of the part stands there), against the claim's part under the PLC's
// part as the lift reads it, one upsert on the seat as read (logNoCart); on a
// produce seat the hold with the claim's part, which the next bin takes on
// its first tick, riding the leave's statement (carrierLeftAtPickup, +0). The
// pile drain is one write only when a pile has stock.
//
// The carrier's own count needs no correction: the leave zeroes it.
//
// Only for a lift Core bumped (bp.Epoch above the generation the seat holds):
// a pickup from a Core that predates the rule, or a lift Core did not bump,
// refuses nothing at Core, so nothing moves here. Normal latency and a long
// outage are the same path; at normal latency m is usually 0. Caller holds
// countMu. Returns the hold to add and its part.
func (e *Engine) takeTicksAfterLift(node *processes.Node, rt *processes.RuntimeState, bp protocol.BinPickedUp) (int, string) {
	if bp.Epoch <= rt.ActiveBinEpoch || bp.PickedUpAt.IsZero() {
		return 0, ""
	}
	sum, err := e.inventoryDelta.TakeAfterLift(bp.BinID, rt.ActiveBinEpoch, bp.PickedUpAt)
	if err != nil {
		log.Printf("bin_picked_up: windows after the lift of bin %d: %v", bp.BinID, err)
	}
	if sum == 0 {
		return 0, ""
	}
	var claim *processes.NodeClaim
	if rt.ActiveClaimID != nil {
		claim, _ = e.db.GetStyleNodeClaim(*rt.ActiveClaimID)
	}
	part := string(rt.LinesidePayloadCode)
	if !rt.PartRecorded() && claim != nil {
		part = claim.PayloadCode
	}
	held := -sum // a draw: the ticks consumed after the lift
	if claim != nil && claim.Role == protocol.ClaimRoleProduce {
		// A fill: the parts made after the lift, of the part the press makes
		// (producedPayload). The carrier's resident payload is "" for the
		// whole fill, and a "" hold replays onto any part.
		held, part = sum, producedPayload(claim)
	} else if held > 0 {
		drains, rest := e.drainLinesideFirst(node.ID, claim, held)
		if len(drains) > 0 {
			_ = e.inventoryDelta.Consumed(uop.TickEvent{NodeID: node.ID, CoreNodeName: node.CoreNodeName, Drains: drains})
		}
		if rest > 0 && claim != nil {
			catid := e.confirmedCATID(node.ProcessID)
			if _, err := e.logNoCart(node.ProcessID, catid, node, claim.PayloadCode, rest, processes.SeatAsReadOf(rt), "after the lift"); err != nil {
				log.Printf("bin_picked_up: %d counted after the lift of bin %d not logged: %v", rest, bp.BinID, err)
			}
		} else if rest > 0 {
			log.Printf("bin_picked_up: %d counted after the lift of bin %d not logged: the seat names no claim", rest, bp.BinID)
		}
		return 0, ""
	}
	if held <= 0 {
		return 0, ""
	}
	if rt.PendingUOPDelta != 0 && !holdIsOf(rt.PendingUOPPayload, part) {
		// D12: the seat holds another part's ticks, which keep their hold.
		log.Printf("bin_picked_up: %d of part %s counted after the lift of bin %d not held -- the seat holds %d of part %s (D12)",
			held, part, bp.BinID, rt.PendingUOPDelta, rt.PendingUOPPayload)
		return 0, ""
	}
	log.Printf("bin_picked_up: bin %d lifted at %s (generation %d -> %d): %d counted after the lift held for the next carrier of %s",
		bp.BinID, bp.PickedUpAt.Format("15:04:05.000"), rt.ActiveBinEpoch, bp.Epoch, held, part)
	return held, part
}
