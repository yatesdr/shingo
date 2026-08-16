//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/fleet/seerrds"
	"shingocore/internal/testdb"
	"shingocore/rds"
	"shingocore/store/orders"
)

// lane_gate_fence_docker_test.go — one decider for every append.
//
// A gate wait's precondition is INTERNAL to Core (lane claim, robot inside, slot
// reachable) and only the evaluator can know when it is satisfied. A station wait's
// precondition is PHYSICAL at a station (a bin loaded, a manifest known) and only
// the station can know. HandleOrderRelease was built for the second and predates
// lane gating entirely; nobody scoped it when the gate arrived, so it would append
// either.

// TestGateWait_StationReleaseIsRefused drives the whole invitation and then the
// click, because the invitation is the part that makes this reachable at all.
//
// Core does not merely fail to mark the wait — it ADVERTISES a gate wait as
// release-ready. The robot parks on its Wait block, RDS reports WAITING, MapState
// turns that into StatusStaged, Core writes it and pushes TypeOrderStaged to the
// station, the board renders RELEASE, and the click comes back as an OrderRelease
// whose `staged` precondition passes BECAUSE CORE ITSELF JUST WROTE IT. Every link
// below is the production one; MapState is called rather than assumed so the
// WAITING → staged step is pinned here rather than trusted.
//
// Vacuity (DESIGN §16 rule 7): an assertion that a station cannot pop this wait
// also passes if the order never reached `staged` at all — the release would bounce
// on the status precondition and prove nothing about the fence. So the staged
// transition is asserted as a PRECONDITION before the release is sent, and it goes
// through lifecycle.MarkStaged, which is what handleVendorStatusChange calls.
//
// MUTATION (verified): delete the IsGateStaged refusal in HandleOrderRelease. The
// release then appends the [pickup, dropoff] tail and this test's own "append
// calls = 0" assertion fires. The status precondition does not shield it — the
// order is genuinely staged, which is the whole point of driving the invitation.
func TestGateWait_StationReleaseIsRefused(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, _, s1 := gateRetrieveLane(t, db, "GCFENCE", "GCFENCE-WAIT")
	line := lineNode(t, db, "GCFENCE-LINE")

	// A dig holds the lane, so the retrieve pre-positions and dwells on its wait.
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
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("append calls after dispatch = %d, want 0 — a dig-held retrieve holds its tail", n)
	}

	// ── The invitation ────────────────────────────────────────────────────
	// The robot reaches the wait point. RDS calls that WAITING.
	if mapped := seerrds.MapState(string(rds.StateWaiting)); protocol.Status(mapped) != protocol.StatusStaged {
		t.Fatalf("MapState(WAITING) = %q, want %q — the invitation chain starts here and this test "+
			"depends on it", mapped, protocol.StatusStaged)
	}
	parked, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload before staging: %v", err)
	}
	// BOTH transitions, because `staged` is not reachable from `dispatched`
	// (protocol/types.go: dispatched → in_transit → staged). The robot DRIVES to
	// the wait point — RDS RUNNING, in_transit — before it parks on the Wait block
	// and RDS turns WAITING. Skipping the drive leaves the order in `dispatched`,
	// where HandleOrderRelease bounces on its own status check and this test would
	// pass without the fence existing.
	if err := d.lifecycle.MarkInTransit(parked, "ROBOT-1", "fleet"); err != nil {
		t.Fatalf("mark in_transit (the robot driving to the wait point): %v", err)
	}
	if err := d.lifecycle.MarkStaged(parked, "fleet"); err != nil {
		t.Fatalf("mark staged (the transition handleVendorStatusChange makes): %v", err)
	}

	// PRECONDITION, not an assertion about the fence: the release must arrive at an
	// order Core genuinely advertised, or the refusal below proves nothing.
	advertised, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload after staging: %v", err)
	}
	if advertised.Status != protocol.StatusStaged {
		t.Fatalf("order status = %s, want staged — without it HandleOrderRelease bounces on its own "+
			"status check and this test proves nothing about the gate", advertised.Status)
	}
	if !IsGateStaged(advertised) {
		t.Fatalf("order must read as gate-staged (steps=%q wait=%d vendor=%q)",
			advertised.StepsJSON, advertised.WaitIndex, advertised.VendorOrderID)
	}

	// ── The click ─────────────────────────────────────────────────────────
	// A real station envelope: RoleEdge with the owning station, so checkOwnership
	// does its genuine comparison rather than being waved through as RoleCore.
	env := d.syntheticEnvelope(advertised.StationID)
	d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: advertised.EdgeUUID})

	// THE ASSERTION. Nobody but the evaluator advances a gate wait.
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("append calls after the station release = %d, want 0 — a station popped a wait whose "+
			"precondition is a lane it cannot see. The robot enters a lane Core has not cleared", n)
	}
	after, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload after release: %v", err)
	}
	if after.WaitIndex != 0 {
		t.Errorf("wait_index = %d, want 0 — the refusal must leave the wait unconsumed so the "+
			"evaluator can still advance it", after.WaitIndex)
	}
	if !IsGateStaged(after) {
		t.Error("the order must still read as gate-staged after a refused release")
	}

	// And the evaluator still can: the refusal fences the station, not the lane.
	d.laneLock.Unlock(laneID, digger.ID)
	d.EvaluateLaneReleases(laneID)
	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("append calls after the dig cleared = %d, want 1 — refusing the station must not "+
			"strand the order; the evaluator is the decider, not a second refuser", n)
	}
}

