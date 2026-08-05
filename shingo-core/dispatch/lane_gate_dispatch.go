package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// The gate_choreography valve — the UNIFORM gated shape.
//
// ⚖ Owner ruling (2026-07-21): EVERY lane-bound order on a gate_choreography
// group ships UNSEALED ending at the lane's wait point. There is NO bypass class
// for the uncontended case; an early append is what makes an open gate invisible.
// The rationale is that the bypass boundary is itself the defect generator — two
// dispatch shapes means two code paths, two sets of edge cases, and a boundary
// that drifts. One shape, always.
//
// So a lane-bound store is created as:
//
//	[pickup@source, wait@laneGatePoint]      Complete:false   (unsealed)
//
// and its tail
//
//	[dropoff@slot]                            complete:true    (seals it)
//
// is appended when the lane is safe. When the lane is ALREADY safe — the common
// case — the append happens immediately, in the same dispatch call, back to back
// with the create. The robot receives the tail long before it finishes its pickup,
// so it never dwells and the gate is invisible. What it costs is one extra block
// and one extra HTTP call per lane-bound store, not latency.
//
// The waybill shape, the durable state, and the append path are all the complex
// staging machinery, reused rather than re-implemented: Complete:false create
// (complex_dispatch.go), splitAtWait / splitSegment (complex_steps.go), and
// appendSegmentAndAdvance (complex_release.go, shared with operator release).
//
// DURABLE TRUTH LIVES ON THE ORDER ROW — status + vendor_order_id + wait_index +
// steps_json, exactly the columns complex staging already survives restarts on. No
// new table and no new column: a second cleanup path is a second thing to get
// wrong, and reservation rows are leases that the reaper deletes, so they can
// never hold this.
//
// KNOWN-INCOMPLETE (increment 3 of the build order, deliberately): when the
// classifier says the lane is contended, the order is created unsealed and then
// DWELLS with no release path — nothing appends its tail until the release
// evaluator lands in increment 4. That is expected behavior for this increment,
// asserted as such in the harness, and it is why the arm ships inert: no plant
// sets lane_enforcement=gate_choreography.

// PropLaneGatePoint is the node-property key, read on the LANE node, naming the
// RDS map point where a robot dwells while Core decides whether it may enter.
//
// It is a PROPERTY, not a Core node, on purpose. A Core node would join the node
// graph — dropoff-capacity accounting, lane child listings, the shuffle-slot pool
// — for a point that never holds a bin and is not part of the lane's geometry.
// The value is passed through to the fleet as a block location verbatim and is
// never resolved against nodes; only the fleet has to know it exists.
const PropLaneGatePoint = "lane_gate_point"

// laneGateTarget describes a destination that the valve owns: the lane node and
// the wait point its robots stage at.
type laneGateTarget struct {
	lane      *nodes.Node
	gatePoint string
}

// resolveLaneGateTarget reports whether destNode is a slot in a gate_choreography
// lane group, and if so returns the lane plus its configured wait point.
//
// ok=false for every other destination — not a lane slot, a lane with no group, a
// group on any other arm — so a plant that has not configured the arm never
// reaches the valve at all.
//
// A gate_choreography group whose lane has NO gate point configured is a
// MISCONFIGURATION, and it is reported as an error rather than silently falling
// back to the sealed shape. Falling back would reintroduce exactly the bypass
// class the uniform ruling exists to forbid, and it would do so invisibly, on the
// one lane an operator had explicitly asked to be gated.
func (d *Dispatcher) resolveLaneGateTarget(destNode *nodes.Node) (laneGateTarget, bool, error) {
	if destNode == nil {
		return laneGateTarget{}, false, nil
	}
	lane, err := d.db.LaneForNode(destNode.ID)
	if err != nil || lane == nil || lane.ParentID == nil {
		return laneGateTarget{}, false, err
	}
	if d.laneEnforcementMode(*lane.ParentID) != LaneEnforceGateChoreography {
		return laneGateTarget{}, false, nil
	}
	gatePoint := d.db.GetNodeProperty(lane.ID, PropLaneGatePoint)
	if gatePoint == "" {
		return laneGateTarget{}, false, fmt.Errorf(
			"lane %q is configured %s but has no %s property — a gated lane needs a wait point for its robots",
			lane.Name, LaneEnforceGateChoreography, PropLaneGatePoint)
	}
	return laneGateTarget{lane: lane, gatePoint: gatePoint}, true, nil
}

