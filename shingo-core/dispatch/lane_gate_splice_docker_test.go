//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// lane_gate_splice_docker_test.go — the transform, on the population it was
// built for.
//
// The valve used to AUTHOR a three-step plan, which is why coordinated orders
// were excluded from it: overwriting steps_json destroys an Edge-authored
// choreography. An inserted wait has nothing to destroy. These are the tests that
// could not exist while the valve authored.
//
// The plain shapes are NOT re-asserted here: lane_gate_dispatch_docker_test.go
// and lane_gate_retrieve_docker_test.go already pin them step by step, and they
// pass unchanged against the splice — which is the byte-identical claim, made by
// the tests that were written against the builders the splice replaced.

// complexIntoGatedLane submits a complex store whose dropoff is a slot of a
// gated lane, and returns the order after dispatch.
func complexIntoGatedLane(t *testing.T, d *Dispatcher, db *store.DB, env *protocol.Envelope,
	uuid, sourceNode, slotName, payload string) *orders.Order {
	t.Helper()
	return submitComplexAndDispatch(t, d, db, env, &protocol.ComplexOrderRequest{
		OrderUUID:   uuid,
		PayloadCode: payload,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: sourceNode},
			{Action: "dropoff", Node: slotName},
		},
	})
}

// TestSplice_ComplexStoreDoesItsPreLaneWorkThenDwells is the owner's motivating
// case, made executable.
//
// "I don't want a compound order which may have 5, 7, 10 steps to queue because a
// lane block. It should do all the work it can up until the lane."
//
// A complex store into a contended gated lane: the PICKUP and the drive go out
// on the create, and only the drop into the lane waits. Before the transform this
// order took no gate at all — it drove straight in — and the alternative on the
// table (hold it pre-dispatch) would have wasted both.
func TestSplice_ComplexStoreDoesItsPreLaneWorkThenDwells(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	sd := testdb.SetupStandardData(t, db)
	laneID, s0, _ := gateChoreoLane(t, db, "SPL-CX", "SPL-CX-GATE")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "SPL-CX-BIN")

	// A dig holds the lane, so the order must dwell rather than seal at once.
	digger := testdb.CreateOrder(t, db)
	if !d.laneLock.TryLock(laneID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}

	o := complexIntoGatedLane(t, d, db, testEnvelope(), "spl-cx",
		sd.StorageNode.Name, s0.Name, sd.Payload.Code)
	if o.VendorOrderID == "" {
		t.Fatalf("complex order was not dispatched (status %s)", o.Status)
	}

	// The PLAN was transformed, not replaced: the Edge-authored pickup and dropoff
	// are both still there, with a wait inserted between them.
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(o.StepsJSON), &steps); err != nil {
		t.Fatalf("parse persisted plan: %v", err)
	}
	if len(steps) != 3 {
		t.Fatalf("plan has %d steps, want 3 (%+v) — the authored pickup and dropoff must survive "+
			"with one wait spliced between them", len(steps), steps)
	}
	if steps[0].Action != protocol.ActionPickup || steps[0].Node != sd.StorageNode.Name {
		t.Errorf("step 0 = %+v, want the authored pickup at %s", steps[0], sd.StorageNode.Name)
	}
	if steps[1].Action != protocol.ActionWait || steps[1].WaitKind != WaitKindLane || steps[1].WaitLane != laneID {
		t.Errorf("step 1 = %+v, want a lane wait for lane %d", steps[1], laneID)
	}
	if steps[2].Action != protocol.ActionDropoff || steps[2].Node != s0.Name {
		t.Errorf("step 2 = %+v, want the authored dropoff at %s", steps[2], s0.Name)
	}

	// THE PRE-LANE WORK WENT OUT. The create carries the pickup and the drive to
	// the gate; only the lane entry is withheld.
	creates := backend.CreateRequests()
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	if creates[0].Complete {
		t.Error("the create sealed the order; a gated order ships UNSEALED")
	}
	if n := len(creates[0].Blocks); n != 2 {
		t.Fatalf("create carried %d blocks, want 2 (pickup + wait). The requirement: "+
			"the order does all the work it can before it dwells", n)
	}
	if creates[0].Blocks[0].Location != sd.StorageNode.Name {
		t.Errorf("first block location = %q, want the pickup at %s — the pre-lane work",
			creates[0].Blocks[0].Location, sd.StorageNode.Name)
	}
	if creates[0].Blocks[1].Location != "SPL-CX-GATE" {
		t.Errorf("second block location = %q, want the gate point", creates[0].Blocks[1].Location)
	}

	// And the tail is withheld: the lane is dug.
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("append calls = %d, want 0 — a dig holds the lane", n)
	}
	if !IsGateStaged(o) {
		t.Fatalf("a coordinated order dwelling at a lane wait is not gate-staged (wait=%d vendor=%q). "+
			"Nothing but the evaluator can release it", o.WaitIndex, o.VendorOrderID)
	}
}

