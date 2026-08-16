//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// ── MULTI-GATE PLANS, END TO END ──────────────────────────────────────────
//
// The simplest plan that enters two marked lanes is the plainest order there is:
// pick out of lane A, drop into lane B. No complex authoring, no swap machinery —
// buildTransportPlan produces [pickup@A, dropoff@B] and the splice does the rest.
// That it is this ordinary is the point: the shape the old rule terminated was
// never exotic, it was one plant marking a second lane away.
//
// The spliced plan is
//
//	[wait@A-GATE, pickup@A_slot, wait@B-GATE, dropoff@B_slot]
//
// so the robot is created holding ONE block — a wait at A's mark. It is let into
// A, picks, drives to B's mark, and dwells THERE until B is safe. Two entries,
// two gates, each answered by its own lane's admission on its own lane's events.

// waitLanesOf reads the lane waits off an order's persisted plan, in order.
func waitLanesOf(t *testing.T, db *store.DB, orderID int64) []int64 {
	t.Helper()
	o, err := db.GetOrder(orderID)
	testutil.MustNoErr(t, err, "reload order")
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &steps), "parse steps_json")
	var out []int64
	for _, s := range steps {
		if s.WaitKind == WaitKindLane {
			out = append(out, s.WaitLane)
		}
	}
	return out
}