// resolveLaneGateSource is the retrieve-direction mirror of resolveLaneGateTarget:
// it reports whether sourceNode is a slot in a gate_choreography lane group, and if
// so returns that lane plus its wait point.
//
// A retrieve's SOURCE is the lane (it pulls a bin OUT); a store's DESTINATION is the
// lane (it drops a bin IN). The store valve keys on destination; the retrieve valve
// keys on source. Same lane, same gate point, same enforcement-mode check — the only
// difference is which end of the order the lane is on. ok=false for every other
// source, so an unconfigured plant never reaches the retrieve valve at all.
func (d *Dispatcher) resolveLaneGateSource(sourceNode *nodes.Node) (laneGateTarget, bool, error) {
	if sourceNode == nil {
		return laneGateTarget{}, false, nil
	}
	lane, err := d.db.LaneForNode(sourceNode.ID)
	if err != nil || lane == nil || lane.ParentID == nil {
		return laneGateTarget{}, false, err
	}
	if d.laneEnforcementMode(*lane.ParentID) != LaneEnforceGateChoreography {
		return laneGateTarget{}, false, nil
	}
	gatePoint := d.db.GetNodeProperty(lane.ID, PropLaneGatePoint)
	if gatePoint == "" {
		return laneGateTarget{}, false, fmt.Errorf(
			"lane %q is configured %s but has no %s property — a gated lane needs a wait point for its robots",
			lane.Name, LaneEnforceGateChoreography, PropLaneGatePoint)
	}
	return laneGateTarget{lane: lane, gatePoint: gatePoint}, true, nil
}

// buildGatedTransportPlan is buildTransportPlan with the lane's wait point spliced
// in before the dropoff: [pickup@source, wait@gatePoint, dropoff@delivery].
//
// splitAtWait then yields [pickup, wait] as the unsealed create (a wait WITH a node
// produces a real block, so the robot is told to drive there), and splitSegment at
// wait_index 0 yields [dropoff] as the tail with blockOffset 2 — so the appended
// block id continues the create's numbering instead of colliding with it.
func buildGatedTransportPlan(sourceNode, gatePoint, deliveryNode string, emptyPickup bool) []resolvedStep {
	return []resolvedStep{
		{Action: protocol.ActionPickup, Node: sourceNode, Empty: emptyPickup},
		{Action: protocol.ActionWait, Node: gatePoint},
		{Action: protocol.ActionDropoff, Node: deliveryNode},
	}
}

// buildGatedRetrievePlan is the retrieve-direction gated plan: a robot dwells at
// the lane's wait point FIRST, then — once the lane is safe — enters to pick the
// bin out of its lane slot and carry it to the line.
//
//	[wait@gatePoint, pickup@laneSlot, dropoff@delivery]   Complete:false at create
//
// The wait precedes the pickup because a retrieve has NO legal work to do before
// the lane opens — its first real action IS the pickup from the lane slot. (A
// store's pickup is at its line source, so it does real work before dwelling; a
// retrieve's pickup is the lane, so it must wait first.) This is the shape
// buried_retrieve_test.go's gatedRetrieveReq models, and what pre-positioning means:
// the robot drives to the gate during a dig so its approach overlaps the dig, then
// dwells until Core releases it.
//
// splitAtWait yields [wait] as the unsealed create (the wait-with-node is a real
// block, so the robot is told to drive to the gate and park there), and splitSegment
// at wait_index 0 yields [pickup, dropoff] as the tail with blockOffset 1 — so the
// appended block ids continue the create's single wait block instead of colliding.
func buildGatedRetrievePlan(gatePoint, laneSlot, deliveryNode string, emptyPickup bool) []resolvedStep {
	return []resolvedStep{
		{Action: protocol.ActionWait, Node: gatePoint},
		{Action: protocol.ActionPickup, Node: laneSlot, Empty: emptyPickup},
		{Action: protocol.ActionDropoff, Node: deliveryNode},
	}
}