// TestSplice_FenceHoldsOnASplicedPlan is the fence, both directions, on a plan
// the SPLICE produced rather than one a test hand-built.
//
// The hand-built version (lane_gate_wait_kind_docker_test.go) proved the
// predicate. This proves the producer and the predicate agree — that the splice
// stamps what IsGateStaged reads, end to end, with no test-authored plan in
// between.
func TestSplice_FenceHoldsOnASplicedPlan(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	sd := testdb.SetupStandardData(t, db)
	laneID, s0, _ := gateChoreoLane(t, db, "SPL-FENCE", "SPL-FENCE-GATE")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "SPL-FENCE-BIN")

	digger := testdb.CreateOrder(t, db)
	if !d.laneLock.TryLock(laneID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}
	env := testEnvelope()
	o := complexIntoGatedLane(t, d, db, env, "spl-fence",
		sd.StorageNode.Name, s0.Name, sd.Payload.Code)

	// The robot drives to the gate and parks; RDS says WAITING and Core writes
	// `staged`, which is what makes the board offer a RELEASE at all.
	if err := d.lifecycle.MarkInTransit(o, "ROBOT-9", "fleet"); err != nil {
		t.Fatalf("mark in_transit: %v", err)
	}
	if err := d.lifecycle.MarkStaged(o, "fleet"); err != nil {
		t.Fatalf("mark staged: %v", err)
	}
	staged, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !IsGateStaged(staged) {
		t.Fatal("precondition: the order must be gate-staged or the refusal below proves nothing")
	}

	// DIRECTION 1: the station must not advance a lane wait.
	before := len(backend.ReleaseCalls())
	d.HandleOrderRelease(d.syntheticEnvelope(staged.StationID),
		&protocol.OrderRelease{OrderUUID: staged.EdgeUUID})
	if n := len(backend.ReleaseCalls()); n != before {
		t.Fatalf("append calls after a station release = %d, want %d — a station advanced a wait "+
			"whose precondition is a lane it cannot see", n, before)
	}

	// DIRECTION 2: the evaluator can, and does, once the lane is safe.
	d.laneLock.Unlock(laneID, digger.ID)
	d.EvaluateLaneReleases(laneID)
	if n := len(backend.ReleaseCalls()); n != before+1 {
		t.Fatalf("append calls after the dig cleared = %d, want %d — refusing the station must not "+
			"strand the order; the evaluator is the decider", n, before+1)
	}
	sealed, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("reload after release: %v", err)
	}
	if sealed.WaitIndex != 1 {
		t.Errorf("wait_index = %d, want 1 — the evaluator consumed the lane wait", sealed.WaitIndex)
	}
}

