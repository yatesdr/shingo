package dispatch

import (
	"encoding/json"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// The gated-lane valve — the UNIFORM gated shape.
//
// EVERY lane-bound order on a gated group ships UNSEALED, ending at
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
// sets a gate mark.
//
// ── "SHIPS INERT" IS NOT FREE, AND THE PARAGRAPH ABOVE READS AS IF IT IS ──
//
// Inert is the right word for the WAIT: with no mark, nothing dwells and nothing
// is left un-appended, so the increment is genuinely safe unfinished. It is the
// wrong word for what the same flag also switches off. The gated arm is the only
// caller of rebindGatedDropoff, which is the only place a dropoff slot is
// re-resolved after the order was planned — so "no plant sets a gate mark" also
// says, without meaning to, that no plant has ever late-bound a dropoff.
//
// What an unmarked lane does instead is bind at dispatch and drive. The slot is
// then as old as the trip, and the plant wears the two consequences that shows
// up as: an order queued on `dropoff-occupied`, and an air bubble (an empty slot
// sealed behind a shallower one, because several stores each picked correctly
// and then arrived in an order nobody chose). Neither is a fault in this file.
// Both are the cost of the sentence above, and it should be read as a cost
// rather than as a reassurance.

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
// A gated group whose lane has NO gate point configured is a
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
	// configured as gated, and does the lane have a wait point — and had
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
//  2. A WAIT PER GATED LANE THE PLAN ENTERS. This rule USED TO BE "one gated lane
//     per plan", with a second one refused outright, and the refusal was terminal
//     — a swap whose pickup sits in one marked lane and whose drop-off sits in
//     another was FAILED at dispatch. That is demand terminated for a request
//     with nothing wrong with it, which is the one thing the wait-not-fail law
//     forbids, and the shape becomes ordinary the moment a plant marks a second
//     lane.
//
//     What the refusal was protecting was never expressible-per-plan: it was
//     "releasing per-wait is not built". It is built, and it always was —
//     splitSegment already walks to an arbitrary wait index and reports whether
//     more remain, appendSegmentAndAdvance already leaves an order unsealed when
//     they do, and IsGateStaged already reads the wait the order is parked AT
//     rather than assuming there is one. The assert was the only thing standing
//     in front of machinery that could already do this; nothing per-wait had to
//     be added, so the loop below is the whole of the change.
//
//     Each wait is released independently by its OWN lane's admission, on that
//     lane's own events. The robot picks in the first lane, drives to the second
//     lane's mark, and dwells there until that lane is safe.
//
//     A SECOND TOUCH OF THE SAME LANE IN A ROW IS STILL ONE WAIT — a plan can
//     drop into a slot and pick out of another, and the robot never left. What
//     starts a new wait is a lane CHANGE, which is also why a step outside any
//     gated lane resets the tracking: a plan that leaves lane A for the line and
//     comes back is re-entering, and re-entering is what a gate is for.
//
//  3. THE STEP AFTER EACH WAIT MUST BE THE ENTRY IT GATES. Asserted after the
//     inserts rather than assumed from the loop, so a future edit that moves an
//     index fails the dispatch loudly instead of shipping a wait that gates
//     nothing.
//
// Returns the plan unchanged with ok=false when no gated lane is on its path,
// which is every order at both plants today. The laneGateTarget returned is the
// FIRST gate — the one the create sends the robot to, and the only one the
// dispatch-time valve is about.
func (d *Dispatcher) spliceLaneWait(steps []resolvedStep) ([]resolvedStep, laneGateTarget, bool, error) {
	// gate is one inserted wait: where it goes, and which lane it guards.
	type gate struct {
		idx    int
		target laneGateTarget
	}
	var (
		gates   []gate
		blankAt = -1
		// inside is the gated lane the robot is currently in, as the walk sees it.
		// 0 means "not in a gated lane", which is both the start state and what a
		// step somewhere else resets it to.
		inside int64
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
			inside = 0 // somewhere we cannot classify is somewhere else
			continue   // not a Core node we can classify; the claim path surfaces it
		}
		lane, err := d.db.LaneForNode(node.ID)
		if err != nil {
			return nil, laneGateTarget{}, false, fmt.Errorf("splice: resolve lane for %q: %w", s.Node, err)
		}
		if lane == nil {
			inside = 0
			continue
		}
		t, gated, err := d.gateTargetForLane(lane)
		if err != nil {
			return nil, laneGateTarget{}, false, err
		}
		if !gated {
			inside = 0
			continue
		}
		if lane.ID == inside {
			continue // still in the lane the last wait let it into
		}
		gates = append(gates, gate{idx: i, target: t})
		inside = lane.ID
	}
	if len(gates) == 0 {
		return steps, laneGateTarget{}, false, nil // nothing gated on this path
	}
	// Rule 1, against the LAST gate rather than the first: a blank anywhere ahead
	// of a gated entry makes the plan's lane ORDER undecidable, and the order is
	// what the waits encode. Same conservatism as before, applied to the whole
	// sequence instead of to a single entry.
	//
	// ── THE OUTBOUND DWELL WAS CHECKED AGAINST THIS AND IT IS UNAFFECTED ──
	//
	// Recorded rather than left to be re-derived, because "a plan whose tail is not
	// chosen yet" and "a plan with a blank step" sound like the same thing and are
	// not. blankAt tracks PICKUP AND DROPOFF steps carrying no node — a deferred
	// destination that some later resolver will fill IN PLACE, which is what makes
	// the lane sequence undecidable: the blank might turn out to be a lane entry
	// sitting between two others. A dwelling dig leg has no such step. Its tail is
	// not blank, it is ABSENT, and it is appended after the wait rather than
	// resolved into a gap before it — so the lane order the waits encode is
	// complete as it stands, and every step this walk sees is concrete.
	//
	// The one thing that would break the equivalence is a dwell plan that ALSO
	// carried a deferred dropoff. digDwellPlan authors exactly two concrete steps,
	// so there is none to have.
	if last := gates[len(gates)-1].idx; blankAt >= 0 && blankAt < last {
		return nil, laneGateTarget{}, false, fmt.Errorf(
			"splice: step %d has no node yet and precedes the gated entry at step %d - "+
				"the lanes a plan enters cannot be sequenced before its steps are concrete",
			blankAt, last)
	}

	out := make([]resolvedStep, 0, len(steps)+len(gates))
	next := 0
	for i, s := range steps {
		if next < len(gates) && gates[next].idx == i {
			out = append(out, resolvedStep{
				Action:   protocol.ActionWait,
				Node:     gates[next].target.gatePoint,
				WaitKind: WaitKindLane,
				WaitLane: gates[next].target.lane.ID,
			})
			next++
		}
		out = append(out, s)
	}

	if err := d.assertEachWaitGatesItsEntry(out); err != nil {
		return nil, laneGateTarget{}, false, err
	}
	if err := assertEveryWaitDeclaresAnOwner(out); err != nil {
		return nil, laneGateTarget{}, false, err
	}
	return out, gates[0].target, true, nil
}

