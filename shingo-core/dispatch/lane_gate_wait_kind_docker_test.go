//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// lane_gate_wait_kind_docker_test.go — the fence, on the plan that could not
// exist before.
//
// The old predicate could not be tested this way at all: `!order.Coordinated`
// made the answer a property of the ORDER, so a coordinated order was never
// gate-staged and there was nothing to fence. The transform removes the
// exclusions that kept that true, and this is the test that could not be
// written until the kind moved onto the step.
//
// ONE ORDER, BOTH WAITS, BOTH DIRECTIONS. A single coordinated plan holding an
// operator wait followed by a lane wait, walked through both positions:
//
//	wait_index 0 → parked at the OPERATOR wait → the station may release,
//	               the evaluator must be blind to it
//	wait_index 1 → parked at the LANE wait     → the station is refused,
//	               the evaluator must see it
//
// The plan is hand-built because the splice does not exist yet. That is the
// point of the sequencing: the predicate is made honest first, so the splice
// lands against a fence that already holds.

func TestWaitKind_FenceHoldsBothWaysOnOneCoordinatedPlan(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	_, laneID, _ := gatedLane(t, db, "WK-BOTH", "WK-BOTH"+"-WAIT")
	slots := laneSlots(t, db, laneID)
	target := slots[1] // the deeper slot; any slot of this lane will do
	line := lineNode(t, db, "WK-BOTH-LINE")

	// [pickup@line, wait(operator, bare), wait@gate(lane), dropoff@slot]
	//
	// The operator wait is BARE, which is the shape Edge emits for a "tooling
	// done" gate — and the shape most likely to break the wait numbering, since
	// it produces no RDS block.
	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: line.Name},
		{Action: protocol.ActionWait},
		{Action: protocol.ActionWait, Node: "WK-BOTH-GATE", WaitKind: WaitKindLane, WaitLane: laneID},
		{Action: protocol.ActionDropoff, Node: target.Name},
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "wk-both"
		o.StationID = "line-1"
		o.Coordinated = true
		o.SourceNode = line.Name
		o.DeliveryNode = target.Name
		o.Status = protocol.StatusStaged
	})
	if err := db.UpdateOrderStepsJSON(order.ID, string(planJSON)); err != nil {
		t.Fatalf("persist plan: %v", err)
	}
	// A vendor order is the robot-committed witness IsGateStaged keeps.
	if err := db.UpdateOrderVendor(order.ID, "WK-BOTH-V1", "CREATED", ""); err != nil {
		t.Fatalf("set vendor order: %v", err)
	}

	// ── Position 1: parked at the OPERATOR wait ───────────────────────────
	atOperator, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if atOperator.WaitIndex != 0 {
		t.Fatalf("fixture: wait_index = %d, want 0", atOperator.WaitIndex)
	}
	if IsGateStaged(atOperator) {
		t.Fatal("a coordinated order parked at its OPERATOR wait read as gate-staged. The predicate " +
			"must name the wait the order is at, not the fact that the plan contains a lane wait " +
			"somewhere — the station owns this one")
	}

	// The evaluator must be blind to it. This is the direction that has no
	// operator-visible symptom: an evaluator that saw this order would append
	// past a wait a human still owes, silently.
	cands, err := d.gateStagedForLane(mustNode(t, db, laneID))
	if err != nil {
		t.Fatalf("gateStagedForLane: %v", err)
	}
	if containsOrder(cands, order.ID) {
		t.Fatal("the lane evaluator listed an order parked at an OPERATOR wait as its candidate. It " +
			"would append past a wait a human owes — and would run rebindGatedDropoff first, " +
			"re-aiming an authored final dropoff at a lane slot")
	}

	// And the station's own release still works, appending only up to the lane
	// wait. This is the owner's requirement made concrete: do all the work you
	// can, stop at the lane.
	before := len(backend.ReleaseCalls())
	env := d.syntheticEnvelope(atOperator.StationID)
	d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: atOperator.EdgeUUID})
	if n := len(backend.ReleaseCalls()); n != before+1 {
		t.Fatalf("append calls after the station released its OWN wait = %d, want %d — the fence is "+
			"scoped to lane waits and must not swallow the station's", n, before+1)
	}

	// ── Position 2: the same order, now parked at the LANE wait ───────────
	atLane, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload after operator release: %v", err)
	}
	if atLane.WaitIndex != 1 {
		t.Fatalf("wait_index = %d, want 1 — the operator's release should have consumed exactly its "+
			"own wait", atLane.WaitIndex)
	}
	if !IsGateStaged(atLane) {
		t.Fatalf("the same order, now parked at its LANE wait, does not read as gate-staged "+
			"(steps=%q wait=%d vendor=%q). Nothing else can release it: only the evaluator knows "+
			"whether the lane is safe", atLane.StepsJSON, atLane.WaitIndex, atLane.VendorOrderID)
	}

	// Now the evaluator sees it.
	cands, err = d.gateStagedForLane(mustNode(t, db, laneID))
	if err != nil {
		t.Fatalf("gateStagedForLane (at lane wait): %v", err)
	}
	if !containsOrder(cands, order.ID) {
		t.Fatal("the lane evaluator cannot see an order parked at ITS wait. Nothing else advances a " +
			"lane wait, so the robot dwells until the abandon sweep")
	}

	// And the station is now refused.
	before = len(backend.ReleaseCalls())
	d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: atLane.EdgeUUID})
	if n := len(backend.ReleaseCalls()); n != before {
		t.Fatalf("append calls after the station released a LANE wait = %d, want %d — a station "+
			"advanced a wait whose precondition is a lane it cannot see, and the robot enters a lane "+
			"Core has not cleared", n, before)
	}
	final, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload after refused release: %v", err)
	}
	if final.WaitIndex != 1 {
		t.Errorf("wait_index = %d, want 1 — a refused release must leave the wait unconsumed for the "+
			"evaluator", final.WaitIndex)
	}
}