// IsGateStaged reports whether a PLAIN order is currently parked at a lane gate
// holding an unsealed waybill — a robot physically committed at a wait point whose
// tail Core has not appended yet.
//
// Derived entirely from the durable order row, no new column:
//
//   - not coordinated — a complex order's staging is operator-owned and has its
//     own release path (HandleOrderRelease); this predicate is about Core's valve.
//   - carries a plan — a plain order gets steps_json ONLY from the valve. (Safe:
//     IsCoordinated reads the `coordinated` COLUMN, not steps-presence, so a plain
//     order carrying a plan still takes the plain dispatch branch. The v46
//     migration that backfilled coordinated from steps-presence has already run and
//     is version-guarded, so it cannot retro-flip these orders.)
//   - wait_index 0 — the tail has not been appended. appendSegmentAndAdvance
//     bumps it to 1 on success, so an order whose valve opened is not staged.
//   - has a vendor order — it reached the fleet, so a robot is actually committed.
//
// Used by the abandon sweep to refuse to auto-cancel a committed robot. The
// watchdog that gives such an order an operator surface is increment 7.
func IsGateStaged(order *orders.Order) bool {
	if order == nil || order.Coordinated {
		return false
	}
	return order.StepsJSON != "" && order.WaitIndex == 0 && order.VendorOrderID != ""
}

// dispatchGated is the valve: create the unsealed waybill, then append its tail
// immediately if the classifier says the lane is safe.
//
// Returns the vendor order id on success. A failure to CREATE is returned to the
// caller, which fails/requeues the order exactly as it does for the sealed path. A
// failure to APPEND is NOT fatal: the create succeeded, the robot is en route to
// the wait point, and the tail can be appended later — so the order is left staged
// and the error is logged rather than propagated. Retrying an append that may have
// landed would risk duplicate block ids (SEER's one contract on them is
// uniqueness), so the retry belongs in the evaluator, which re-derives the segment
// from durable state.
func (d *Dispatcher) dispatchGated(order *orders.Order, target laneGateTarget, sourceNode, destNode *nodes.Node) (string, error) {
	vendorOrderID := mintVendorOrderID(order.ID)

	plan := buildGatedTransportPlan(sourceNode.Name, target.gatePoint, destNode.Name,
		order.SourceIntent == SourceIntentEmpty)
	stepsJSON, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("marshal gated plan for order %d: %w", order.ID, err)
	}
	// Persist the plan BEFORE the fleet create. The tail is reconstructed from
	// steps_json at append time, so a crash between create and append must leave a
	// row the evaluator can still finish — writing it afterwards would open a
	// window where a committed robot has no recoverable tail.
	if err := d.db.UpdateOrderStepsJSON(order.ID, string(stepsJSON)); err != nil {
		return "", fmt.Errorf("persist gated plan for order %d: %w", order.ID, err)
	}
	order.StepsJSON = string(stepsJSON)

	preWait, hasWait := splitAtWait(plan)
	if !hasWait {
		return "", fmt.Errorf("gated plan for order %d has no wait step", order.ID)
	}
	blocks := stepsToBlocks(vendorOrderID, preWait, 0, d.loadSequenceForPayload(order.PayloadCode))

	priority := order.Priority
	if p, ok := d.laneDispatchPriority(destNode); ok {
		priority = p
	}
	req := fleet.CreateOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   priority,
		RobotGroup: d.robotGroupForPayload(order.PayloadCode),
		Complete:   false, // unsealed: the dropoff tail is appended when the lane is safe
	}
	d.dbg("lane gate: order=%d vendor=%s creating unsealed %s -> wait@%s (dest %s) priority=%d",
		order.ID, vendorOrderID, sourceNode.Name, target.gatePoint, destNode.Name, priority)

	if _, err := d.backend.CreateOrder(req); err != nil {
		d.dbg("lane gate: fleet create unsealed order failed: %v", err)
		return "", err
	}

	if err := d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", ""); err != nil {
		d.dbg("update order %d vendor: %v", order.ID, err)
	}
	order.VendorOrderID = vendorOrderID
	if err := d.lifecycle.Dispatch(order, vendorOrderID, "dispatcher"); err != nil {
		d.dbg("order %d → dispatched: %v", order.ID, err)
	}
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, sourceNode.Name, destNode.Name)

	// The valve. An admitted order gets its tail NOW, back to back with the create,
	// so the robot has the whole waybill before it finishes its pickup and never
	// dwells. A contended one is left unsealed at wait_index 0 for the evaluator.
	//
	// Under the SAME per-lane key the evaluator takes: from UpdateOrderVendor above,
	// this order already satisfies IsGateStaged and is already in the evaluator's
	// candidate set, so an evaluator pass firing now is looking at an order whose
	// tail this call is about to append. Classifier and append are one decision and
	// belong inside one critical section.
	//
	// Taken HERE and not around the whole function: the fleet create must not hold a
	// lane, and neither must the failure paths above, which run lifecycle
	// transitions whose events the evaluator itself subscribes to (the bus dispatches
	// synchronously on the emitting goroutine, so a lock held across one would
	// deadlock against itself).
	unlock := d.laneGates.lock(target.lane.ID)
	defer unlock()

	park, cause, err := d.laneEntryCause(target.lane, order, destNode)
	if err != nil {
		log.Printf("lane gate: order %d classifier errored (%v) — leaving staged for the evaluator", order.ID, err)
		return vendorOrderID, nil
	}
	if park {
		// KNOWN-INCOMPLETE until increment 4: nothing appends this tail yet.
		log.Printf("lane gate: order %d staged at %s for lane %s (%s) — awaiting release",
			order.ID, target.gatePoint, target.lane.Name, cause)
		return vendorOrderID, nil
	}
	if err := d.appendGateTail(order, "lane gate open"); err != nil {
		log.Printf("lane gate: order %d created but tail append failed (%v) — left staged, robot holds at %s",
			order.ID, err, target.gatePoint)
	}
	return vendorOrderID, nil
}

