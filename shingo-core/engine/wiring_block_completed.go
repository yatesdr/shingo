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
	case isPickupBlock(ev.BinTask):
		e.handlePickupBlockCompleted(ev)
	case isDropoffBlock(ev.BinTask):
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
//  2. Single-bin order fallback: order.BinID.
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
			Action:      "moved",
			BinID:       updated.ID,
			PayloadCode: updated.PayloadCode,
			// FromNodeID intentionally 0: the bin arrives from _TRANSIT, not a
			// real slot, so kanban's produce-on-storage-exit check must not fire.
			ToNodeID: destNode.ID,
			NodeID:   destNode.ID,
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
		// Single-bin swaps (two_robot / simple) bind via handleNodeOrderDelivered and
		// have no junction rows, so resolveDropoffBin returns false for them above —
		// they never reach here. In production this is a dormant correctness add: it
		// only fires for multi-bin swaps, which previously left the at-node bin
		// unbound the same way the manual-Move path did before 75643f9.
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

// resolveDropoffBin finds the bin this order dropped at `location` via the
// order_bins junction (dest_node == location). Among matching rows it returns
// the bin still claimed by the order — the one actually in flight for this
// leg; a bin already delivered to the same dest is unclaimed and skipped,
// which makes the caller idempotent against duplicate/replayed block events.
// Returns false when no junction rows exist (single-bin orders, compound
// children) or none match.
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

// isPickupBlock returns true when a block's BinTask designates a
// pickup-shaped operation. The vendor's BinTask vocabulary is
// roboshop-configurable (the storage-bin-location action key), so we
// match on common patterns rather than an exact set.
func isPickupBlock(binTask string) bool {
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

// isDropoffBlock returns true when a block's BinTask designates a
// dropoff-shaped operation — the store/deliver dual of isPickupBlock. Same
// roboshop-configurable-vocabulary caveat, so it mixes exact-match with a
// substring fallback on "unload"/"drop"/"release".
func isDropoffBlock(binTask string) bool {
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
