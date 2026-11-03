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
// Idempotence: BinService.Lift moves nothing (Lift.Moved false) when the bin is
// already at _TRANSIT, so duplicate or replayed events are safe; the tote hook
// runs only on a lift that moved the bin.
//
// Which cart a block moves is the order's row for that lift (an order's carts
// are a row per lift: resolvePickupBinRow, resolveDropoffBin). A block no row
// names moves nothing: we log and drop, and the cart moves to its drop at
// whole-order FINISHED, the pre-Phase-2 behavior.

package engine

import (
	"encoding/json"
	"fmt"
	"strings"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/fleet/seerrds"
	"shingocore/service"
	"shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/telemetry"
)

// handleBlockCompleted is called from wiring.go's EventBlockCompleted
// subscription. Routes the block by kind and drives the corresponding
// bin lifecycle transition: pickups free the source slot (→ _TRANSIT),
// intermediate storage dropoffs record the bin at its slot immediately.
func (e *Engine) handleBlockCompleted(ev BlockCompletedEvent) {
	// The order, read once for the leg and the count below. nil when it cannot
	// be read: the leg is still recorded, without its robot.
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		order = nil
	}
	// Record the observation before routing it. This handler was the sole
	// subscriber and it threw the block away after moving the bin, so every
	// leg the fleet reported — travel-to-source, load, travel-to-dest, unload —
	// existed for the duration of one function call and was never written down.
	//
	// A block already recorded is a replay (a restarted poller reports every
	// finished block again): it is counted once, the first time. The bin
	// handlers below see it again, as they always did; they are idempotent on
	// the bin's own state.
	if e.recordBlockLeg(ev, order) {
		e.countStepDone(ev, order)
	}

	switch {
	case IsPickupBlock(ev.BinTask):
		e.handlePickupBlockCompletedFor(ev, order)
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

// countStepDone advances the order's steps_done when the block finished the
// plan's next open step (domain.StepsDoneAfter): one write for such a block,
// none for any other. The route strip reads where the robot is from it, and
// TestBlockFinished_EveryBlockSentReachesTheSubscriber is why it can: every
// block the dispatcher sends reaches this handler as FINISHED. Best-effort,
// like the leg: a lost count draws the robot a stop behind until its next
// block, and must never stop a bin from moving.
func (e *Engine) countStepDone(ev BlockCompletedEvent, order *orders.Order) {
	if order == nil {
		return
	}
	n, ok := domain.StepsDoneAfter(order, ev.Location, blockAction(ev.BinTask))
	if !ok || n <= order.StepsDone {
		return
	}
	if err := e.db.AdvanceOrderStepsDone(order.ID, n); err != nil {
		e.logFn("route: count %d steps done for order %d: %v", n, order.ID, err)
	}
}

// blockAction is the plan action a block of binTask does, as this handler
// reads the vendor's vocabulary: IsPickupBlock, IsDropoffBlock, and the Wait
// binTask a wait at a node is sent as. "" for a block of no step's kind, such
// as the turns of an advanced load sequence around its one load.
func blockAction(binTask string) string {
	switch {
	case IsPickupBlock(binTask):
		return protocol.ActionPickup
	case IsDropoffBlock(binTask):
		return protocol.ActionDropoff
	case strings.EqualFold(binTask, seerrds.BinTaskForAction(protocol.ActionWait)):
		return protocol.ActionWait
	}
	return ""
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
// It reports whether the block is new: false only when the order already has
// the block's row, the replay of a block counted before. A row that cannot be
// written is new, so a lost telemetry row never stops a step from counting.
//
// Best-effort: a failure here logs and returns. Losing a telemetry row must
// never stop the bin from moving.
func (e *Engine) recordBlockLeg(ev BlockCompletedEvent, order *orders.Order) bool {
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
		return true
	}

	// The robot comes from the ORDER. Block events carry no vehicle at all —
	// neither the poller's EmitBlockCompleted nor the simulator's has ever had
	// one — so without this every leg row would have a blank robot_id and the
	// per-robot leg breakdown would be empty for the same reason the mission
	// summaries were.
	robotID := ""
	if order != nil {
		robotID = order.RobotID
	}

	detail := fmt.Sprintf("block %s @ %s (binTask=%s)", ev.BlockID, ev.Location, ev.BinTask)
	if leg.DurationSeconds > 0 {
		detail += fmt.Sprintf(" took %ds", leg.DurationSeconds)
	}

	fresh, err := e.db.InsertBlockLeg(&telemetry.Event{
		OrderID:       ev.OrderID,
		VendorOrderID: ev.VendorOrderID,
		RobotID:       robotID,
		BlocksJSON:    string(blocks),
		ErrorsJSON:    "[]",
		Detail:        detail,
	}, BlockLegState, ev.BlockID)
	if err != nil {
		e.logFn("telemetry: record block leg for order %d: %v", ev.OrderID, err)
		return true
	}
	return fresh
}

// handlePickupBlockCompleted drives the cart a pickup block lifted onto the
// synthetic _TRANSIT node so the source slot frees immediately — the
// slot-vacancy signal queued orders need to unblock.
func (e *Engine) handlePickupBlockCompleted(ev BlockCompletedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		order = nil
	}
	e.handlePickupBlockCompletedFor(ev, order)
}

