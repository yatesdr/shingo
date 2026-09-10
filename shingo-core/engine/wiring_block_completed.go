// wiring_block_completed.go — Phase 2 of the bin-transit-state project.
//
// Engine handler for EventBlockCompleted (fired by the rds.Poller when a
// per-block state transitions to FINISHED while the parent order is
// still mid-flight). For pickup blocks, drives the bin claimed at that
// step onto the synthetic _TRANSIT node so the source slot is freed
// immediately — the slot-vacancy signal queued orders need to unblock.
//
// Block-kind routing:
//
//   - pickup-shaped (BinTask=Load, "pickup", or any operation that
//     loads a goods onto the robot): bin transitions to _TRANSIT.
//   - dropoff-shaped: INTERMEDIATE dropoffs (a midway storage slot, not the
//     order's final delivery) fire bin arrival immediately via
//     handleStoreBlockCompleted, so the slot reflects the physical bin the
//     moment the store completes — and a mid-flight cancel leaves the bin at
//     its slot instead of stranded at _TRANSIT (with the slot reading empty,
//     a double-store hazard). The FINAL delivery is still driven by
//     handleOrderDelivered when the whole order reaches FINISHED (that path
//     is robust, idempotent, and also sends the Edge OrderDelivered
//     notification), so we deliberately do not race it here.
//   - waits, scripts, navigation-only: no-op.
//
// Idempotence: BinService.MoveToTransit is a no-op when the bin is
// already at _TRANSIT, so duplicate or replayed events are safe.
//
// Failure mode: if order_bins lookup misses for the block's location
// (concrete bin couldn't be claimed at order-creation time, or the
// junction row was never written for a single-bin complex order), we
// fall back to order.BinID. That covers the simpler case where there's
// only one bin per order. If neither path resolves a bin, we log and
// drop — the bin's source slot stays nominally occupied until delivery,
// which is the pre-Phase-2 behavior. Acceptable degradation.

package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/domain"
	"shingocore/service"
	"shingocore/store/orders"
	"shingocore/store/telemetry"
)

// handleBlockCompleted is called from wiring.go's EventBlockCompleted
// subscription. Routes the block by kind and drives the corresponding
// bin lifecycle transition: pickups free the source slot (→ _TRANSIT),
// intermediate storage dropoffs record the bin at its slot immediately.
func (e *Engine) handleBlockCompleted(ev BlockCompletedEvent) {
	// Record the observation before routing it. This handler was the sole
	// subscriber and it threw the block away after moving the bin, so every
	// leg the fleet reported — travel-to-source, load, travel-to-dest, unload —
	// existed for the duration of one function call and was never written down.
	e.recordBlockLeg(ev)

	switch {
	case IsPickupBlock(ev.BinTask):
		e.handlePickupBlockCompleted(ev)
	case IsDropoffBlock(ev.BinTask):
		e.handleStoreBlockCompleted(ev)
	}
}

// BlockLegState is the mission_events.new_state marker for a per-block
// completion.
//
// Deliberately NOT a vendor OrderState value. These rows are a finer grain
// than the order-level transitions beside them, so a reader filtering on real
// vendor states must not pick them up by accident — and one that wants legs
// can select exactly this.
//
// EXPORTED so the mission-detail handler can recognise a leg row without
// re-spelling the string. There was a second spelling — a literal in
// mission-detail.js — and the page now asks the server instead (is_leg on the
// event view), so this constant is the only place the value appears.
//
// That matters more than tidiness. A leg row's new_state is NOT a vendor state,
// so putting it through fleet.MapState takes the unrecognised-value arm, which
// answers "dispatched" and logs a line. Every leg row of every mission would
// have acquired a status it never had, and printed a log line for the privilege.
const BlockLegState = "BLOCK_FINISHED"