// TestMultiGate_EachLaneIsEnteredThroughItsOwnGate is Part B's proof.
//
// It asserts the three things that make this a gate rather than a splice that
// compiles: the robot is created UNSEALED at the first mark, its release into the
// first lane leaves it UNSEALED again at the second mark, and the second lane's
// own congestion — not the first's — is what holds it there.
//
// THE TRAFFIC ARM IS THE LOAD-BEARING ONE. A second wait that always releases
// immediately would pass a shape test and gate nothing. So lane B is occupied by
// another robot when the order arrives at B's mark, and the assertion is that it
// stays put until that robot is out. That is the whole claim of "released
// independently by that lane's own admission".
//
// MUTATION (run 2026-08-10): make the splice loop `break` after the first gate.
// The order is sealed on its FIRST release — one wait, complete=true — and drives
// straight into an occupied lane B. The "still dwelling" assertion fires.
func TestMultiGate_EachLaneIsEnteredThroughItsOwnGate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)
	testdb.SetupStandardData(t, db)

	laneA, a0, _ := gateChoreoLane(t, db, "MG-A", "MG-A-GATE")
	laneB, b0, _ := gateChoreoLane(t, db, "MG-B", "MG-B-GATE")

	// Something to pick out of lane A.
	bp := &payloads.Payload{Code: "MGP"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")
	createTestBinAtNode(t, db, bp.Code, a0.ID, "BIN-MG")

	// LANE B IS BUSY. Another order is inside it, holding Hold B.
	inB := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = b0.Name
		o.Status = "in_transit"
	})
	bNode, err := db.GetNode(b0.ID)
	testutil.MustNoErr(t, err, "reload b0")
	testutil.MustNoErr(t, d.TakeLaneOccupancy(inB.ID, bNode), "put a robot inside lane B")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = a0.Name
		o.DeliveryNode = b0.Name
		o.Status = "sourcing"
		o.PayloadCode = bp.Code
	})
	aNode, err := db.GetNode(a0.ID)
	testutil.MustNoErr(t, err, "reload a0")
	if _, err := d.DispatchDirect(order, aNode, bNode); err != nil {
		t.Fatalf("a pick-in-one-marked-lane, drop-in-another order must dispatch: %v", err)
	}

	// 1. TWO WAITS ON THE PERSISTED PLAN, in lane order.
	lanes := waitLanesOf(t, db, order.ID)
	if len(lanes) != 2 || lanes[0] != laneA || lanes[1] != laneB {
		t.Fatalf("persisted waits name lanes %v, want [%d %d] — the plan is what the evaluator "+
			"re-derives from, so a missing second wait is a lane entered with no gate",
			lanes, laneA, laneB)
	}

	// 2. LANE A WAS OPEN, so the first segment went out immediately — and it did
	// NOT seal the order, because there is another wait behind it.
	staged, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload after dispatch")
	if staged.WaitIndex != 1 {
		t.Fatalf("wait_index = %d after the first release, want 1", staged.WaitIndex)
	}
	first := backend.ReleaseCalls()
	if len(first) != 1 {
		t.Fatalf("append calls = %d after dispatch, want 1 (lane A was open)", len(first))
	}
	if first[0].Complete {
		t.Error("the FIRST append must not seal the order — the robot still has a gate to pass. " +
			"Sealing here hands the fleet a waybill that drives into lane B unguarded")
	}
	if !IsGateStaged(staged) {
		t.Fatal("after clearing lane A the order must be gate-staged at lane B's mark")
	}

	// 3. IT IS LANE B THAT HOLDS IT NOW. The order arrives at B's mark; B is
	// occupied; it dwells.
	markStaged(t, db, order.ID)
	d.EvaluateLaneReleases(laneB)
	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("append calls = %d while lane B is occupied, want 1 — the second gate is not gating",
			n)
	}
	held, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload while held at B")
	if !IsGateStaged(held) {
		t.Fatal("the order must still be dwelling at lane B's mark while another robot is inside B")
	}
	if held.QueueCause == "" {
		t.Error("a robot dwelling at a mark must say why (dcb2c014) — even at the second gate")
	}

	// A firing for lane A must not release it either: the order is parked at B's
	// wait, and lane A's evaluator has no business in it.
	d.EvaluateLaneReleases(laneA)
	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("lane A's evaluator released an order parked at lane B's wait (appends now %d)", n)
	}

	// 4. LANE B CLEARS. Now, and only now, the order is sealed into it.
	d.ReleaseLaneOccupancy(inB.ID)
	d.EvaluateLaneReleases(laneB)

	appends := backend.ReleaseCalls()
	if len(appends) != 2 {
		t.Fatalf("append calls = %d after lane B cleared, want 2", len(appends))
	}
	if !appends[1].Complete {
		t.Error("the LAST append must seal the order — there is no gate left to pass")
	}
	// The sealing block drops in LANE B — but not necessarily in the slot the order
	// was born naming. rebindGatedDropoff re-resolves against the lane as it stands
	// at the moment of append, and back-to-front packing means a deeper free slot
	// wins. Asserting b0 exactly would be asserting that the re-bind did NOT
	// happen, which is a different (and wrong) claim than "it entered lane B".
	if len(appends[1].Blocks) != 1 {
		t.Fatalf("the sealing append carries %d blocks, want 1", len(appends[1].Blocks))
	}
	sealedAt, err := db.GetNodeByDotName(appends[1].Blocks[0].Location)
	testutil.MustNoErr(t, err, "resolve the sealing block's node")
	if sealedAt == nil || sealedAt.ParentID == nil || *sealedAt.ParentID != laneB {
		t.Errorf("the sealing append drops at %q, which is not in lane B",
			appends[1].Blocks[0].Location)
	}
	done, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload after the second release")
	if done.WaitIndex != 2 {
		t.Errorf("wait_index = %d, want 2 — both waits consumed", done.WaitIndex)
	}
	if IsGateStaged(done) {
		t.Error("a fully released order must no longer read as gate-staged")
	}
}