// dispatchGatedRetrieve is the retrieve-direction valve: create the unsealed
// [wait@gate] waybill, then append the [pickup@laneSlot, dropoff@delivery] tail
// immediately if the lane is safe, else leave it staged for the evaluator.
//
// Structurally parallel to dispatchGated (the store valve): same persist-before-create
// discipline, same non-fatal-append contract, same open-valve/contended split. Two
// differences, both forced by direction:
//
//  1. The plan's wait comes FIRST (buildGatedRetrievePlan), so the create is a single
//     wait block — the robot drives to the gate and parks with no other work to do,
//     because a retrieve's first real action IS the lane pickup and there is nothing
//     legal to do before the lane opens.
//  2. The release classifier keys on the SOURCE lane, not the dest. A retrieve is
//     blocked when a dig holds the lane (the mouth gate excludes it) or when the bin
//     it wants is buried behind a shallower one. laneGateRetrieveCause is the
//     retrieve-direction read of those conditions.
//
// sourceNode is the lane slot the bin sits in today; destNode is the line. The
// pickup slot may move while the order dwells (a dig can relocate the bin), so the
// tail's pickup is re-bound at release time (rebindGatedPickup), never trusted from
// the create.
func (d *Dispatcher) dispatchGatedRetrieve(order *orders.Order, target laneGateTarget, sourceNode, destNode *nodes.Node) (string, error) {
	vendorOrderID := mintVendorOrderID(order.ID)

	plan := buildGatedRetrievePlan(target.gatePoint, sourceNode.Name, destNode.Name,
		order.SourceIntent == SourceIntentEmpty)
	stepsJSON, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("marshal gated retrieve plan for order %d: %w", order.ID, err)
	}
	// Persist the plan BEFORE the fleet create — same crash-window reasoning as the
	// store valve: the tail is reconstructed from steps_json at append time, so a
	// crash between create and append must leave a row the evaluator can finish.
	if err := d.db.UpdateOrderStepsJSON(order.ID, string(stepsJSON)); err != nil {
		return "", fmt.Errorf("persist gated retrieve plan for order %d: %w", order.ID, err)
	}
	order.StepsJSON = string(stepsJSON)

	preWait, hasWait := splitAtWait(plan)
	if !hasWait {
		return "", fmt.Errorf("gated retrieve plan for order %d has no wait step", order.ID)
	}
	blocks := stepsToBlocks(vendorOrderID, preWait, 0, nil) // appended legs are never load-sequence expanded

	priority := order.Priority
	if p, ok := d.laneDispatchPriority(sourceNode); ok {
		priority = p
	}
	req := fleet.CreateOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   priority,
		RobotGroup: d.robotGroupForPayload(order.PayloadCode),
		Complete:   false, // unsealed: the [pickup, dropoff] tail is appended when the lane is safe
	}
	d.dbg("lane gate: order=%d vendor=%s creating unsealed retrieve wait@%s (pick %s -> %s) priority=%d",
		order.ID, vendorOrderID, target.gatePoint, sourceNode.Name, destNode.Name, priority)

	if _, err := d.backend.CreateOrder(req); err != nil {
		d.dbg("lane gate: fleet create unsealed retrieve failed: %v", err)
		return "", err
	}

	if err := d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", ""); err != nil {
		d.dbg("update order %d vendor: %v", order.ID, err)
	}
	order.VendorOrderID = vendorOrderID
	if err := d.lifecycle.Dispatch(order, vendorOrderID, "dispatcher"); err != nil {
		d.dbg("order %d → dispatched: %v", order.ID, err)
	}
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, sourceNode.Name, destNode.Name)

	// The valve. If the lane is safe now (no dig, bin reachable), append the tail back
	// to back with the create so the robot never dwells. Else leave it staged for the
	// evaluator to release on dig completion.
	//
	// Same per-lane key as the evaluator, same scope reasoning as the store valve.
	unlock := d.laneGates.lock(target.lane.ID)
	defer unlock()

	park, cause, err := d.laneGateRetrieveCause(target.lane, order)
	if err != nil {
		log.Printf("lane gate: retrieve order %d classifier errored (%v) — leaving staged for the evaluator", order.ID, err)
		return vendorOrderID, nil
	}
	if park {
		log.Printf("lane gate: retrieve order %d staged at %s for lane %s (%s) — awaiting release",
			order.ID, target.gatePoint, target.lane.Name, cause)
		return vendorOrderID, nil
	}
	if err := d.appendGateTail(order, "lane gate open (retrieve)"); err != nil {
		log.Printf("lane gate: retrieve order %d created but tail append failed (%v) — left staged, robot holds at %s",
			order.ID, err, target.gatePoint)
	}
	return vendorOrderID, nil
}