// blockLeg is the per-block record stored in mission_events.blocks_json.
// Epoch seconds, verbatim from the vendor; DurationSeconds is derived and 0
// when either endpoint is missing.
type blockLeg struct {
	BlockID         string `json:"blockId"`
	Location        string `json:"location"`
	BinTask         string `json:"binTask"`
	StartTime       int64  `json:"startTime"`
	TerminateTime   int64  `json:"terminateTime"`
	DurationSeconds int64  `json:"durationSeconds"`
}

// recordBlockLeg writes one mission_events row per completed block — roughly
// two rows per order, into a JSONB column that already exists, so no schema
// change and no new table.
//
// This is the leg decomposition the design has wanted since round 1. It was
// recorded as blocked on the vendor not reporting per-block times; the times
// were on the wire the whole time and rds.BlockDetail simply had no fields to
// hold them.
//
// Best-effort: a failure here logs and returns. Losing a telemetry row must
// never stop the bin from moving.
func (e *Engine) recordBlockLeg(ev BlockCompletedEvent) {
	leg := blockLeg{
		BlockID:       ev.BlockID,
		Location:      ev.Location,
		BinTask:       ev.BinTask,
		StartTime:     ev.StartTime,
		TerminateTime: ev.TerminateTime,
	}
	// Only derive a duration from two real endpoints. A vendor that reports
	// neither leaves this 0, which reads as "unknown" — not as "instant".
	if ev.StartTime > 0 && ev.TerminateTime >= ev.StartTime {
		leg.DurationSeconds = ev.TerminateTime - ev.StartTime
	}

	blocks, err := json.Marshal([]blockLeg{leg})
	if err != nil {
		e.logFn("telemetry: marshal block leg for order %d: %v", ev.OrderID, err)
		return
	}

	// The robot comes from the ORDER. Block events carry no vehicle at all —
	// neither the poller's EmitBlockCompleted nor the simulator's has ever had
	// one — so without this every leg row would have a blank robot_id and the
	// per-robot leg breakdown would be empty for the same reason the mission
	// summaries were.
	robotID := ""
	if order, oerr := e.db.GetOrder(ev.OrderID); oerr == nil && order != nil {
		robotID = order.RobotID
	}

	detail := fmt.Sprintf("block %s @ %s (binTask=%s)", ev.BlockID, ev.Location, ev.BinTask)
	if leg.DurationSeconds > 0 {
		detail += fmt.Sprintf(" took %ds", leg.DurationSeconds)
	}

	if err := e.db.InsertMissionEvent(&telemetry.Event{
		OrderID:       ev.OrderID,
		VendorOrderID: ev.VendorOrderID,
		OldState:      "",
		NewState:      BlockLegState,
		RobotID:       robotID,
		BlocksJSON:    string(blocks),
		ErrorsJSON:    "[]",
		Detail:        detail,
	}); err != nil {
		e.logFn("telemetry: record block leg for order %d: %v", ev.OrderID, err)
	}
}

// handlePickupBlockCompleted drives the bin claimed at a pickup block onto
// the synthetic _TRANSIT node so the source slot frees immediately — the
// slot-vacancy signal queued orders need to unblock.
func (e *Engine) handlePickupBlockCompleted(ev BlockCompletedEvent) {
	binID, stepIndex, fromNodeID, ok := e.resolvePickupBin(ev.OrderID, ev.Location)
	if !ok {
		e.logFn("transit: order %d block %s @ %s — no claimed bin matched; bin will move to dest at delivery (pre-Phase-2 behavior)",
			ev.OrderID, ev.BlockID, ev.Location)
		return
	}

	if err := e.binService.MoveToTransit(binID); err != nil {
		e.logFn("transit: MoveToTransit bin %d for order %d: %v", binID, ev.OrderID, err)
		return
	}

	e.dbg("transit: bin %d entered _TRANSIT (order %d, block %s @ %s, step %d)",
		binID, ev.OrderID, ev.BlockID, ev.Location, stepIndex)

	// Item 11: notify Edge that the bin was physically picked up. The
	// SEND PARTIAL BACK flow needs this signal to flush the released
	// bin's delta accumulator and advance the active claim. We publish
	// for every pickup (not just partial-back) — the Edge handler
	// no-ops gracefully when the order doesn't match a tracked bin.
	if order, err := e.db.GetOrder(ev.OrderID); err == nil && order != nil && order.StationID != "" {
		if err := e.SendDataToEdge(protocol.SubjectBinPickedUp, order.StationID, &protocol.BinPickedUp{
			OrderUUID:  order.EdgeUUID,
			BinID:      binID,
			Location:   ev.Location,
			PickedUpAt: clock.Now().UTC(),
		}); err != nil {
			e.logFn("transit: send BinPickedUp bin %d order %d: %v", binID, ev.OrderID, err)
		}
	}

	e.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{
		BinID:      binID,
		OrderID:    ev.OrderID,
		FromNodeID: fromNodeID,
		StepIndex:  stepIndex,
	}})
}