// TestMultiGate_SecondGateGetsWindow3sRescue is the interaction the batch asked
// about explicitly: a multi-gate plan staged at its SECOND gate, behind a bin no
// one is coming for, must be healed like anybody else.
//
// There is no reason it would not be — the heal reads the candidate set, and the
// candidate set is derived from the wait an order is parked AT rather than from
// its endpoints or from how it got there. But "there is no reason it would not
// be" is what a test is for, and the two features landed in the same batch, so
// the composition is the thing neither of them proves alone.
//
// MUTATION (run 2026-08-10): comment out the propose() calls, as in the window 3
// suite. No dig is created and the order dwells at its second gate indefinitely —
// the F-11 shape, reached through a door F-11 never used.
func TestMultiGate_SecondGateGetsWindow3sRescue(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)
	testdb.SetupStandardData(t, db)

	// Lane B here is the WALL lane from the window 3 fixture: marked, three deep,
	// and in a group with an ungated lane to park a blocker in.
	wall, park, w, _, bp := healLaneFixture(t, db, "MG2")
	laneA, a0, _ := gateChoreoLane(t, db, "MG2-A", "MG2-A-GATE")

	createTestBinAtNode(t, db, bp.Code, a0.ID, "BIN-MG2")

	// A deeper store in the wall lane, holding its mouth — the Tier-2 reason our
	// order will still be at B's mark when the wall appears.
	line := lineNode(t, db, "MG2-LINE")
	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = w[2].Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(deep, line, w[2], EntryFreshBin); err != nil || !adm {
		t.Fatalf("the deeper store must take its mouth row: adm=%v err=%v", adm, err)
	}
	testutil.MustNoErr(t, db.UpdateOrderVendor(deep.ID, "mg2-deep", "RUNNING", ""), "deep vendor")

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = a0.Name
		o.DeliveryNode = w[1].Name
		o.Status = "sourcing"
		o.PayloadCode = bp.Code
	})
	aNode, err := db.GetNode(a0.ID)
	testutil.MustNoErr(t, err, "reload a0")
	dest, err := db.GetNode(w[1].ID)
	testutil.MustNoErr(t, err, "reload wall slot")
	if _, err := d.DispatchDirect(order, aNode, dest); err != nil {
		t.Fatalf("dispatch across two marked lanes: %v", err)
	}

	lanes := waitLanesOf(t, db, order.ID)
	if len(lanes) != 2 || lanes[0] != laneA || lanes[1] != wall.ID {
		t.Fatalf("waits name lanes %v, want [%d %d]", lanes, laneA, wall.ID)
	}
	atSecond, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload")
	if atSecond.WaitIndex != 1 || !IsGateStaged(atSecond) {
		t.Fatalf("the order must be dwelling at the SECOND gate (wait_index=%d staged=%v)",
			atSecond.WaitIndex, IsGateStaged(atSecond))
	}
	markStaged(t, db, order.ID)

	// The wall arrives at the second lane's mouth, unclaimed, wanted by nobody.
	blocker := createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-MG2-WALL")
	d.ReleaseInboundLaneForOrder(deep.ID, w[2].Name)

	d.EvaluateLaneReleases(wall.ID)

	parent := digParentFor(t, db, w[0].Name)
	if parent == nil {
		t.Fatalf("no heal dig for %s. The order is dwelling at its SECOND gate behind unclaimed bin "+
			"%d — the same F-11 state, reached by a plan that passed one gate already. Nothing about "+
			"how it got to the mark should change whether the lane can heal itself.",
			wall.Name, blocker.ID)
	}
	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list dig legs")
	if len(children) != 1 {
		t.Fatalf("dig has %d legs, want 1", len(children))
	}
	// The heal dig's leg dwells in the walled lane until Core picks it a slot, so
	// the destination is read after the release rather than off the plan.
	digLeg := releaseDwell(t, d, db, children[0])
	destNode, err := db.GetNodeByDotName(digLeg.DeliveryNode)
	testutil.MustNoErr(t, err, "resolve the leg's dropoff")
	if destNode == nil || destNode.ParentID == nil || *destNode.ParentID != park.ID {
		t.Errorf("the blocker was parked at %q, want a slot in the ungated %s",
			digLeg.DeliveryNode, park.Name)
	}

	// And it completes the way it does for a single-gate dweller.
	robotRunsDigLeg(t, db, digLeg, destNode.ID)
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "advance the finished dig")

	freed, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload after the dig")
	if IsGateStaged(freed) {
		t.Errorf("the wall is gone and the order is still at its second mark (wait_index=%d)",
			freed.WaitIndex)
	}
}