// appendGateTail appends the deferred [dropoff@slot] tail to a gate-staged order
// and seals it, binding the drop AT APPEND TIME.
//
// The binding is late by construction: the create carried no dropoff block at all,
// so there is no stale target to correct — the tail's node is taken from
// order.DeliveryNode as it stands at this instant, via the same pre-append rewrite
// the lane-gate release path uses for a redirect (patchRedirectSegments). That is
// what lets a later increment re-target a dwelling order simply by updating
// delivery_node.
//
// ── The durable guard, and why it lives HERE ──────────────────────────────
// Four sites funnel through this function: the valve's two (at create time, "was
// the lane open when I made this order") and the evaluator's two (later, "is it
// open now that something changed"). Same decision, two moments.
//
// The evaluator's callers reload the row and re-check IsGateStaged under the
// per-lane mutex before they get here. The valve's did not — it holds the struct
// it built moments earlier and never looks again. So a valve that lost the race
// would append a tail the evaluator had already appended, and duplicate block ids
// are the one thing SEER rejects outright.
//
// Serializing the valve is necessary but NOT sufficient on its own: whichever of
// the two waits on the mutex still has to notice what the other did while it
// waited, and only a reload can tell it. So the reload is the floor, taken by
// every caller, and it is what makes the guard a property of the ROW rather than
// of whichever in-memory struct happened to reach here first.
//
// The evaluator's callers keep their own reload as well — they need the fresh row
// for the rebind, not only for the guard — and a second read of one fact is not a
// second writer.
func (d *Dispatcher) appendGateTail(order *orders.Order, what string) error {
	if fresh, err := d.db.GetOrder(order.ID); err != nil {
		return fmt.Errorf("reload gated order %d before append: %w", order.ID, err)
	} else if fresh == nil || !IsGateStaged(fresh) {
		d.dbg("%s: order %d is no longer awaiting a tail (wait_index=%d) — another pass appended it",
			what, order.ID, waitIndexOf(fresh))
		return nil
	}

	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		return fmt.Errorf("parse gated plan for order %d: %w", order.ID, err)
	}
	segment, moreWaits, blockOffset := splitSegment(steps, order.WaitIndex)
	if segment == nil {
		return fmt.Errorf("gated order %d has no segment at wait_index %d", order.ID, order.WaitIndex)
	}
	d.patchRedirectSegments(segment, order, moreWaits)
	return d.appendSegmentAndAdvance(order, segment, moreWaits, blockOffset, what)
}

// waitIndexOf reports an order's wait index, or -1 when the row is gone. Only for
// the log line above, where a nil row and a zero index must not read the same.
func waitIndexOf(o *orders.Order) int {
	if o == nil {
		return -1
	}
	return o.WaitIndex
}