// resolvePickupBin finds the bin claimed at the given pickup-block
// location. Returns binID, stepIndex, fromNodeID (the source node ID
// the bin is leaving), and ok.
//
// Lookup order:
//  1. Multi-bin complex order: order_bins junction. Match by
//     NodeName == location AND Action == "pickup". When multiple
//     pickups share a location (rare — same supermarket lane twice
//     in one swap), pick the earliest unmoved one (lowest step_index
//     whose bin's NodeID still equals the source node — others have
//     already transitioned).
//  2. The order's own claimed bin AT this location — see below.
//  3. Single-bin order fallback: order.BinID.
//
// ── WHY (2) EXISTS: A PLAN'S RE-PICKUPS HAVE NO JUNCTION ROWS ──────────────
//
// The junction is written at ALLOCATION time and names the endpoints the
// allocator chose. A plan's INTERMEDIATE pickups are not among them. A
// single_robot swap parks the fresh carrier at InboundStaging (step 2) and
// collects it again at step 6; it parks the spent one at OutboundStaging
// (step 5) and collects it again at step 8. Neither of those two pickups has a
// junction row, so both fell straight through to (3) — which returns
// `order.BinID` WITHOUT ASKING WHERE THAT BIN IS.
//
// So the step-6 pickup at InboundStaging resolved to the SPENT carrier sitting
// at OutboundStaging, and the step-7 dropoff then recorded the spent carrier
// onto the LINE — with its own count, which is what the Edge binds and charges
// PLC ticks against. The fresh carrier stayed recorded at InboundStaging until
// whole-order FINISHED.
//
// Measured on the demo plant, 2026-09-06, order 8 at ALN_004 (and identically
// order 5 at ALN_003, same run, and again on a clean re-run):
//
//	b3 SLN_005 JackUnload   bin 7  stored at SLN_005   the fresh carrier, parked
//	b6 SLN_006 JackUnload   bin 24 stored at SLN_006   the spent carrier, parked
//	b7 SLN_005 JackLoad     bin 24 entered _TRANSIT    <- WRONG: bin 24 is at SLN_006
//	b8 ALN_004 JackUnload   bin 24 stored at ALN_004   <- WRONG: the spent one, on the line
//	b9 SLN_006 JackLoad     bin 24 entered _TRANSIT    <- and off again
//
// The second consequence is the one that wedged cells: because b9 lifted the
// only bin Core believed was on the line, ALN_004 read EMPTY from step 8
// onward, and the level sweep minted a bare move into a position that
// physically held the fresh carrier. That move holds forever. See
// ISSUE-sim-position-hold-deadlock-2026-09-06.md.
//
// (2) ASKS THE SAME QUESTION resolveDropoffBin ASKS, from the other side. That
// function resolves a dropoff as "the one bin this order has claimed at
// _TRANSIT" — what the robot is holding — and its header records why one rule
// beat two special cases. A pickup is the dual: the bin this order lifts at L
// is the one bin it has claimed AT L. No junction, no step index, no agreement
// with anything.
//
// FAILS CLOSED, and closed here means "today's behaviour". Zero claimed bins at
// the location or more than one leaves (3) to answer exactly as it does now;
// nothing that resolves today stops resolving.
func (e *Engine) resolvePickupBin(orderID int64, location string) (binID int64, stepIndex int, fromNodeID int64, ok bool) {
	// LOAD-BEARING (same contract as shingo-edge/engine/handler_bin_picked_up.go):
	// `location` arrives from BlockCompletedEvent.Location, originally
	// an un-normalized RDS vendor string. `ob.NodeName` comes from
	// the order_bins junction, populated at order-creation time from
	// nodes.name. All Core write paths trim on write today, but
	// tainted rows from a pre-trim install (or any future write path
	// that bypasses the trim) would silently break this filter,
	// leaving the bin nominally occupied at source.
	//
	// Defensive TrimSpace on both sides. Trim only — NOT case-fold;
	// case mismatch is a real config error.
	locationTrimmed := strings.TrimSpace(location)
	// Multi-bin path: junction table.
	rows, err := e.db.ListOrderBins(orderID)
	if err == nil && len(rows) > 0 {
		for _, ob := range rows {
			if ob.Action != protocol.ActionPickup {
				continue
			}
			if strings.TrimSpace(ob.NodeName) != locationTrimmed {
				continue
			}
			bin, err := e.db.GetBin(ob.BinID)
			if err != nil || bin == nil {
				continue
			}
			// Skip bins that have already transitioned (their NodeID
			// is no longer at the source). This handles duplicate
			// pickup events for repeated-location orders.
			srcNode, srcErr := e.db.GetNodeByDotName(ob.NodeName)
			if srcErr != nil || srcNode == nil {
				continue
			}
			if bin.NodeID == nil || *bin.NodeID != srcNode.ID {
				continue
			}
			return ob.BinID, ob.StepIndex, srcNode.ID, true
		}
	}

	// The order's own claimed bin AT this location. See the header: a plan's
	// intermediate re-pickups carry no junction row, and the fallback below
	// answers with order.BinID wherever that bin happens to be.
	if binID, from, ok := e.claimedBinAt(orderID, locationTrimmed); ok {
		return binID, 0, from, true
	}

	// Single-bin fallback.
	order, err := e.db.GetOrder(orderID)
	if err != nil || order == nil || order.BinID == nil {
		if err == nil && order != nil && order.BinID == nil {
			// SHADOWED: the single-bin fallback gives up here for a coordinator
			// and for a defect alike.
			owns, oerr := e.db.OrderOwnsNoCargo(order.ID)
			service.NoteFolderShadow(service.FolderSiteBlockCompleted, order.ID, owns, oerr)
		}
		return 0, 0, 0, false
	}
	bin, err := e.db.GetBin(*order.BinID)
	if err != nil || bin == nil {
		return 0, 0, 0, false
	}
	from := int64(0)
	if bin.NodeID != nil {
		from = *bin.NodeID
	}
	return *order.BinID, 0, from, true
}