// TestGatedCreate_DeclaresTheLaneItWorksBeforeTheGate is the rig's find, pinned.
//
// A dig leg picks a blocker out of one lane and parks it in another. When only
// the PARK lane is marked, the splice puts the wait immediately before the
// dropoff — so the CREATE carries the pickup, and the robot drives into the
// unmarked source lane as its first act. The gated arm declared no entering nodes
// at all, on the reasoning that "the create ends at the wait point outside the
// corridor". True of the gated lane; false of every lane the plan works on the
// way to it.
//
// The result was a robot inside a corridor holding no occupancy row — F-12's
// shape, reached through the gated door. The seam assertion caught it on the
// lane-stress rig within minutes of the rebuilt plant coming up, and caught it by
// REFUSING the dispatch: leg failed, parent failed, the two-robot swap behind it
// cancelled its evac, and the line starved. Right answer, expensive lesson.
//
// MUTATION (run 2026-08-10): drop planNodes(preWait) from the gated commit. The
// occupancy assertion fires directly, and with the seam guard in place the
// dispatch is refused outright — which is how the rig reported it.
func TestGatedCreate_DeclaresTheLaneItWorksBeforeTheGate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)
	testdb.SetupStandardData(t, db)

	// The PARK lane is marked; the PICKUP lane is not. That asymmetry is the whole
	// fixture — it is what puts a lane-entering step ahead of the gate.
	parkLane, park0, _ := gateChoreoLane(t, db, "PWG-PARK", "PWG-PARK-GATE")
	pickLane, pick0, _ := gateChoreoLane(t, db, "PWG-PICK", "") // no mark

	bp := &payloads.Payload{Code: "PWGP"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")
	createTestBinAtNode(t, db, bp.Code, pick0.ID, "BIN-PWG")

	// The park lane is BUSY, so the robot actually dwells at its mark instead of
	// being released back-to-back with the create. That dwell is the dangerous
	// window and the reason this test exists: the robot has already driven into the
	// unmarked pickup lane and is now standing still, holding a bin, for as long as
	// the park lane stays contended.
	inPark := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = park0.Name
		o.Status = "in_transit"
	})
	parkNode, err := db.GetNode(park0.ID)
	testutil.MustNoErr(t, err, "reload park slot")
	testutil.MustNoErr(t, d.TakeLaneOccupancy(inPark.ID, parkNode), "put a robot inside the park lane")
	_ = parkLane

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = pick0.Name
		o.DeliveryNode = park0.Name
		o.Status = "sourcing"
		o.PayloadCode = bp.Code
	})
	src, err2 := db.GetNode(pick0.ID)
	err = err2
	testutil.MustNoErr(t, err, "reload pickup slot")
	if _, err := d.DispatchDirect(order, src, parkNode); err != nil {
		t.Fatalf("a dig-shaped leg into a marked park lane must dispatch: %v", err)
	}

	staged, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload")
	if !IsGateStaged(staged) {
		t.Fatalf("the order should be dwelling at the park lane's mark (wait_index=%d)", staged.WaitIndex)
	}

	// THE CLAIM: the robot is already inside the unmarked pickup lane, and the
	// books say so.
	occupants, err := occupantsOfLane(db, pickLane)
	testutil.MustNoErr(t, err, "read occupants")
	if !containsID(occupants, order.ID) {
		t.Fatalf("order %d was sent to pick at %s — inside lane %d — while it dwells at the park "+
			"lane's mark, and holds NO occupancy row there (occupants: %v). Admission would report "+
			"that corridor empty to the next entrant and admit it lawfully into an occupied lane.",
			order.ID, pick0.Name, pickLane, occupants)
	}
}

// occupantsOfLane and containsID are the two reads the pre-wait test needs, kept
// here rather than borrowed: it is asserting on the reservation rows directly,
// which is the fact under every admission answer.
func occupantsOfLane(db *store.DB, laneID int64) ([]int64, error) {
	return reservations.OccupantsOf(db.DB, laneID)
}