// TestStationWait_ReleaseStillWorks is the other half, and it is what stops the
// fence from being "HandleOrderRelease never appends".
//
// A coordinated order's waits are STATION waits — its plan is Edge-authored and its
// preconditions are physical at the station. IsGateStaged returns false for it by
// its first clause (`!order.Coordinated`), so the fence must not touch it.
//
// MUTATION (verified): widen the fence from IsGateStaged(order) to
// `order.StepsJSON != ""`. This test's "append calls = 1" assertion fires, because
// every coordinated order carries steps — which is exactly the over-broad predicate
// worth guarding against, since steps-presence has been mistaken for provenance in
// this codebase before (the v46 coordinated backfill).
func TestStationWait_ReleaseStillWorks(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "FENCE-STATION-BIN")

	env := testEnvelope()
	o := submitComplexAndDispatch(t, d, db, env, &protocol.ComplexOrderRequest{
		OrderUUID:   "fence-station-wait",
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: storageNode.Name},
			{Action: "wait"},
			{Action: "dropoff", Node: lineNode.Name},
		},
	})
	if o.VendorOrderID == "" {
		t.Fatalf("complex order was not dispatched (status %s)", o.Status)
	}
	if IsGateStaged(o) {
		t.Fatal("a coordinated order must never read as gate-staged — its waits belong to the station")
	}
	// Same two transitions as the gate case: the robot drives to the line, then
	// dwells there awaiting the operator. `staged` is HandleOrderRelease's
	// precondition and it is not reachable from `dispatched`.
	if err := d.lifecycle.MarkInTransit(o, "ROBOT-2", "fleet"); err != nil {
		t.Fatalf("mark in_transit: %v", err)
	}
	if err := d.lifecycle.MarkStaged(o, "fleet"); err != nil {
		t.Fatalf("mark staged: %v", err)
	}
	before := len(backend.ReleaseCalls())

	d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: o.EdgeUUID})

	if n := len(backend.ReleaseCalls()); n != before+1 {
		t.Fatalf("append calls = %d, want %d — the station's own wait must still be releasable by the "+
			"station. The fence is scoped to gate waits, not to this handler", n, before+1)
	}
}

// TestGateTail_StaleStructDoesNotDoubleAppend pins the durable half of the
// valve/evaluator guard.
//
// The valve and the evaluator are the same decision at two moments, and they now
// serialize on one per-lane key. The mutex alone does not close the window:
// whichever of the two waits on it still has to notice what the other did WHILE it
// waited, and only a reload can tell it. The evaluator's callers reload; the valve
// held the struct it had just built and never looked again.
//
// This drives that state directly rather than racing for it. An in-memory order
// says wait_index 0 — exactly what the valve carries — while the durable row says
// 1, which is what an evaluator pass that won the race would have left behind.
// Appending on the strength of the stale struct is a duplicate tail, and duplicate
// block ids are the one thing SEER rejects outright.
//
// WHAT THIS DOES NOT PROVE, stated plainly: that the interleaving is reachable in
// production. A deterministic red for that would have to re-enter the evaluator
// from inside the valve's own append, which the per-lane mutex — not reentrant —
// turns into a deadlock rather than a double append, so the test would hang rather
// than fail. The window was closed on the structural argument (both writers, one
// key, one reload) and this pins the reload half of it.
//
// MUTATION (verified): delete the reload guard at the top of appendGateTail. The
// append then goes out on the stale struct and this test's own append-count
// assertion fires.
func TestGateTail_StaleStructDoesNotDoubleAppend(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	_, _, s1 := gateRetrieveLane(t, db, "GCSTALE", "GCSTALE-WAIT")
	line := lineNode(t, db, "GCSTALE-LINE")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = OrderTypeRetrieve
		o.SourceNode = s1.Name
		o.DeliveryNode = line.Name
		o.Status = "sourcing"
	})
	if _, err := d.DispatchDirect(order, s1, line); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}

	// The lane was clear, so the valve already appended and sealed. That is the
	// durable state an evaluator pass would have left behind had it won the race.
	sealed, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if sealed.WaitIndex != 1 {
		t.Fatalf("fixture: wait_index = %d, want 1 — nothing has appended, so there is no second "+
			"append for this test to refuse", sealed.WaitIndex)
	}
	before := len(backend.ReleaseCalls())

	// The other writer's view: the struct it built before the row moved.
	stale := *sealed
	stale.WaitIndex = 0
	if !IsGateStaged(&stale) {
		t.Fatal("fixture: the stale struct must read as still awaiting a tail, or the guard under " +
			"test is not the thing being exercised")
	}

	if err := d.appendGateTail(&stale, "stale-struct probe"); err != nil {
		t.Fatalf("appendGateTail on a stale struct returned an error: %v — it should decline "+
			"quietly, not fail the caller", err)
	}

	if n := len(backend.ReleaseCalls()); n != before {
		t.Fatalf("append calls = %d, want 0 — the tail went out twice for one order. The second "+
			"carries block ids the first already used, which SEER rejects outright", n-before)
	}
	final, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("reload after probe: %v", err)
	}
	if final.WaitIndex != 1 {
		t.Errorf("wait_index = %d, want 1 — a declined append must not advance the row", final.WaitIndex)
	}
}
