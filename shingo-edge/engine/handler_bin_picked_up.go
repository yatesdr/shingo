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
// on the wire envelope). Used to gate the runtime-mutation branches below:
// the supply leg of a two_robot swap has one pickup at a remote source
// (supermarket) and one dropoff at our slot; the order ID matches but no
// slot pickup event ever fires. Without the gate, the supermarket
// pickup's BinPickedUp would clear active state at our slot.
func (e *Engine) HandleBinPickedUp(orderUUID string, binID int64, location string) {
	e.HandleBinPickedUpMessage(protocol.BinPickedUp{OrderUUID: orderUUID, BinID: binID, Location: location})
}

// HandleBinPickedUpMessage is HandleBinPickedUp with the whole message: the
// lift's generation (Epoch) and time (PickedUpAt) that Lane H's mechanisms
// read (DECISIONS D11). A message without an Epoch (a Core that predates it)
// is handled as before.
func (e *Engine) HandleBinPickedUpMessage(bp protocol.BinPickedUp) {
	orderUUID, binID, location := bp.OrderUUID, bp.BinID, bp.Location
	order, err := e.db.GetOrderByUUID(orderUUID)
	if err != nil || order == nil {
		// Unknown order — Edge may have GC'd a terminal order, or the
		// envelope is for a different station. Log and move on.
		e.logFn("bin_picked_up: order uuid=%s not found", orderUUID)
		return
	}
	// A lift anywhere is a held placer's wake (G7), before every filter below.
	e.onPickupForIntents(order)

	// === Departure stamp — ABOVE the location gate, and the only thing above it ===
	//
	// Everything below the gate is OUR SLOT's work. The departure stamp is not
	// slot work: single_robot departs at OutboundStaging, and a 3-position
	// press's cell pickups can be at the middle or back index position. Gating
	// it on location == CoreNodeName would stamp exactly the legs that do not
	// need it and skip the ones that do.
	//
	// It reads the order and the claim and writes one column on the order row.
	// It touches no runtime slot, so it cannot interact with anything below,
	// and the gate's own early returns stay exactly where they are.
	e.stampDepartureIfLeftCell(order, location)
	// And the auto_push unloader's re-pull, for the same reason it sits here: it
	// reads the order and the window's claim and touches no runtime slot.
	e.rePullOnEmptyOutPickup(order, location)
	// And the keep-staged keeper, when the lift was at the line's spot: the
	// refill behind the spare is ordered now, not at the swap's end.
	e.keepSpotOnPickup(order, location)

	// === Location gate (inverted; fails closed) ===
	//
	// LOAD-BEARING: this gate compares Core's ev.Location (from
	// wiring_block_completed.go:74, un-normalized RDS vendor string)
	// to Edge's ProcessNode.CoreNodeName (which IS trimmed on write
	// at store/processes/processes.go:219,259 via strings.TrimSpace).
	//
	// Trim policy at compare time:
	//   - TrimSpace(location)          — LOAD-BEARING. Core's RDS path
	//                                    doesn't normalize; admin save
	//                                    with stray whitespace on the
	//                                    vendor side would otherwise
	//                                    silently dead-code this gate.
	//   - TrimSpace(node.CoreNodeName) — defense-in-depth. Today Edge
	//                                    always trims on write; trim
	//                                    again here guards against any
	//                                    future write path that skips
	//                                    the canonical trim.
	//
	// Trim only — NOT case-fold. A case mismatch is a real config
	// error and the loud byte-mismatch is a useful failure, not
	// noise to mask.
	//
	// Failure modes treated as "couldn't verify, do nothing":
	//   - order.ProcessNodeID == nil  — kanban-only orders. F' Phase 2
	//     doesn't apply (no process node = no changeover task) and the
	//     downstream flush+clear shouldn't fire either. Fall through
	//     the gate by skipping the whole side-effecting block.
	//   - GetProcessNode error or nil — DB error or missing node. Treat
	//     as "not our slot" rather than fall open (the pre-1.1 bug
	//     class).
	//   - Empty location string       — Core didn't populate. Treat as
	//     "not our slot."
	//
	// If either side ever adds case-folding, prefix-stripping, or any
	// other transform, this gate can flip to "always reject" and the
	// flush + clear become silent dead code. Do not weaken or remove
	// this without replacing the proof end-to-end on the wire path.
	if order.ProcessNodeID == nil {
		// Kanban-only order — no slot to verify, nothing to flush /
		// clear via this handler. Logging would be noise.
		return
	}
	node, nerr := e.db.GetProcessNode(*order.ProcessNodeID)
	if nerr != nil || node == nil {
		e.logFn("bin_picked_up: order=%s — couldn't resolve process node id=%v (nerr=%v); skipping for safety",
			orderUUID, order.ProcessNodeID, nerr)
		return
	}
	if location == "" {
		e.logFn("bin_picked_up: order=%s node=%s — empty Location from Core; skipping for safety (Springfield-class regression path)",
			orderUUID, node.CoreNodeName)
		return
	}
	if strings.TrimSpace(location) != strings.TrimSpace(node.CoreNodeName) {
		e.logFn("bin_picked_up: order=%s pickup at location=%q vs CoreNodeName=%q (display name=%s) — ignoring, not our slot",
			orderUUID, location, node.CoreNodeName, node.Name)
		return
	}

	// A changeover evac's pickup at our slot advances its drop task. (Its
	// supply is no longer released here: the lift is a wake for the supply's
	// own intent — onPickupForIntents, above — S5.)
	if task, terr := e.db.GetChangeoverNodeTaskByEvacOrderID(order.ID); terr == nil && task != nil {
		// Drop tasks with the evacuate marker stay non-terminal until the
		// line is physically clear — the operator opted in to "wait for
		// this node to be evacuated before cutover." Pickup is that
		// moment; the bin's onward trip to the supermarket is just a
		// logistics move and doesn't gate cutover. Advance the task to
		// line_cleared (terminal for drop, per IsNodeTaskStateTerminal)
		// so the cutover guard and downstream rollups unblock immediately
		// rather than waiting for order completion at the destination.
		// Non-evac drops were stamped terminal at plan time, so the
		// !terminal guard makes this a no-op for them.
		if task.Situation == "drop" && !domain.IsNodeTaskStateTerminal(task.State, task.Situation) {
			if err := e.db.UpdateChangeoverNodeTaskState(task.ID, domain.NodeTaskLineCleared); err != nil {
				e.logFn("bin_picked_up: advance drop task %d to line_cleared: %v", task.ID, err)
			}
		}
	}

	if e.inventoryDelta == nil {
		// Nothing to flush; reporter not wired (test contexts).
		return
	}

	// The runtime row is read BEFORE the flush now (it was read just after):
	// the late-lift move below needs the generation the seat holds. Same one
	// read. Under countMu from here to the leave, so no tick lands between
	// taking the after-lift windows out and the seat letting the carrier go
	// (lock order countMu, then the flush lock, as everywhere).
	e.countMu.Lock()
	runtime, rterr := e.db.GetProcessNodeRuntime(*order.ProcessNodeID)
	bound := rterr == nil && runtime != nil && runtime.ActiveBinID != nil && *runtime.ActiveBinID == binID
	hold, holdPart := 0, ""
	if bound {
		hold, holdPart = e.takeTicksAfterLift(node, runtime, bp)
	}

	// Flush the released-bin's accumulator: any deltas recorded
	// between RELEASE PARTIAL and BinPickedUp must ship before the
	// active claim advances or the runtime clears, otherwise
	// post-flush ticks attribute against a bin that's no longer at
	// the slot. A window that started after the lift was taken out above
	// and is not sent.
	if err := e.inventoryDelta.OnBinPickedUp(order.ProcessNodeID); err != nil {
		e.logFn("bin_picked_up: flush failed: %v", err)
	}

	// Clear the active-BIN pointer whenever the bin that just physically left
	// the slot is the one bound as active — gated on BIN IDENTITY, not on the
	// order. Two ways an order-gated clear missed a departed bin, both of which
	// let PLC consume ticks keep charging a bin that was gone:
	//   1. Two-robot swap: the EVAC leg carries the old bin out, but the evac
	//      is usually the staged (not active) order.
	//   2. Changeover abort (cancelProcessChangeover) nulls the active-order
	//      ref *before* the evac's pickup event lands, permanently disarming
	//      the clear.
	// Springfield 2026-06-02: ALN_003 RH→LH changeover aborted mid-swap; bin 18
	// (RH) departed but active_bin_id stayed = 18, so consume ticks drained bin
	// 18 while it sat in the supermarket. Tracking the pointer by physical bin
	// identity makes it follow reality through aborts.
	//
	// ActiveOrderID IS DELIBERATELY LEFT ALONE. This handler used to null it
	// here "so the next tick attribution lands cleanly" — a job it lost at the
	// bin-as-truth flip, where binAtNode (wiring_counter_delta.go) moved to
	// ActiveBinID and no tick, delta or UOP path has read ActiveOrderID since.
	// What it still is is the cell-busy pointer the admission guards read, and
	// clearing it at the press pickup declared the cell free five steps early
	// (single_robot's 4→8 window). orderWorksTheCell (leg_departure.go) frees
	// the cell honestly instead: the pointer stays set until the leg is
	// terminal or departed.
	if rterr != nil || runtime == nil {
		e.countMu.Unlock()
		return
	}
	if bound {
		// The carrier left: pointer, count and identity go together
		// (carrier_left.go), with the lift's generation stamped and the
		// ticks after the lift held for the next carrier of the part, in the
		// same statement (Lane H, D11).
		if err := e.carrierLeftAtPickup(*order.ProcessNodeID, location, bp.Epoch, hold, holdPart); err != nil {
			e.logFn("bin_picked_up: clear active bin node=%d: %v", *order.ProcessNodeID, err)
		}
	}
	e.countMu.Unlock()

	e.logFn("bin_picked_up: flushed deltas + cleared active bin for order=%s bin=%d (status=%s)",
		orderUUID, binID, order.Status)

	// Home consolidation: if this was Order A of a ClearLoaderHome sequence,
	// the robot just cleared the home position. Fire Order B (buffer partial → home).
	// The intent is on Order A's row, so it fires after an Edge restart too; the
	// take is exactly-once, so a replayed pickup does not fire a second Order B.
	if it, ok := e.takeOrderIntent(order.ID, intentConsolidation); ok {
		e.dispatchBufferConsolidation(it.consolidation())
	}
}