// TestSplice_TwoGatedLanesGetOneWaitEach pins rule 2, AND IT REPLACES A TEST THAT
// ASSERTED THE OPPOSITE.
//
// TestSplice_RefusesTwoGatedLanesInOnePlan stood here and pinned the refusal: a
// plan touching two marked lanes was rejected at splice time, which failed the
// order, which terminated the demand. Its stated justification was that "per-wait
// release machinery is the plan-rewriting layer the design declined to build".
//
// That justification did not survive being looked at. Nothing per-wait needed
// building: splitSegment already walks to an arbitrary wait index and reports
// whether more remain, appendSegmentAndAdvance already computes complete =
// !moreWaits, and IsGateStaged already reads the wait the order is parked AT. The
// assert was standing in front of machinery that could already do this — and it
// was standing there terminating demand for a legitimately shaped request, which
// the wait-not-fail law forbids in the one place that was supposed to enforce it.
//
// So the plan gets a wait per lane, each released by its own lane's admission,
// and the FIRST is what the create sends the robot to.
//
// MUTATION (run 2026-08-10): make the splice loop `break` after the first gate.
// Only lane A gets a wait, the robot drives into lane B with no gate at all, and
// the second-wait assertion below fires — which is the failure the gate exists to
// prevent, shipped by the thing that installs it.
func TestSplice_TwoGatedLanesGetOneWaitEach(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	laneA, a0, _ := gateChoreoLane(t, db, "SPL-2A", "SPL-2A-GATE")
	laneB, b0, _ := gateChoreoLane(t, db, "SPL-2B", "SPL-2B-GATE")
	line := lineNode(t, db, "SPL-2-LINE")

	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionDropoff, Node: a0.Name},
		{Action: protocol.ActionPickup, Node: b0.Name},
		{Action: protocol.ActionDropoff, Node: line.Name},
	}
	spliced, target, gated, err := d.spliceLaneWait(plan, 0)
	if err != nil {
		t.Fatalf("a swap picking in one marked lane and dropping in another is an ordinary request "+
			"and must splice: %v", err)
	}
	if !gated {
		t.Fatal("a plan entering two marked lanes is gated")
	}
	if target.lane.ID != laneA {
		t.Errorf("the create's target is lane %d, want the FIRST gate %d — the robot is sent to one "+
			"mark, not to both at once", target.lane.ID, laneA)
	}

	var waits []resolvedStep
	for i, s := range spliced {
		if s.Action != protocol.ActionWait || s.WaitKind != WaitKindLane {
			continue
		}
		waits = append(waits, s)
		if i+1 >= len(spliced) {
			t.Fatalf("wait at %d gates nothing", i)
		}
	}
	if len(waits) != 2 {
		t.Fatalf("spliced plan carries %d lane wait(s), want 2 — one per marked lane it enters. "+
			"Plan: %+v", len(waits), spliced)
	}
	if waits[0].WaitLane != laneA || waits[0].Node != "SPL-2A-GATE" {
		t.Errorf("first wait names lane %d at %q, want %d at SPL-2A-GATE", waits[0].WaitLane, waits[0].Node, laneA)
	}
	if waits[1].WaitLane != laneB || waits[1].Node != "SPL-2B-GATE" {
		t.Errorf("second wait names lane %d at %q, want %d at SPL-2B-GATE", waits[1].WaitLane, waits[1].Node, laneB)
	}
	// Each wait must sit immediately before the step that enters ITS lane. The
	// splice asserts this itself; this is the assertion on the assertion, because
	// a wait in the wrong place reads exactly like a wait in the right one.
	//
	// The plan opens with a pickup at the LINE, so the inserted waits land at 1 and
	// 3 and their entries at 2 and 4 — the point being that neither wait goes to
	// the front of the plan. Everything an order can do before it dwells, it does.
	if spliced[2].Node != a0.Name || spliced[4].Node != b0.Name {
		t.Errorf("waits are not immediately before their entries: %+v", spliced)
	}
	if spliced[0].Node != line.Name {
		t.Errorf("step 0 is %+v, want the line pickup ahead of both gates", spliced[0])
	}
}

