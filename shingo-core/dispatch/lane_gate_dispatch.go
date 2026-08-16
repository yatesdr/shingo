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
// EVERY lane-bound order on a gate_choreography group ships UNSEALED, ending at
// the lane's wait point. There is NO bypass class for the uncontended case; an
// early append is what makes an open gate invisible.
//
// The bypass boundary would itself be the defect generator — two dispatch shapes
// means two code paths, two sets of edge cases, and a boundary that drifts. One
// shape, always.
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

// gateTargetForLane reports whether this LANE is gated, and if so returns it with
// the wait point its robots stage at.
//
// ok=false for a lane with no group or a group on any other arm, so a plant that
// has not configured the arm never reaches the valve at all.
//
// A gate_choreography group whose lane has NO gate point configured is a
// MISCONFIGURATION, and it is reported as an error rather than silently falling
// back to the sealed shape. Falling back would reintroduce exactly the bypass
// class the uniform ruling exists to forbid, and it would do so invisibly, on the
// one lane an operator had explicitly asked to be gated.
//
// It takes the LANE because the splice walks steps and already has one. The two
// functions this replaced - resolveLaneGateTarget(destNode) and its byte-for-byte
// twin resolveLaneGateSource(sourceNode) - existed only because the valve keyed
// on an END of the order and so had to be written once per end.
func (d *Dispatcher) gateTargetForLane(lane *nodes.Node) (laneGateTarget, bool, error) {
	if lane == nil || lane.ParentID == nil {
		return laneGateTarget{}, false, nil
	}
	// THE MARK IS THE WHOLE ANSWER. This used to ask two things — is the group
	// configured gate_choreography, and does the lane have a wait point — and had
	// to treat "configured but no point" as a misconfiguration error, because the
	// two could disagree. They cannot disagree now: there is one fact.
	gatePoint := d.laneWaitPoint(lane.ID)
	if gatePoint == "" {
		return laneGateTarget{}, false, nil
	}
	return laneGateTarget{lane: lane, gatePoint: gatePoint}, true, nil
}