// assertEveryWaitDeclaresAnOwner is W1's drift test on the CORE side, armed on
// the dispatch path rather than left in a test file.
//
// Every wait in a plan Core is about to persist must say who advances it: the
// ones this function just inserted are WaitKindLane, and the ones it copied
// through came from the station and carry WaitKindStation. A wait with neither
// is unowned — no fence claims it, no floor covers it, and the board cannot say
// whether to offer Release. That is the shape that held three robots for a whole
// soak (§12.49), and it is worth refusing a plan over.
//
// ── THE DRAIN WINDOW IS WHY THIS WARNS RATHER THAN REFUSES, FOR NOW ───────
//
// Plans authored before the stamp existed are still in flight, and they carry no
// kind at all. Refusing them would fail live orders for a field they could not
// have had. So an untagged wait is LOUD and allowed — IsStationWait still reads
// it as the station's, the historical default — and this returns an error only
// for a kind that is set to something unrecognised, which can only be a new
// author disagreeing with the vocabulary.
//
// WHEN THE WINDOW CLOSES: turn the log into a returned error, and delete
// IsStationWait's `== ""` arm. Both halves in the same commit, or an untagged
// wait becomes unowned at the fence while still passing here.
func assertEveryWaitDeclaresAnOwner(steps []resolvedStep) error {
	for i, s := range steps {
		if s.Action != protocol.ActionWait {
			continue
		}
		switch s.WaitKind {
		case WaitKindLane, WaitKindStation:
			continue
		case "":
			log.Printf("WAIT OWNERSHIP: step %d (%q) carries no wait_kind — reading it as station-owned "+
				"for the drain window. A wait nobody claims is one no fence guards and no floor sweeps; "+
				"if this is a NEW plan rather than one authored before the field, its author is missing "+
				"a stamp", i, s.Node)
		default:
			return fmt.Errorf("splice: step %d (%q) declares wait_kind %q, which is neither %q nor %q — "+
				"an unrecognised owner is a wait no fence will claim",
				i, s.Node, s.WaitKind, WaitKindLane, WaitKindStation)
		}
	}
	return nil
}