// claimedBinAt returns the single bin this order has claimed at `location`, and
// the node it is leaving. The pickup dual of resolveDropoffBin's transit rule:
// one robot lifts one bin, so if exactly one of the order's own bins is sitting
// at the node the block completed at, that is the bin.
//
// Ambiguity is not resolved here, deliberately. Zero means the order has nothing
// of its own at this node — a junction-less pickup of a bin it does not hold, or
// a replayed block whose bin has already moved to _TRANSIT. More than one means
// two of its bins share the node, which one robot cannot lift. Either way the
// caller falls through to the behaviour that predates this, rather than guessing
// — and a wrong bin identity here writes a wrong count onto a line position.
func (e *Engine) claimedBinAt(orderID int64, location string) (binID int64, fromNodeID int64, ok bool) {
	if location == "" {
		return 0, 0, false
	}
	node, err := e.db.GetNodeByDotName(location)
	if err != nil || node == nil {
		return 0, 0, false
	}
	held, err := e.db.ListBinsByClaim(orderID)
	if err != nil {
		return 0, 0, false
	}
	var found []int64
	for _, b := range held {
		if b.NodeID != nil && *b.NodeID == node.ID {
			found = append(found, b.ID)
		}
	}
	if len(found) != 1 {
		if len(found) > 1 {
			e.logFn("transit: order %d pickup @ %s — %d of its own bins are at this node, want exactly 1; "+
				"falling back to the order's bin_id", orderID, location, len(found))
		}
		return 0, 0, false
	}
	return found[0], node.ID, true
}