// spliceLaneWait is THE TRANSFORM: it takes whatever plan an order already has
// and inserts a Core-owned wait immediately before the step that enters a gated
// lane. It does not author, and it does not care which end of the order the lane
// is on or how many steps the plan has.
//
// -- WHY THIS REPLACED TWO PLAN BUILDERS ------------------------------------
//
// buildGatedTransportPlan and buildGatedRetrievePlan wrote three-step plans from
// scratch, which is why coordinated orders had to be excluded from the valve:
// overwriting steps_json destroys an Edge-authored choreography. An INSERTED wait
// has nothing to destroy, so the exclusion has nothing left to protect and every
// order type transforms the same way, and the requirement falls out of the
// shape: everything before the wait is dispatched now, so an order does all the
// work it can before it dwells.
//
// -- THE RULES, ALL THREE ENFORCED HERE -------------------------------------
//
//  1. SPLICE ONLY WHERE STEPS ARE CONCRETE. A step with a blank node (a deferred
//     dropoff, resolved later by placeForDedicatedLoader) cannot be asked which
//     lane it enters. If one appears BEFORE the entry we picked, we cannot know
//     it is really the first, so this errors rather than guessing. A blank on a
//     plan with no gated lane at all is not this function's problem and passes
//     through untouched.
//
//  2. ONE GATED LANE PER PLAN. A second touch of the SAME lane is fine and
//     expected - a plan can drop into a slot and pick out of another. A touch of
//     a DIFFERENT gated lane is refused, because releasing per-wait needs
//     machinery this deliberately does not build (SYNTH-round6: per-wait release
//     is the plan-rewriting layer; earn it if a real plan ever needs it). The
//     second lane's protection is pre-dispatch admission, which is why the
//     unification landed first.
//
//  3. THE STEP AFTER THE WAIT MUST BE THE ENTRY. Asserted after the insert rather
//     than assumed from the loop, so a future edit that moves the index fails the
//     dispatch loudly instead of shipping a wait that gates nothing.
//
// Returns the plan unchanged with ok=false when no gated lane is on its path,
// which is every order at both plants today.
func (d *Dispatcher) spliceLaneWait(steps []resolvedStep) ([]resolvedStep, laneGateTarget, bool, error) {
	var (
		target   laneGateTarget
		entryIdx = -1
		blankAt  = -1
	)
	for i, s := range steps {
		if s.Action != protocol.ActionPickup && s.Action != protocol.ActionDropoff {
			continue
		}
		if s.Node == "" {
			if blankAt < 0 {
				blankAt = i
			}
			continue
		}
		node, err := d.db.GetNodeByDotName(s.Node)
		if err != nil || node == nil {
			continue // not a Core node we can classify; the claim path surfaces it
		}
		lane, err := d.db.LaneForNode(node.ID)
		if err != nil {
			return nil, laneGateTarget{}, false, fmt.Errorf("splice: resolve lane for %q: %w", s.Node, err)
		}
		if lane == nil {
			continue
		}
		t, gated, err := d.gateTargetForLane(lane)
		if err != nil {
			return nil, laneGateTarget{}, false, err
		}
		if !gated {
			continue
		}
		if entryIdx < 0 {
			target, entryIdx = t, i
			continue
		}
		if lane.ID != target.lane.ID {
			return nil, laneGateTarget{}, false, fmt.Errorf(
				"splice: plan touches two gated lanes (%s at step %d, %s at step %d) - "+
					"one gated lane per plan; releasing per-wait is not built",
				target.lane.Name, entryIdx, lane.Name, i)
		}
	}
	if entryIdx < 0 {
		return steps, laneGateTarget{}, false, nil // nothing gated on this path
	}
	if blankAt >= 0 && blankAt < entryIdx {
		return nil, laneGateTarget{}, false, fmt.Errorf(
			"splice: step %d has no node yet and precedes the gated entry at step %d - "+
				"the lane a plan enters first cannot be decided before its steps are concrete",
			blankAt, entryIdx)
	}

	out := make([]resolvedStep, 0, len(steps)+1)
	out = append(out, steps[:entryIdx]...)
	out = append(out, resolvedStep{
		Action:   protocol.ActionWait,
		Node:     target.gatePoint,
		WaitKind: WaitKindLane,
		WaitLane: target.lane.ID,
	})
	out = append(out, steps[entryIdx:]...)

	// Rule 3. The wait is at entryIdx; the step it gates is the one after it.
	if err := d.assertGatesTheEntry(out, entryIdx, target); err != nil {
		return nil, laneGateTarget{}, false, err
	}
	return out, target, true, nil
}

// assertGatesTheEntry checks that the step following the inserted wait really
// enters the lane the wait names. A mis-splice would ship a robot into a gated
// lane with its wait somewhere harmless - a gate that gates nothing, which reads
// as working.
func (d *Dispatcher) assertGatesTheEntry(steps []resolvedStep, waitIdx int, target laneGateTarget) error {
	if waitIdx+1 >= len(steps) {
		return fmt.Errorf("splice: wait inserted at %d with no step after it", waitIdx)
	}
	next := steps[waitIdx+1]
	node, err := d.db.GetNodeByDotName(next.Node)
	if err != nil || node == nil {
		return fmt.Errorf("splice: step after the wait (%q) does not resolve: %v", next.Node, err)
	}
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil {
		return fmt.Errorf("splice: resolve lane for %q: %w", next.Node, err)
	}
	if lane == nil || lane.ID != target.lane.ID {
		return fmt.Errorf("splice: wait names lane %s but the step after it (%q) is in %s - "+
			"the wait would gate nothing", target.lane.Name, next.Node, nodeName(lane))
	}
	return nil
}