// assertEachWaitGatesItsEntry checks that the step following every lane wait
// really enters the lane that wait names. A mis-splice would ship a robot into a
// gated lane with its wait somewhere harmless - a gate that gates nothing, which
// reads as working.
//
// It walks the whole plan rather than checking one index, because a plan can now
// carry several waits and a fault in the second would be invisible to a check
// aimed at the first. Only WaitKindLane waits are examined: an operator wait is
// somebody else's and gates nothing by design.
//
// ── THE OUTBOUND ARM: A WAIT CAN GATE THE APPEND INSTEAD OF AN ENTRY ──────
//
// This was written when every lane wait was an INBOUND wait, and it encodes that
// assumption twice: it requires a step after the wait, and requires that step to
// enter the wait's lane. Both are properties of asking permission to go IN.
//
// A dig leg's outbound dwell asks the opposite question. The robot is already in
// the lane, standing in the slot the dig just emptied with the blocker on its
// deck, and what it is waiting for is Core to choose a destination and APPEND
// the tail. There is no step after the wait because the step after the wait is
// exactly what has not been decided — so the inbound form would refuse every dig
// in the plant, which is how this arm was found: the moment a MARKED lane was
// dug, the splice returned "lane wait at 2 with no step after it" and the leg was
// parked.
//
// SO THE ARM IS POSITIVE, NOT AN EXEMPTION (law 10's dual). "There is nothing
// after it" on its own would also accept a truncated inbound plan, which is the
// mis-splice this function exists to catch. What is asserted instead is the
// dweller's identity: it STANDS IN THE LANE IT NAMES, and it got there by picking
// out of that same lane. An inbound wait cannot satisfy that — its node is the
// lane's mark, a map-point property that is deliberately not a Core node, and the
// splice never puts a lane wait immediately after a pickup from its own lane (a
// second touch of the same lane in a row is one wait, not two).
func (d *Dispatcher) assertEachWaitGatesItsEntry(steps []resolvedStep) error {
	for i, s := range steps {
		if s.Action != protocol.ActionWait || s.WaitKind != WaitKindLane {
			continue
		}
		if i+1 >= len(steps) {
			if err := d.assertWaitGatesAnAppend(steps, i); err != nil {
				return err
			}
			continue
		}
		next := steps[i+1]
		node, err := d.db.GetNodeByDotName(next.Node)
		if err != nil || node == nil {
			return fmt.Errorf("splice: step after the wait (%q) does not resolve: %v", next.Node, err)
		}
		lane, err := d.db.LaneForNode(node.ID)
		if err != nil {
			return fmt.Errorf("splice: resolve lane for %q: %w", next.Node, err)
		}
		if lane == nil || lane.ID != s.WaitLane {
			return fmt.Errorf("splice: the wait at step %d names lane %d but the step after it (%q) "+
				"is in %s - the wait would gate nothing", i, s.WaitLane, next.Node, nodeName(lane))
		}
	}
	return nil
}