// handleStoreBlockCompleted records a bin at its destination slot the moment
// an INTERMEDIATE dropoff block finishes — the store dual of the
// pickup→_TRANSIT transition. Without it, a bin dropped at a midway storage
// slot (the "store the full bin, then go retrieve" leg of a complex swap)
// stays recorded at _TRANSIT until the WHOLE order reaches FINISHED. For the
// duration the slot reads empty, and if the order is cancelled mid-flight the
// bin is stranded at _TRANSIT while a downstream order can dispatch a second
// bin into the physically-occupied slot (the Hopkinsville #130/#132 divergence).
//
// Scope is deliberately narrow:
//   - Only multi-bin complex orders (order_bins junction populated) have an
//     intermediate dropoff; resolveDropoffBin no-ops for single-bin orders
//     and compound children (no junction rows), leaving their well-tested
//     completion path untouched.
//   - The FINAL delivery (location == order.DeliveryNode) is skipped — it is
//     driven by handleOrderDelivered at whole-order FINISHED, which also ships
//     the Edge OrderDelivered notification; racing it here would buy nothing.
//
// Idempotent: resolveDropoffBin returns only a bin still claimed by this
// order, so an already-delivered (unclaimed) bin or a replayed block event is
// a no-op.
func (e *Engine) handleStoreBlockCompleted(ev BlockCompletedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil || order == nil {
		return
	}
	location := strings.TrimSpace(ev.Location)
	// Lane mouth gate (§4): release the order's inbound hold on the drop lane as
	// soon as the dropoff completes — BEFORE the delivery-node early-return below,
	// or a simple store's final drop (drop == delivery) would never early-release
	// its lane. A no-op when the gate is off or the drop is not into a lane.
	e.dispatcher.ReleaseInboundLaneForOrder(ev.OrderID, location)
	// Hold B: the leg has PLACED its bin, so it is out of the lane it was
	// working.
	//
	// NO LONGER THE ONLY RELEASE, and the reason it used to be is now recorded as
	// wrong: "released here and not at pickup — after a pickup the robot is still
	// in the lane holding the bin" is true only when the dropoff is in the SAME
	// lane as the pickup. For every other shape the robot picks, drives out, and
	// the row outlives its presence — which jammed five robots in front of an
	// empty lane on the rig (see HandleTransitForLaneGate).
	//
	// The exit release is there, on the pickup. This stays because it is still
	// the right release for the dropoff end: an order that PLACES into a lane was
	// never going to hit the pickup path for it, and a store's only visit ends
	// here.
	e.dispatcher.ReleaseLaneOccupancy(ev.OrderID)
	// ...and the lane just became free, so re-drive the reshuffle NOW rather than
	// waiting for this leg to reach a terminal status.
	//
	// THIS IS WHERE THE GAIN ACTUALLY COMES FROM, and without it removing the
	// sibling-in-flight guard changes nothing observable. A leg's occupancy ends
	// at its dropoff, but the ORDER ends later, at whole-order FINISHED; the only
	// caller of AdvanceCompoundOrder was child completion, so nothing ever
	// dispatched during the window between the two. The guard was gone and the
	// serialization remained, just enforced somewhere else.
	//
	// Re-driving here is what puts a second leg into the lane while the first is
	// still driving back. It is safe to call spuriously: AdvanceCompoundOrder
	// refuses a child whose lane is occupied, and refuses to dispatch any child
	// that already carries a VendorOrderID.
	if order.ParentOrderID != nil {
		if err := e.dispatcher.AdvanceCompoundOrder(*order.ParentOrderID); err != nil {
			e.logFn("engine: advance compound %d after a leg cleared its lane: %v", *order.ParentOrderID, err)
		}
	}
	if location == "" || location == strings.TrimSpace(order.DeliveryNode) {
		return // final delivery is recorded at whole-order FINISHED
	}

	binID, ok := e.resolveDropoffBin(order, location)
	if !ok {
		e.dbg("transit: order %d dropoff block %s @ %s — no in-flight claimed bin matched; store recorded at order FINISHED instead",
			ev.OrderID, ev.BlockID, ev.Location)
		return
	}

	destNode, err := e.db.GetNodeByDotName(location)
	if err != nil || destNode == nil {
		e.logFn("transit: order %d store dropoff @ %s — dest node lookup failed: %v", ev.OrderID, ev.Location, err)
		return
	}

	staged, expiresAt := e.resolveNodeStaging(destNode)
	// The bin physically leaves _TRANSIT — a real, addressable node — so the
	// event says so. This used to emit FromNodeID 0 to keep kanban's
	// produce-on-storage-exit check from firing, but that subscriber was
	// deleted in 2026-08: the zero silenced nobody while making "arrives from
	// _TRANSIT" indistinguishable from "emitter did not know". Verified at
	// 3098a615 that no live reader's behaviour changes: _TRANSIT resolves to
	// no CMS boundary (FindCMSBoundary) and no lane (LaneForNode), and the
	// other subscribers read PayloadCode or nothing. Pinned by
	// TestHandleStoreBlockCompleted_IntermediateDropoffCarriesRealFromNode.
	transit, terr := e.db.GetNodeByDotName(domain.TransitNodeName)
	if terr != nil || transit == nil {
		e.logFn("transit: order %d dropoff @ %s — cannot resolve %s for the From side: %v",
			order.ID, location, domain.TransitNodeName, terr)
		return
	}
	// Intermediate, not final — the early-return above already sent every drop
	// at the delivery node down the whole-order FINISHED path. So the order is
	// coming back for this bin and keeps its claim; handing it off here is what
	// stranded these bins at _TRANSIT (see ApplyIntermediateStore).
	evicted, err := e.binService.ApplyIntermediateStore(binID, destNode.ID, staged, expiresAt, order.ID)
	if err != nil {
		e.logFn("transit: order %d intermediate store arrival bin %d -> %s: %v", order.ID, binID, ev.Location, err)
		return
	}
	e.noteEvictedGhosts(evicted, "intermediate store", binID, ev.Location)

	e.dbg("transit: bin %d stored at %s on dropoff (order %d, block %s) — slot now reflects the physical bin",
		binID, ev.Location, order.ID, ev.BlockID)

	updated, uerr := e.db.GetBin(binID)
	if uerr != nil {
		e.logFn("transit: get bin %d after intermediate store arrival: %v", binID, uerr)
	}
	if updated != nil {
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      BinActionMoved,
			BinID:       updated.ID,
			PayloadCode: updated.PayloadCode,
			FromNodeID:  transit.ID,
			ToNodeID:    destNode.ID,
			NodeID:      destNode.ID,
			RobotID:     order.RobotID,
			OrderID:     order.ID,
		}})

		// Bind the arrived bin onto the Edge runtime if this dropoff node is an
		// Edge line node. A MULTI-BIN swap (single_robot carries the new bin IN and
		// the old bin OUT in one order) drops the new bin here as an INTERMEDIATE
		// dropoff, and the whole-order OrderDelivered then lands at the market — so
		// handleNodeOrderDelivered (single-bin only; no-ops on BinID==nil) never
		// binds it and the line node sits unbound (active_bin_id=NULL → "no bin /
		// starved", its PLC ticks attributed to nothing). Reuse the
		// UOPAdjustment{Bound} channel the admin-Move fix added (75643f9): the Edge
		// binds ONLY the process node it owns and no-ops for supermarket / staging /
		// synthetic dests, so it is safe to fire on every intermediate dropoff.
		//
		// NOT DORMANT, and this comment used to say it was. It claimed the broadcast
		// fires only for multi-bin swaps because "single-bin swaps have no junction
		// rows, so resolveDropoffBin returns false for them above". resolveDropoffBin
		// stopped reading the junction (see its own header) — it asks what this order
		// has at _TRANSIT — and a two_robot supply leg's intermediate dropoff at
		// InboundStaging resolves fine and reaches this broadcast. Harmless at Edge,
		// which no-ops for a staging node, but a reader reasoning from "dormant"
		// would be reasoning from a fact that has not held for some time.
		//
		// It is also load-bearing now, and not only for the tile: this is the
		// PLACEMENT RECORD half of a leg's departure. Edge's settleCellPlacement
		// hangs off this bind, and a leg that places at a cell stays undeparted —
		// holding the cell — until it lands. See shingo-edge/engine/leg_departure.go
		// and docs/order-lifecycle.md § Departed legs and cell-done.
		if err := e.SendDataToEdge(protocol.SubjectUOPAdjustment, protocol.StationBroadcast, &protocol.UOPAdjustment{
			BinID:        binID,
			CoreNodeName: destNode.Name,
			NewRemaining: updated.UOPRemaining,
			Epoch:        updated.DeltaEpoch,
			Bound:        true,
		}); err != nil {
			e.logFn("transit: bind-to-edge broadcast bin %d -> %s: %v", binID, destNode.Name, err)
		}
	}
}