// IsGateStaged reports whether an order is currently parked at a LANE wait
// holding an unsealed waybill — a robot physically committed at a wait point
// whose tail only Core can append.
//
// ── ONE QUESTION, WHERE THERE USED TO BE THREE PROXIES FOR IT ─────────────
//
// It asked: not coordinated, carries a plan, wait_index 0, has a vendor order.
// Three of those four were order-scoped stand-ins for a fact about a STEP, and
// each inverts the moment one plan can hold both kinds of wait:
//
//   - `!Coordinated` returns false for exactly the population a coordinated
//     lane wait creates, so the fence it guards would never fire.
//   - `StepsJSON != ""` is vacuous for a complex order — steps_json is where a
//     complex order lives.
//   - `WaitIndex == 0` is false for any plan whose operator wait comes FIRST,
//     which is the ordinary shape.
//
// So the predicate now reads the wait the order is actually parked at
// (waitAt, complex_steps.go — enumerating waits exactly as splitSegment does)
// and asks whether THAT wait is Core's. Everything else follows: the fence in
// HandleOrderRelease is correct in both directions, and the evaluator's
// candidate filter is too, with no other change at either site.
//
// STILL NO NEW COLUMN. The wait kind is durable in steps_json, which is already
// what this survives restarts on.
//
// The vendor-order term SURVIVES and is the one that was never a proxy: it is
// the witness that a robot is committed, which is what makes this a wait worth
// exempting from the abandon sweep rather than a plan detail.
//
// NO COMPATIBILITY FALLBACK — see the note below on why the shape it would
// catch cannot exist, and why catching it silently would be worse than not.
func IsGateStaged(order *orders.Order) bool {
	if order == nil || order.VendorOrderID == "" || order.StepsJSON == "" {
		return false
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		// A plan that cannot be parsed cannot be appended to either — every
		// releaser unmarshals the same bytes — so no answer here rescues the
		// order. Answer false and be LOUD: the abandon sweep's exemption keys on
		// this, so a silent false is a committed robot quietly losing its
		// exemption, and that is worth a log line rather than a shrug.
		log.Printf("lane gate: order %d has unparseable steps_json (%v) — treating as not gate-staged; "+
			"if a robot is dwelling for this order it is now sweep-eligible", order.ID, err)
		return false
	}
	w, ok := waitAt(steps, order.WaitIndex)
	return ok && w.WaitKind == WaitKindLane
}

// ── WHY THERE IS NO PRE-FIELD FALLBACK ARM ────────────────────────────────
//
// The obvious safety net — "no WaitKind AND !Coordinated AND wait_index 0 AND a
// plan, so assume the old derivation" — was specified and is deliberately not
// here. Two reasons, and the second is the one that decided it.
//
// IT CANNOT FIRE. A non-coordinated order gets steps_json from exactly one
// place: this file's two valve functions (lane_gate_dispatch.go, the
// UpdateOrderStepsJSON calls in dispatchGated / dispatchGatedRetrieve). Every
// other writer is complex-scoped — complex intake, complex re-resolve
// (complex_dispatch.go), the allocator's fungible-slot revert — and the two
// patchers (applyDeliveryNode / applySourceNode, loader_place.go) round-trip
// through resolvedStep, so they PRESERVE the stamp rather than dropping it.
// Both valve functions now stamp. So an unstamped lane wait would have to come
// from a build older than this field, and none can be in flight: the gate has
// never run at a plant, `lane_enforcement` is set on no node at either plant,
// and this branch has never been deployed to one.
//
// AND IT WOULD HIDE THE NEXT BUG. The splice lands next, and it becomes a THIRD
// producer of lane waits — in exactly the code most likely to forget the stamp.
// A fallback that reconstructs the old answer would make a missing stamp look
// like it worked, on the plain path, where the old derivation happens to agree.
// The failure would surface later, somewhere else, as a fence that does not
// hold. An unstamped wait must read as an operator wait and be visibly wrong at
// the fence, not invisibly right underneath it.
//
// If a pre-field row ever does appear, it reads as an operator wait: the
// evaluator ignores it and AbandonStuckOrders bounds it, which is the same
// disposition every other unreleasable order gets.