// handlePickupBlockCompletedFor is handlePickupBlockCompleted with the order
// already read (handleBlockCompleted reads it once for the leg, the count and
// this). An order that could not be read lifts nothing: its carts are not
// known, and a wrong cart on _TRANSIT is worse than a late one.
func (e *Engine) handlePickupBlockCompletedFor(ev BlockCompletedEvent, order *orders.Order) {
	if order == nil {
		e.logFn("transit: order %d block %s @ %s — the order could not be read; nothing lifted",
			ev.OrderID, ev.BlockID, ev.Location)
		return
	}
	bin, stepIndex, fromNodeID, ok := e.resolvePickupBinRow(order, ev.Location)
	if !ok {
		e.logFn("transit: order %d block %s @ %s — no cart of this order stands here to lift; it moves to its drop at delivery",
			ev.OrderID, ev.BlockID, ev.Location)
		return
	}

	// Item 11: notify Edge that the bin was physically picked up. The
	// SEND PARTIAL BACK flow needs this signal to flush the released
	// bin's delta accumulator and advance the active claim. We publish
	// for every pickup (not just partial-back) — the Edge handler
	// no-ops gracefully when the order doesn't match a tracked bin.
	//
	// THE LIFT AND ITS NOTICE ARE ONE TRANSACTION (Lane H, DECISIONS D11
	// (i)): the lift ends a streamed bin's generation, and the pickup message
	// carrying the new Epoch and the lift time is that bump's announcement. It
	// used to be the UPDATE, then a separate outbox row after it; a crash
	// between them moved the bin and told nobody.
	notice := service.PickupNotice{OrderID: ev.OrderID, Location: ev.Location, OrderUUID: order.EdgeUUID}
	notice.Station, _ = materialStation(order)
	binID := bin.ID
	lift, err := e.binService.Lift(binID, notice)
	if err != nil {
		e.logFn("transit: lift bin %d for order %d: %v", binID, ev.OrderID, err)
		return
	}
	if lift.Moved {
		e.deriveTotesAtLift(bin, order, ev.OrderID)
	}

	e.dbg("transit: bin %d entered _TRANSIT (order %d, block %s @ %s, step %d)",
		binID, ev.OrderID, ev.BlockID, ev.Location, stepIndex)

	e.Events.Emit(Event{Type: EventBinEnteredTransit, Payload: BinEnteredTransitEvent{
		BinID:      binID,
		OrderID:    ev.OrderID,
		FromNodeID: fromNodeID,
		StepIndex:  stepIndex,
	}})
}