// resolveDropoffBin finds the bin this order just set down at `location`: the
// one bin the order still has claimed at _TRANSIT — what the robot is carrying.
// A bin already delivered is unclaimed and not at _TRANSIT, which makes the
// caller idempotent against duplicate/replayed block events. Returns false when
// the order has no bin in transit under its claim, or more than one.
//
// It does NOT read the order_bins junction, and this summary used to say it did
// (dest_node == location, "returns false when no junction rows exist"). That
// describes the version this replaced; the paragraphs below document the
// rewrite and were correct while the summary above them was not.
// ── IT ASKS WHAT THE ROBOT IS CARRYING, AND IT USED TO ASK SOMETHING ELSE ──
//
// A dropoff places the bin the robot has in its forks. That bin is, by
// construction, the one this order most recently picked up and has not yet put
// down — which is the bin sitting at _TRANSIT under this order's claim. One
// robot carries one bin, so the question has exactly one answer and the database
// already holds it.
//
// WHAT IT ASKED BEFORE: it walked the order_bins junction for a row whose
// DEST_NODE equalled this location. That is a different question — "which bin
// ENDS UP here" — and it is only accidentally the same one when the dropoff
// being completed happens to be the bin's final destination.
//
// Two shapes therefore never matched, and both pin a robot forever:
//
//	INTERMEDIATE DROPS. The junction records PICKUP rows only (node_name = where
//	it is picked, dest_node = where it finally goes); there is no dropoff row to
//	match. A two_robot swap that drops at inbound staging on its way to the cell
//	has no row whose dest_node is that staging node, so the store was never
//	recorded, the bin never appeared at the staging slot, and the order's OWN
//	next step — a pickup at that slot — had nothing to pick.
//
//	SINGLE-BIN ORDERS. They carry no junction rows at all (the allocator writes
//	them only for multi-bin orders — see dispatch.binForStep), so the walk exited
//	at len(rows)==0. Its sibling resolvePickupBin has had the order.BinID
//	fallback all along; this half never got it.
//
// MEASURED, lane-stress rig 2026-08-11: orders 1, 7 and 10 staged from the first
// minute of the run, each holding an AMR, for the entire soak. Order 1 is the
// single-bin shape (bin 5, claimed by order 1, zero junction rows); 7 and 10 are
// the intermediate-drop shape (rows naming ALN_003/ALN_004 as dest while the
// block completed at SLN_003/SLN_004). Core logged "no in-flight claimed bin
// matched; store recorded at order FINISHED instead" and moved on, and the order
// never reached FINISHED because it was waiting on the step that store would
// have enabled.
//
// ONE RULE REPLACES TWO SPECIAL CASES. The obvious patch was to add the
// single-bin fallback and to match intermediate drops by step index. Both work,
// and both leave two readers of "which bin is this block about" answering with
// different machinery — which is the drift that produced this. Asking the plant
// what the robot is holding needs no junction, no step index, and no agreement
// with anything.
//
// FAILS CLOSED on an ambiguous answer. Zero bins in transit means the pickup was
// never recorded and there is nothing to place; more than one means this order
// has two bins in flight, which one robot cannot do — either way, guessing would
// record a bin at a slot it is not in, and a wrong location is worse than a late
// one (the order-FINISHED path still catches the honest case).
func (e *Engine) resolveDropoffBin(order *orders.Order, location string) (int64, bool) {
	transit, err := e.db.GetNodeByDotName(domain.TransitNodeName)
	if err != nil || transit == nil {
		e.logFn("transit: order %d dropoff @ %s — cannot resolve the %s node: %v",
			order.ID, location, domain.TransitNodeName, err)
		return 0, false
	}
	held, err := e.db.ListBinsByClaim(order.ID)
	if err != nil {
		e.logFn("transit: order %d dropoff @ %s — claimed-bin read failed: %v", order.ID, location, err)
		return 0, false
	}
	var carried []int64
	for _, b := range held {
		if b.NodeID != nil && *b.NodeID == transit.ID {
			carried = append(carried, b.ID)
		}
	}
	if len(carried) != 1 {
		e.dbg("transit: order %d dropoff @ %s — %d bin(s) in transit under this claim, want exactly 1",
			order.ID, location, len(carried))
		return 0, false
	}
	return carried[0], true
}