// assertWaitGatesAnAppend is the outbound arm's body: the wait at index i ends
// the plan, so it must be a dweller standing in the lane it names, having lifted
// out of that same lane.
//
// Two facts, and each of them is what makes the dweller releasable rather than
// the silent trap an unidentified outbound wait would be:
//
//	the wait's NODE is a slot in WaitLane  — so the lane it names is the lane it
//	                                         is in: an evaluator, a floor, a cause
//	                                         vocabulary and a tripwire all key on it
//	the step BEFORE it is a pickup there   — so it is holding a blocker out of that
//	                                         lane, which is what makes an append,
//	                                         rather than an entry, the thing it waits for
func (d *Dispatcher) assertWaitGatesAnAppend(steps []resolvedStep, i int) error {
	s := steps[i]
	at, err := d.db.GetNodeByDotName(s.Node)
	if err != nil || at == nil {
		return fmt.Errorf("splice: the lane wait at step %d ends the plan, so it must be an outbound "+
			"dwell — but its own node %q does not resolve (%v), and a wait naming no real place is one "+
			"no evaluator and no floor can find", i, s.Node, err)
	}
	lane, err := d.db.LaneForNode(at.ID)
	if err != nil {
		return fmt.Errorf("splice: resolve lane for the dwell position %q: %w", s.Node, err)
	}
	if lane == nil || lane.ID != s.WaitLane {
		return fmt.Errorf("splice: the lane wait at step %d ends the plan and names lane %d, but it "+
			"stands at %q in %s - an outbound wait must be INSIDE the lane it names",
			i, s.WaitLane, s.Node, nodeName(lane))
	}
	if i == 0 {
		return fmt.Errorf("splice: the lane wait at step %d ends the plan with nothing before it - "+
			"an outbound wait gates the append of a tail for a bin the robot is already holding, so "+
			"something must have picked it up", i)
	}
	prev := steps[i-1]
	if prev.Action != protocol.ActionPickup {
		return fmt.Errorf("splice: the lane wait at step %d ends the plan but the step before it is a "+
			"%s, not a pickup - an outbound wait is where a robot holds a bin it has just lifted",
			i, prev.Action)
	}
	from, err := d.db.GetNodeByDotName(prev.Node)
	if err != nil || from == nil {
		return fmt.Errorf("splice: the pickup before the outbound wait at step %d (%q) does not resolve: %v",
			i, prev.Node, err)
	}
	fromLane, err := d.db.LaneForNode(from.ID)
	if err != nil {
		return fmt.Errorf("splice: resolve lane for %q: %w", prev.Node, err)
	}
	if fromLane == nil || fromLane.ID != s.WaitLane {
		return fmt.Errorf("splice: the outbound wait at step %d names lane %d but the pickup before it "+
			"(%q) is in %s - a dweller waits in the lane it dug out of",
			i, s.WaitLane, prev.Node, nodeName(fromLane))
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
// never run at a plant, no lane carries a mark at either plant,
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
		Vehicle:    pinnedVehicleFor(order),
		// The claim's routing hints, if it configured any. Nil/empty is SEER
		// auto-pick, which is every order in the plant until one does.
		KeyRoute: order.KeyRoute,
		KeyTask:  order.KeyTask,
		Complete: false, // unsealed: the tail is appended when the lane is safe
	}
	d.dbg("lane gate: order=%d vendor=%s creating unsealed %d block(s) -> wait@%s (lane %s)",
		order.ID, vendorOrderID, len(blocks), target.gatePoint, target.lane.Name)

	// Claim, commit, name it - see fleet_handover.go. It also sets
	// order.VendorOrderID, which the valve below relies on: IsGateStaged requires
	// it, and the tail is appended from this struct.
	//
	// ── IT DECLARES THE PRE-WAIT SEGMENT'S NODES, AND IT USED TO DECLARE NONE ──
	//
	// The old statement here was "the create ends at the wait point OUTSIDE the
	// corridor, so there is no presence to record yet". That is true of the GATED
	// lane and false of everything else the create does on the way to it. A plan is
	// spliced immediately before the step entering the gated lane, so every step
	// BEFORE that goes out in this create — and those steps can work another lane
	// entirely.
	//
	// A reshuffle leg is the ordinary case rather than a corner: it picks a blocker
	// out of one lane and parks it in another, and if only the PARK lane is marked,
	// the create sends a robot into the unmarked pickup lane holding nothing. That
	// is F-12's shape exactly — a robot inside a corridor that admission reports as
	// empty to the next entrant — reached through the gated door instead of the
	// complex one.
	//
	// FOUND BY THE SEAM ASSERTION, ON THE RIG, WITHIN MINUTES. It refused the
	// dispatch of a dig leg (LSD_011 in LS_D3 → LSD_004 in the marked LS_D1), which
	// failed the leg, the parent, and the two-robot swap behind it. The guard was
	// right and the arm was wrong: the fix is to declare, not to relax the guard.
	// It is also why "all four arms already satisfied it" was too strong when the
	// guard landed — the characterization plans had no pre-wait lane touch, and the
	// plant produced one immediately.
	//
	// planNodes is the same walk admitPlan and the complex arm use, so the read and
	// the write see one set. The gate point contributes nothing: it is a property,
	// never resolved against nodes, which is what still makes the gated lane's own
	// row appendGateTail's to take.
	if err := d.commitToFleet(order, req, "dispatcher", d.planNodes(preWait)...); err != nil {
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
	entry, _, isRetrieve, ok := laneEntryAfterWait(plan, 0)
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
// so there is no stale target to correct. The tail's nodes are read from the PLAN,
// which the re-bind has already patched by carried index through applyPlanNode —
// the one writer of a step's node.
//
// ── WHY THERE IS NO REDIRECT OVERLAY HERE ANY MORE ────────────────────────
// This function used to call patchRedirectSegments on the segment first, and the
// paragraph above used to claim that is "what lets a later increment re-target a
// dwelling order simply by updating delivery_node". Both are gone, and the reason
// is the swap clobber (PLAN §R.5) surviving on the emit path for a week after it
// was declared fixed.
//
// The overlay is a BACKWARD LAST-DROPOFF SCAN keyed on order.DeliveryNode. Before
// the re-bind those two agreed, so it rewrote a dropoff to itself and was
// invisible. After the re-bind, order.DeliveryNode IS THE LANE SLOT — so on a swap
// ([dropoff@lane, pickup@empties, dropoff@press]) the scan found the press, saw it
// differ, and re-aimed the empty's return leg at the lane slot the same order had
// just filled. Both of the order's bins went to one node on the wire while
// steps_json stayed correct, which is why the steps_json signature query that
// "confirmed" 768a2985 could not see it, and why 768a2985's "no backward scan
// survives on the gate path" was false at this line.
//
// The re-target mechanism the old doc described has no live caller for a gated
// complex order. Every delivery_node writer is excluded: PrepareRedirect refuses
// order.Coordinated outright (dispatcher.go), redirectStoreOffDugLane sits behind
// the scanner's IsCoordinated fork so it is plain-path only, the planner's NGRP
// re-resolve runs before a plan exists, the compound arm rewrites steps_json in
// the same block, and applyDeliveryNodeAtStep patches the step itself. A column
// written without its step is a state this path can no longer be reached in — and
// if one is ever added, the fix is to write the step through applyPlanNode like
// every other re-point, not to re-derive the target here by scanning.
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
	//
	// ── FOR AN OUTBOUND DWELL, WaitLane IS THE LANE BEING LEFT ────────────
	//
	// The take below is then a genuine no-op rather than a mistake: the robot
	// entered that lane at its leg's dispatch and Core has held the row across the
	// dwell, so AcquireOccupancy's NOT EXISTS guard on (order, node) finds the row
	// already there and inserts nothing. Verified rather than assumed, because it
	// is what lets the one append door serve both directions. The dweller's own
	// resolver drops that row after this returns — the robot is driving out, which
	// is the moment the lane frees.
	w, ok := waitAt(steps, order.WaitIndex)
	if !ok {
		return fmt.Errorf("gated order %d has no wait at wait_index %d", order.ID, order.WaitIndex)
	}
	// ── THE ROLLBACK GIVES BACK WHAT THIS CALL TOOK, AND NOTHING ELSE ────────
	//
	// The take reports which of its two idempotent outcomes happened, and that —
	// not the plan's shape — decides the rollback. The plan-shape guard this
	// replaced (waitGatesAnAppend here) was STRUCTURALLY DEAD on exactly the arm
	// it was written for: the dwell's release binds the tail BEFORE appending
	// (bindDwellTail — the plan first, the column second, §R.5's crash ordering),
	// so by the time this function reads the plan the dwell wait has an
	// actionable step after it and `dwelling` read false for every dweller. The
	// failed dweller append then ran the INBOUND rollback — an order-wide
	// ReleaseLaneOccupancy that also dropped the dweller's dispatch-time
	// source-lane row — declaring an occupied corridor empty to the next leg.
	// Two robots nose to tail, from a guard that never once fired true.
	//
	// The take's own insert is the only witness that survives both writers: it
	// says "took" for an inbound append entering an open row, and "already
	// there" for a dweller re-taking the row its dispatch took. The rollback
	// keys on it, per-lane through ReleaseOccupancyForLane rather than the
	// order-wide release, so a leg inside TWO lanes loses neither row to the
	// other's failure.
	took, err := d.takeLaneOccupancyByID(order.ID, w.WaitLane)
	if err != nil {
		return err
	}
	if err := d.appendSegmentAndAdvance(order, segment, moreWaits, blockOffset, what); err != nil {
		// The robot never got the tail. For an INBOUND wait that means it is still
		// at the gate, still outside, and the row this call took must go — THIS
		// lane's row, not every lane's: the corridor the robot was sent into is
		// the one whose row is a leftover.
		//
		// FOR A DWELLER, took=false: this call inserted nothing, so there is
		// nothing to give back, and the source-lane row its dispatch took stands
		// until the dwell's own release drops it — releasing here would be the
		// phantom-absence twin of §R.54's phantom row, a robot standing in a
		// corridor the table says is empty.
		//
		// AND THE INBOUND SENTENCE HAS ITS OWN EXCEPTION (§R.98 stage A3). An
		// AppendLandedError says the fleet took the segment and the failure is
		// downstream of it: the robot is driving INTO this lane right now, so the
		// row is not a leftover to clean up, it is the true one. Rolling it back
		// declares the same occupied corridor empty, arrived at from the other
		// direction.
		if took && !IsAppendLanded(err) {
			if relErr := reservations.ReleaseOccupancyForLane(d.db.DB, order.ID, w.WaitLane); relErr != nil {
				log.Printf("lanegate: rollback occupancy for order %d on lane %d after failed append: %v",
					order.ID, w.WaitLane, relErr)
			}
		}
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