// deriveTotesAtLift writes what a tote press build drew from the cart outside
// it, when that cart has just been lifted (store.DB.DeriveTotesAtLift). It acts
// only on a cart with no part and a count of totes, judged on the row the lift
// already read, so any other lift (every lift at a plant that sets no tote
// numbers) costs no statement. It runs after the lift has committed, in its
// own transaction: a failure is logged and never fails the lift, its notice or
// the transit event. Pins: TestToteLift_* (tote_lift_docker_test.go).
func (e *Engine) deriveTotesAtLift(bin *bins.Bin, order *orders.Order, orderID int64) {
	if bin.PayloadCode != "" || bin.TotesOnCart == nil || bin.NodeName == "" {
		return
	}
	sibling := ""
	if order != nil {
		sibling = order.SiblingOrderUUID
	}
	if err := e.db.DeriveTotesAtLift(bin.ID, orderID, bin.NodeName, *bin.TotesOnCart, sibling); err != nil {
		e.logFn("totes: derive at the lift of bin %d off %s for order %d: %v", bin.ID, bin.NodeName, orderID, err)
	}
}

// resolvePickupBin is resolvePickupBinRow for an order by id, answering with
// the bin's id. It reads the order; the block handler passes the one it read.
func (e *Engine) resolvePickupBin(orderID int64, location string) (binID int64, stepIndex int, fromNodeID int64, ok bool) {
	order, err := e.db.GetOrder(orderID)
	if err != nil || order == nil {
		return 0, 0, 0, false
	}
	bin, stepIndex, fromNodeID, ok := e.resolvePickupBinRow(order, location)
	if !ok {
		return 0, 0, 0, false
	}
	return bin.ID, stepIndex, fromNodeID, true
}

// resolvePickupBinRow is the cart a pickup block at location lifts, as its row
// stood before the lift (the lift's tote hook judges the cart on it), the plan
// step that lifts it, and the node it is leaving.
//
// ONE READ OVER THE ORDER'S ROWS. The order's carts are a row per lift (read
// with the order, no statement); the candidates are the rows whose pick step
// happens at location (domain.StepNode), and the one statement asks which of
// those carts is still recorded THERE — the lift has not happened yet. The
// earliest such pick wins. A relay's re-lift has its own row (the single_robot
// swap's fresh cart re-lifted at inbound staging), so nothing is inferred from
// what the order happens to have claimed at the node, and nothing falls back to
// orders.bin_id.
//
// THE BIN'S RECORD MUST STAND ON THE BLOCK'S NODE. A lift writes last_lift_at,
// and that is the fact a vacate hold releases on (bins.LeftSince), so naming a
// cart that stands elsewhere — a replayed pickup after a restart, a re-pickup
// whose cart is parked at another node — releases a hold for a lift that never
// happened. Measured on the demo plant, 2026-09-06, order 8 at ALN_004: a
// re-pickup at inbound staging resolved to the spent carrier parked at outbound
// staging, put it on the line with its own count, and wedged the cell
// (ISSUE-sim-position-hold-deadlock-2026-09-06.md). Pinned by
// TestPickupBlock_FallbackDoesNotLiftABinRecordedElsewhere.
//
// AN ORDER THAT HAS ENDED STILL LIFTS. A Core cancel untracks nothing, and the
// poller diffs blocks before it checks the order's state (rds/poller.go), so a
// pickup that finished on the floor reaches here after the order is cancelled.
// Its row survives the end, and the lift is recorded where the cart physically
// is: on _TRANSIT, unclaimed, for the stranded sweep. Refusing it would leave
// the cart recorded at its slot while it rides a robot (finding 7).
//
// A LIFT WITH NO ROW AT THIS NODE (an appended pickup the plan grew after its
// rows were written) lifts the one cart the order has claimed here, and writes
// that lift's row in its own statement: the plan's first pickup at this node
// with no row, set down at the plan's next dropoff after it, open when there is
// none.
func (e *Engine) resolvePickupBinRow(order *orders.Order, location string) (bin *bins.Bin, stepIndex int, fromNodeID int64, ok bool) {
	// Trim only — NOT case-fold; case mismatch is a real config error. The
	// location is an un-normalized RDS vendor string (same contract as
	// shingo-edge/engine/handler_bin_picked_up.go).
	loc := strings.TrimSpace(location)
	if loc == "" {
		return nil, 0, 0, false
	}
	var ids []int64
	pickOf := map[int64]int{}
	for _, c := range order.Carts {
		if c.PickStep == nil || *c.PickStep < 0 || strings.TrimSpace(domain.StepNode(order, *c.PickStep)) != loc {
			continue
		}
		if _, seen := pickOf[c.BinID]; !seen {
			ids = append(ids, c.BinID)
			pickOf[c.BinID] = *c.PickStep
		}
	}
	standing, err := e.db.BinsStandingAt(ids, order.ID, loc)
	if err != nil {
		e.logFn("transit: order %d pickup @ %s — read its carts there: %v", order.ID, loc, err)
		return nil, 0, 0, false
	}
	if len(ids) == 0 {
		return e.appendedLift(order, loc, standing)
	}
	if len(standing) == 0 {
		return nil, 0, 0, false
	}
	b := standing[0]
	from := int64(0)
	if b.NodeID != nil {
		from = *b.NodeID
	}
	return b, pickOf[b.ID], from, true
}