// TestWaitKind_ValveAuthoredOrdersAreUnmoved is the regression half: the
// population that existed before this change reads exactly as it did.
//
// Both valve shapes, through the real dispatch path, asserted gate-staged at
// wait_index 0 and NOT gate-staged once sealed — which is what the deleted
// `WaitIndex == 0` and `!Coordinated` terms used to deliver between them.
func TestWaitKind_ValveAuthoredOrdersAreUnmoved(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, _, s1 := gateRetrieveLane(t, db, "WK-VALVE", "WK-VALVE-WAIT")
	line := lineNode(t, db, "WK-VALVE-LINE")

	// A dig holds the lane so the retrieve dwells rather than sealing at once.
	digger := testdb.CreateOrder(t, db)
	if !d.laneLock.TryLock(laneID, digger.ID) {
		t.Fatal("TryLock on a free lane must succeed")
	}

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeRetrieve
		o.SourceNode = s1.Name
		o.DeliveryNode = line.Name
		o.Status = "sourcing"
	})
	if _, err := d.DispatchDirect(order, s1, line); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	dwelling, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !IsGateStaged(dwelling) {
		t.Fatalf("a valve-authored retrieve dwelling at its gate is not gate-staged "+
			"(steps=%q wait=%d vendor=%q) — the valve must stamp the wait it mints",
			dwelling.StepsJSON, dwelling.WaitIndex, dwelling.VendorOrderID)
	}

	// The stamp is on the persisted plan, not merely on the in-memory one.
	var persisted []resolvedStep
	if err := json.Unmarshal([]byte(dwelling.StepsJSON), &persisted); err != nil {
		t.Fatalf("parse persisted plan: %v", err)
	}
	w, ok := waitAt(persisted, dwelling.WaitIndex)
	if !ok || w.WaitKind != WaitKindLane || w.WaitLane != laneID {
		t.Errorf("persisted wait = %+v (ok=%v), want WaitKind %q and lane %d — steps_json is what "+
			"survives a restart, so the stamp has to be in it", w, ok, WaitKindLane, laneID)
	}

	// Sealed: the evaluator appends the tail, wait_index moves past the only
	// wait, and the order stops being gate-staged.
	d.laneLock.Unlock(laneID, digger.ID)
	d.EvaluateLaneReleases(laneID)

	sealed, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload after release: %v", err)
	}
	if sealed.WaitIndex != 1 {
		t.Fatalf("wait_index = %d, want 1 — the evaluator should have appended the tail", sealed.WaitIndex)
	}
	if IsGateStaged(sealed) {
		t.Error("a sealed order still reads as gate-staged. wait_index has moved past the plan's only " +
			"wait, so there is no wait to be parked at — the abandon sweep would exempt it forever")
	}
}

// TestWaitKind_UnstampedLaneWaitIsNotSilentlyRescued pins the DELETED
// compatibility fallback.
//
// The specified design carried a fallback arm: no WaitKind, plus !Coordinated,
// wait_index 0 and a plan, would be read as a lane wait using the old
// derivation. It is deliberately absent, and this is the assertion that it stays
// absent — an unstamped wait on a plain order reads as an OPERATOR wait.
//
// WHY THAT IS THE BEHAVIOUR WORTH PINNING. The fallback cannot fire on any row
// that can exist (the two valve functions are the only writers of steps_json for
// a non-coordinated order, and both stamp), but the splice becomes a THIRD
// producer of lane waits — in the code most likely to forget. A fallback would
// reconstruct the right answer from the wrong evidence and make a missing stamp
// look like it worked, on the one path where the old derivation happens to
// agree. The bug would surface later, elsewhere, as a fence that does not hold.
func TestWaitKind_UnstampedLaneWaitIsNotSilentlyRescued(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_ = d

	// Exactly the shape the fallback described: plain, one wait, index 0, a
	// vendor order — but the wait is unstamped.
	plan := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SRC"},
		{Action: protocol.ActionWait, Node: "GATE"},
		{Action: protocol.ActionDropoff, Node: "DEST"},
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "wk-unstamped"
		o.StationID = "line-1"
	})
	if err := db.UpdateOrderStepsJSON(order.ID, string(planJSON)); err != nil {
		t.Fatalf("persist plan: %v", err)
	}
	if err := db.UpdateOrderVendor(order.ID, "WK-UNSTAMPED-V1", "CREATED", ""); err != nil {
		t.Fatalf("set vendor: %v", err)
	}
	fresh, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if IsGateStaged(fresh) {
		t.Fatal("an UNSTAMPED wait was read as a lane wait. That is the deleted fallback arm coming " +
			"back: it reconstructs the old answer from order-shaped evidence, so a producer that " +
			"forgets to stamp looks correct on the plain path and breaks the fence everywhere else")
	}
}

// mustNode resolves a node by id or fails the test.
func mustNode(t *testing.T, db *store.DB, id int64) *nodes.Node {
	t.Helper()
	n, err := db.GetNode(id)
	if err != nil || n == nil {
		t.Fatalf("get node %d: %v", id, err)
	}
	return n
}

func containsOrder(cands []gateCandidate, orderID int64) bool {
	for _, c := range cands {
		if c.order.ID == orderID {
			return true
		}
	}
	return false
}