// IsPickupBlock returns true when a block's BinTask designates a
// pickup-shaped operation. The vendor's BinTask vocabulary is
// roboshop-configurable (the storage-bin-location action key), so we
// match on common patterns rather than an exact set.
//
// EXPORTED FOR ONE READER, AND THE REASON IS THE VOCABULARY. soakstat's
// bin-residence check reads the same BLOCK_FINISHED ledger rows this handler
// writes and has to classify them the same way. A second copy of a
// roboshop-CONFIGURABLE match would drift the moment a plant renames an action
// key — and it would drift silently, because the instrument would go on
// answering, just wrongly. One definition, two readers.
func IsPickupBlock(binTask string) bool {
	if binTask == "" {
		return false
	}
	t := strings.ToLower(binTask)
	switch t {
	case "load", "pickup", "pick", "jackload", "jack_load", "fork_load", "rollerload":
		return true
	}
	// Substring fallback: any binTask containing "load" or "pick" but
	// NOT "unload" / "drop" / "release" is treated as pickup-shaped.
	if strings.Contains(t, "unload") || strings.Contains(t, "drop") || strings.Contains(t, "release") {
		return false
	}
	if strings.Contains(t, "load") || strings.Contains(t, "pick") {
		return true
	}
	return false
}

// IsDropoffBlock returns true when a block's BinTask designates a
// dropoff-shaped operation — the store/deliver dual of IsPickupBlock. Same
// roboshop-configurable-vocabulary caveat, so it mixes exact-match with a
// substring fallback on "unload"/"drop"/"release". Exported for the same one
// reader, for the same reason.
func IsDropoffBlock(binTask string) bool {
	if binTask == "" {
		return false
	}
	t := strings.ToLower(binTask)
	switch t {
	case "unload", "dropoff", "drop", "jackunload", "jack_unload", "fork_unload", "rollerunload", "release":
		return true
	}
	if strings.Contains(t, "unload") || strings.Contains(t, "drop") || strings.Contains(t, "release") {
		return true
	}
	return false
}