// appendedLift is a pickup at a node where the order has no row: exactly one of
// its claimed carts stands there, and its lift's row is written now. Zero or
// several is not a lift this handler can name.
func (e *Engine) appendedLift(order *orders.Order, loc string, claimed []*bins.Bin) (*bins.Bin, int, int64, bool) {
	if len(claimed) != 1 {
		if len(claimed) > 1 {
			e.logFn("transit: order %d pickup @ %s — no row lifts here and %d of its claimed carts stand here; nothing lifted",
				order.ID, loc, len(claimed))
		}
		return nil, 0, 0, false
	}
	b := claimed[0]
	steps := domain.OrderPlan(order)
	rowed := map[int]bool{}
	for _, c := range order.Carts {
		if c.PickStep != nil {
			rowed[*c.PickStep] = true
		}
	}
	pick := -1
	for i, s := range steps {
		if s.Action == protocol.ActionPickup && !rowed[i] && strings.TrimSpace(s.Node) == loc {
			pick = i
			break
		}
	}
	if pick < 0 {
		e.logFn("transit: order %d pickup @ %s — its plan has no pickup here without a row; cart %d not lifted",
			order.ID, loc, b.ID)
		return nil, 0, 0, false
	}
	if err := e.db.RecordAppendedLift(order, b.ID, pick); err != nil {
		e.logFn("transit: order %d pickup @ %s — record the lift of cart %d: %v", order.ID, loc, b.ID, err)
		return nil, 0, 0, false
	}
	from := int64(0)
	if b.NodeID != nil {
		from = *b.NodeID
	}
	return b, pick, from, true
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
//   - Only an intermediate set-down is recorded here, for any order: a cart
//     whose row sets it down at this node and that is in hand. A set-down no
//     row names (a drop the order makes with nothing of its own in hand) is
//     left to the completion path.
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
		// Final delivery is recorded at whole-order FINISHED, and its landing
		// goes with it (applyArrival writes it in that move's transaction, Lane
		// H, H.2): the reason for returning here stands, and the bind gap it
		// left (no Bound for the final drop) is closed there.
		return
	}

	binID, ok := e.resolveDropoffBin(order, location)
	if !ok {
		e.dbg("transit: order %d dropoff block %s @ %s — no in-flight claimed bin matched; store recorded at order FINISHED instead",
			ev.OrderID, ev.BlockID, ev.Location)
		e.noteUnannouncedLineRebind(order, location)
		return
	}

	destNode, err := e.db.GetNodeByDotName(location)
	if err != nil || destNode == nil {
		e.logFn("transit: order %d store dropoff @ %s — dest node lookup failed: %v", ev.OrderID, ev.Location, err)
		return
	}

	staged := e.resolveNodeStaging(destNode)
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
	evicted, err := e.binService.ApplyIntermediateStore(binID, destNode.ID, staged, nil, order.ID)
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

		// The Edge hears of this placement from the landing ApplyIntermediateStore
		// wrote in the move's own transaction (Lane H, H.2; service.announceLanding),
		// addressed to the station claiming the node, or the order's (H.3). This
		// used to be a UOPAdjustment{Bound} broadcast here, after the commit: a
		// crash between the two left the carrier on the line unbound. It is still
		// the PLACEMENT RECORD half of a leg's departure (the Edge's
		// settleCellPlacement hangs off the bind; shingo-edge/engine/
		// leg_departure.go, docs/order-lifecycle.md § Departed legs and
		// cell-done). An Edge too old for the landing gets that Bound, addressed
		// to it (H.7).
		//
		// The old comment's facts stand for the landing: a single_robot swap's
		// fresh carrier is placed on the line as an INTERMEDIATE drop and the
		// whole-order delivered lands at the market, so this is the only bind it
		// gets; and a staging node that is no station's position is told nothing
		// worth binding (the Edge binds by the node's name).
	}
}