// TestSplice_SameGatedLaneTwiceIsFine is the other half of rule 2, and the reason
// it is not spelled "one gated STEP per plan".
//
// A plan that drops into one slot and picks out of another is touching ONE lane,
// which one wait gates correctly. Refusing it would refuse the mid-plan shape the
// candidate re-key was built for.
func TestSplice_SameGatedLaneTwiceIsFine(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	laneID, s0, s1 := gateChoreoLane(t, db, "SPL-SAME", "SPL-SAME-GATE")
	line := lineNode(t, db, "SPL-SAME-LINE")

	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionDropoff, Node: s0.Name},
		{Action: protocol.ActionPickup, Node: s1.Name},
		{Action: protocol.ActionDropoff, Node: line.Name},
	}
	out, target, gated, err := d.spliceLaneWait(plan, 0)
	if err != nil {
		t.Fatalf("two touches of the SAME lane were refused: %v", err)
	}
	if !gated || target.lane.ID != laneID {
		t.Fatalf("gated=%v target=%v, want a splice for lane %d", gated, target.lane, laneID)
	}
	if len(out) != 5 || out[1].Action != protocol.ActionWait || out[2].Node != s0.Name {
		t.Fatalf("spliced plan = %+v, want the wait immediately before the FIRST lane touch", out)
	}
}

// TestSplice_RefusesAnUnresolvedStepBeforeTheEntry pins rule 1.
//
// A deferred dropoff (Node == "") is resolved after intake by
// placeForDedicatedLoader. Until it is, nothing can say which lane it enters — so
// a blank sitting BEFORE the entry we picked means we cannot know our pick is
// really the first, and guessing would splice the wait at the wrong boundary: a
// gate that gates nothing while the robot enters somewhere else.
func TestSplice_RefusesAnUnresolvedStepBeforeTheEntry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	_, s0, _ := gateChoreoLane(t, db, "SPL-BLANK", "SPL-BLANK-GATE")
	line := lineNode(t, db, "SPL-BLANK-LINE")

	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionDropoff}, // deferred: node not resolved yet
		{Action: protocol.ActionPickup, Node: s0.Name},
		{Action: protocol.ActionDropoff, Node: line.Name},
	}
	if _, _, _, err := d.spliceLaneWait(plan, 0); err == nil {
		t.Fatal("a plan with an unresolved step BEFORE its gated entry was spliced. That blank may " +
			"itself be the lane entry once it resolves, in which case the wait is in the wrong place")
	}

	// The same blank AFTER the entry is not this function's problem — the entry is
	// already known to be first.
	ok := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionDropoff, Node: s0.Name},
		{Action: protocol.ActionDropoff}, // deferred, but after the entry
	}
	if _, _, gated, err := d.spliceLaneWait(ok, 0); err != nil || !gated {
		t.Errorf("a blank AFTER the gated entry refused the splice (gated=%v err=%v)", gated, err)
	}
}

// TestSplice_UngatedPlanIsUntouched is the narrowness assertion, and it covers
// every order at both plants: no lane anywhere carries a mark, so the
// walk finds nothing gated and the plan comes back byte-identical.
func TestSplice_UngatedPlanIsUntouched(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, shallow, _ := noneLaneWithTwoSlots(t, db, "SPL-NONE")

	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: sd.LineNode.Name},
		{Action: protocol.ActionDropoff, Node: shallow.Name},
	}
	out, _, gated, err := d.spliceLaneWait(plan, 0)
	if err != nil {
		t.Fatalf("splice on an ungated lane errored: %v", err)
	}
	if gated {
		t.Fatal("a lane on the DEFAULT enforcement mode was treated as gated")
	}
	if len(out) != len(plan) {
		t.Fatalf("plan length changed from %d to %d with nothing gated", len(plan), len(out))
	}
	for i := range plan {
		if out[i] != plan[i] {
			t.Errorf("step %d changed: %+v -> %+v", i, plan[i], out[i])
		}
	}
}