// dispatchGated is THE VALVE, one function for every direction and every order
// type: persist the spliced plan, create the unsealed waybill ending at the
// lane's wait point, then append its tail immediately if the lane is already
// safe.
//
// -- IT NO LONGER BUILDS A PLAN, AND THAT IS THE WHOLE CHANGE ---------------
//
// It used to be two functions - dispatchGated for stores and
// dispatchGatedRetrieve for retrieves - each of which AUTHORED a three-step plan
// with the lane at one end. They were near-identical (the file said so:
// "structurally parallel", "same persist-before-create discipline, same
// non-fatal-append contract") and differed only in which end the lane was on and
// therefore where the wait went.
//
// The wait now arrives already spliced into whatever plan the order has, so the
// end stops mattering: splitAtWait finds the wait wherever it is, the pre-wait
// half is the create, and everything after is the tail. A store's create happens
// to carry its pickup and drive; a retrieve's happens to be just the drive to the
// gate, because a retrieve has no legal work to do before the lane opens. That
// difference is now a property of the PLAN rather than of two code paths.
//
// -- WHAT DID NOT CHANGE --------------------------------------------------
//
// PERSIST BEFORE THE CREATE. The tail is reconstructed from steps_json at append
// time, so a crash between create and append must leave a row the evaluator can
// still finish; writing it afterwards would open a window where a committed robot
// has no recoverable tail.
//
// A failure to CREATE is returned to the caller, which fails or requeues the
// order exactly as it does for the sealed path. A failure to APPEND is NOT fatal:
// the create succeeded, the robot is en route to the wait point, and the tail can
// be appended later - so the order is left staged and the error is logged rather
// than propagated. Retrying an append that may have landed would risk duplicate
// block ids (SEER's one contract on them is uniqueness), so the retry belongs in
// the evaluator, which re-derives the segment from durable state.
func (d *Dispatcher) dispatchGated(order *orders.Order, target laneGateTarget, plan []resolvedStep, payloadCode string, loadSeq []string) (string, error) {
	vendorOrderID := mintVendorOrderID(order.ID)

	stepsJSON, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("marshal gated plan for order %d: %w", order.ID, err)
	}
	if err := d.db.UpdateOrderStepsJSON(order.ID, string(stepsJSON)); err != nil {
		return "", fmt.Errorf("persist gated plan for order %d: %w", order.ID, err)
	}
	order.StepsJSON = string(stepsJSON)

	preWait, hasWait := splitAtWait(plan)
	if !hasWait {
		return "", fmt.Errorf("gated plan for order %d has no wait step", order.ID)
	}
	// loadSeq comes from the CALLER, not from the payload, because the two create
	// sites answer it differently and always did: the plain path expands the load
	// leg for a configured payload (F4c), and complex is explicitly never expanded
	// ("F4c is scoped to the simple transport path"). Resolving it in here would
	// have silently started expanding complex orders the moment they began using
	// this valve.
	blocks := stepsToBlocks(vendorOrderID, preWait, 0, loadSeq)

	// The order's own priority - the depth-derived lane boost was deleted.
	req := fleet.CreateOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   order.Priority,
		RobotGroup: d.robotGroupForPayload(payloadCode),
		Complete:   false, // unsealed: the tail is appended when the lane is safe
	}
	d.dbg("lane gate: order=%d vendor=%s creating unsealed %d block(s) -> wait@%s (lane %s)",
		order.ID, vendorOrderID, len(blocks), target.gatePoint, target.lane.Name)

	// Claim, commit, name it - see fleet_handover.go. It also sets
	// order.VendorOrderID, which the valve below relies on: IsGateStaged requires
	// it, and the tail is appended from this struct.
	if err := d.handoverToFleet(order, req, "dispatcher"); err != nil {
		return "", err
	}
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, order.SourceNode, order.DeliveryNode)

	// The valve. An admitted order gets its tail NOW, back to back with the
	// create, so the robot has the whole waybill before it finishes its pre-lane
	// work and never dwells. A contended one is left unsealed for the evaluator.
	//
	// Under the SAME per-lane key the evaluator takes: from the handover above,
	// this order already satisfies IsGateStaged and is already in the evaluator's
	// candidate set, so an evaluator pass firing now is looking at an order whose
	// tail this call is about to append. Classifier and append are one decision
	// and belong inside one critical section.
	//
	// Taken HERE and not around the whole function: the fleet create must not hold
	// a lane, and neither must the failure paths above, which run lifecycle
	// transitions whose events the evaluator itself subscribes to (the bus
	// dispatches synchronously on the emitting goroutine, so a lock held across one
	// would deadlock against itself).
	unlock := d.laneGates.lock(target.lane.ID)
	defer unlock()

	// Direction off the plan, exactly as the evaluator reads it.
	entry, isRetrieve, ok := laneEntryAfterWait(plan, 0)
	if !ok {
		log.Printf("lane gate: order %d has no actionable step after its wait - leaving staged", order.ID)
		return vendorOrderID, nil
	}
	entryNode, err := d.db.GetNodeByDotName(entry.Node)
	if err != nil || entryNode == nil {
		log.Printf("lane gate: order %d lane entry %q unresolvable (%v) - leaving staged for the evaluator",
			order.ID, entry.Node, err)
		return vendorOrderID, nil
	}

	v, err := d.gateEntryVerdict(target.lane, order, entryNode, isRetrieve)
	if err != nil {
		log.Printf("lane gate: order %d classifier errored (%v) - leaving staged for the evaluator", order.ID, err)
		return vendorOrderID, nil
	}
	if !v.Admitted() {
		log.Printf("lane gate: order %d staged at %s for lane %s (%s) - awaiting release",
			order.ID, target.gatePoint, target.lane.Name, v.Cause())
		return vendorOrderID, nil
	}
	if err := d.appendGateTail(order, "lane gate open"); err != nil {
		log.Printf("lane gate: order %d created but tail append failed (%v) - left staged, robot holds at %s",
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

	// HOLD B FOR A GATED ENTRY, and the moment is HERE rather than at the create.
	//
	// The plain path takes occupancy when it hands the order to the fleet, because
	// that is when the robot is sent into the lane. A gated order's create sends it
	// to the GATE POINT — outside the lane, which is the entire purpose — so taking
	// there would declare a robot present in a corridor it is deliberately parked
	// outside of, and would wall every other entrant for as long as it dwells.
	// The tail append is the moment it actually goes in, and all four append sites
	// (both valves, both evaluator releases) funnel through here.
	//
	// The lane comes off the WAIT STEP (WaitLane), not off the order's endpoint
	// columns: it is the lane whose evaluator owns this wait, recorded when the
	// wait was minted, and it is right for both directions without asking which
	// end the lane is on. First consumer of the field.
	//
	// Taken BEFORE the append and released if the append fails — same ordering
	// discipline as the plain path, and here it is strictly available because the
	// append is the only irreversible step left.
	w, ok := waitAt(steps, order.WaitIndex)
	if !ok {
		return fmt.Errorf("gated order %d has no wait at wait_index %d", order.ID, order.WaitIndex)
	}
	if err := d.takeLaneOccupancyByID(order.ID, w.WaitLane); err != nil {
		return err
	}
	if err := d.appendSegmentAndAdvance(order, segment, moreWaits, blockOffset, what); err != nil {
		// The robot never got the tail, so it is still at the gate, still outside.
		d.ReleaseLaneOccupancy(order.ID)
		return err
	}
	return nil
}

// waitIndexOf reports an order's wait index, or -1 when the row is gone. Only for
// the log line above, where a nil row and a zero index must not read the same.
func waitIndexOf(o *orders.Order) int {
	if o == nil {
		return -1
	}
	return o.WaitIndex
}