// lineRebindUnannouncedAction is the recovery_actions verb for a line placement
// whose rebind announcement Core could not send.
const lineRebindUnannouncedAction = "line_rebind_unannounced"

// noteUnannouncedLineRebind makes resolveDropoffBin's decline AUDIBLE when the
// dropoff it declined is the order's own process node.
//
// ── WHAT GOES SILENT WITHOUT IT ────────────────────────────────────────────
//
// A single_robot consume swap places the fresh carrier on the line at step 7 of
// nine, and that placement is an INTERMEDIATE dropoff (the order ends at the
// market at step 9). The rebind rides the landing ApplyIntermediateStore writes,
// and that happens only if resolveDropoffBin resolved a cart. Since the set-down
// is resolved by its own row (an order's carts are a row per lift), a lost
// earlier store event no longer makes it decline; it declines when the cart its
// row names is NOT IN HAND — its lift was never recorded (the pickup block was
// lost), or it was set down already.
//
// The decline returns quietly. Edge never hears the announcement, and every arm
// of HandleUOPAdjustment that could bind an empty slot refuses one on purpose,
// so the cell is left with a full carrier standing on it, active_bin_id NULL,
// and no releaser. The only trace is the e.dbg line above: nothing at info,
// nothing durable, no alarm naming the node. One missed step five steps earlier,
// and silence.
//
// ── WHY IT IS A recovery_actions ROW AND NOT DeliveredNotBound ─────────────
//
// DeliveredNotBound is the right FAMILY — it exists for "a bin arrived at a node
// we own and nothing bound", and it carries exactly the instruction an operator
// needs here. It lives on the EDGE (shingo-edge/engine/wiring_delivered.go), and
// this decline is known only to CORE. There is no Core→Edge alarm subject on the
// wire to carry it across, and inventing one to reach a handler is a wire change
// that should be decided rather than assumed.
//
// So the alarm is raised where the fact is, through Core's established durable
// surface for "something that should have happened did not" — the same table the
// lane liveness floor, the chapter floor and the dig standoff tripwire write to.
// It carries the SAME operator instruction, so the front door the operator is
// sent to is unchanged, and that door genuinely works: a count correction lands
// as a UOPAdjustment and binds through HandleUOPAdjustment's repair arm.
//
// ── NARROW, BECAUSE A FLOOR THAT CRIES WOLF STOPS BEING READ ───────────────
//
// Only a decline at the order's OWN PROCESS NODE is alarmed. The identical
// decline at a staging or supermarket slot is ordinary — the order is coming
// back for that bin, and two existing tests cover those shapes — and stays at
// debug. Alarming those would put a row on the board for every intermediate
// store in the plant, which is how the one row that matters gets skipped.
//
// Best-effort: a failure to record must never stop the handler. Nothing about
// the bind seam's behaviour changes here — this path already returned without
// binding, and it still does. All that is added is that it says so.
//
// The alarm stays a BACKSTOP: what is left to decline is a set-down whose cart's
// lift Core never heard, which no row can repair. The operator's "Record Count"
// is the recovery for that.
func (e *Engine) noteUnannouncedLineRebind(order *orders.Order, location string) {
	if order == nil || order.ProcessNode == "" || location != order.ProcessNode {
		return
	}
	detail := fmt.Sprintf(
		"order %d placed a bin at %s but Core could not resolve WHICH bin it set down "+
			"(no single claimed bin at _TRANSIT), so no rebind was announced to the edge. "+
			"The cell may now hold a carrier with nothing bound to it. "+
			"Record Count on the bin tab to bind it",
		order.ID, location)
	e.logFn("transit: REBIND NOT ANNOUNCED — %s", detail)
	if err := e.db.RecordRecoveryAction(lineRebindUnannouncedAction, "order", order.ID, detail, "system"); err != nil {
		e.logFn("transit: record unannounced-rebind alarm for order %d: %v", order.ID, err)
	}
}