// takeTicksAfterLift is the Edge half of the late-lift rule (Lane H R4,
// DECISIONS D11 (ii)). Core ended the carrier's generation at the lift, at
// time T = bp.PickedUpAt, and refuses every window of that generation that
// started at or after T as after_lift. The same windows are taken out of the
// Edge's books here, BEFORE the pickup's flush: the unflushed one is never
// sent, and the flushed ones are summed (uop.Mutator.TakeAfterLift). Those m
// ticks were charged to the carrier because it was still bound when they were
// counted; they belong to what the line drew from after the robot took it.
// So they go back the way a tick goes when no carrier is chargeable: the pile
// first (drainLinesideFirst), then the hold with the carrier's part (R2),
// which the next carrier of the part takes on its first tick. The hold rides
// the leave's statement (carrierLeftAtPickup, +0); the pile drain is one
// write only when a pile has stock.
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
	if !rt.LinesidePayloadKnown && claim != nil {
		part = claim.PayloadCode
	}
	held := -sum // a draw: the ticks consumed after the lift
	if claim != nil && claim.Role == protocol.ClaimRoleProduce {
		held = sum // a fill: the parts made after the lift
	} else if held > 0 {
		drains, rest := e.drainLinesideFirst(node.ID, claim, held)
		if len(drains) > 0 {
			_ = e.inventoryDelta.Consumed(uop.TickEvent{NodeID: node.ID, CoreNodeName: node.CoreNodeName, Drains: drains})
		}
		held = rest
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