// resolveDropoffBin finds the cart this order just set down at `location`: the
// cart of the order's row whose drop step happens there, claimed by the order
// and in the robot's hands (on _TRANSIT or a robot's carrier node). A cart
// already set down is not in hand, which makes the caller idempotent against
// duplicate and replayed block events.
//
// ── THE ROW SAYS WHICH CART; THE PLANT SAYS WHETHER IT IS IN HAND ──────────
//
// The set-down is asked by its step, not inferred. This used to count the
// order's claimed bins at _TRANSIT and decline unless exactly one stood there,
// so an earlier step that did not land — a store event lost — left two carts
// in transit and the next set-down gave up silently (its own TODO,
// 2026-09-10, asked for the dropoff step to carry the cart it places; a row per
// lift does). Measured on the lane-stress rig 2026-08-11 before that: orders 1,
// 7 and 10 staged from the first minute of the run, each holding an AMR,
// because a set-down at an intermediate staging node matched no row whose
// dest_node was the cart's final drop.
//
// Which cart is in the forks is a fact about the plant (bins.InHandAmong), never
// stamped on the row: of two candidates in hand, the one lifted last is the one
// being set down. One statement; none when the order's plan sets nothing down
// here.
//
// FAILS CLOSED: no candidate in hand means there is nothing to place, and a
// wrong location is worse than a late one (the order-FINISHED path still
// catches the honest case).
func (e *Engine) resolveDropoffBin(order *orders.Order, location string) (int64, bool) {
	loc := strings.TrimSpace(location)
	var ids []int64
	for _, c := range order.CartsSettingDownAt(loc) {
		ids = append(ids, c.BinID)
	}
	if len(ids) == 0 {
		e.dbg("transit: order %d dropoff @ %s — no row of this order sets a cart down here", order.ID, loc)
		return 0, false
	}
	bin, ok, err := e.db.CartInHandAmong(ids, order.ID)
	if err != nil {
		e.logFn("transit: order %d dropoff @ %s — read which of its carts is in hand: %v", order.ID, loc, err)
		return 0, false
	}
	if !ok {
		e.dbg("transit: order %d dropoff @ %s — none of carts %v is in hand under this order", order.ID, loc, ids)
	}
	return bin, ok
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
