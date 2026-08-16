//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// service_dig_end_to_end_docker_test.go — THE A BATCH'S PROOF.
//
// Everything else in this batch pins a piece: the demand keeps its status, the
// legs hang off the dig, the causes are right, the bridge is gone. None of them
// says the thing the ruling is actually about, which is that the whole cycle
// completes WITHOUT A HUMAN and WITHOUT THE DEMAND EVER LEAVING THE ACQUIRING
// SET.
//
// The shape the batch replaced, for contrast: the demand became the compound's
// parent, went to `reshuffling`, and had to be brought back by ResumeCompound to
// re-run its own first pickup — a status excursion on an order nothing was wrong
// with, plus a resumption path that existed only for this case, plus a lane lock
// that had to outlive the compound so the parent could still get in when it
// returned. That last one was the expose bridge, and every bridge-dependent
// releaser hung off it.

// TestServiceDig_BuriedComplexDemand_DigsThenDispatchesItsOwnPlan is the
// end-to-end.
//
// MUTATION (verified): make proposeLaneClearDig return early with
// serviceDigLaneBusy — i.e. suppress the proposal. The demand parks with
// CauseIntakeBuried and never moves; the "a dig was raised" assertion fires, and
// the parked row is exactly what the stall checker would report, which is the
// honest failure mode rather than a wedge.
func TestServiceDig_BuriedComplexDemand_DigsThenDispatchesItsOwnPlan(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-E2E-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-E2E-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	demand := mkQueuedComplexParent(t, db, "uuid-e2e-service-dig", bp.Code)
	// AN ORIGIN, because production always stamps one and the corridor handoff
	// needs it: a service dig inherits the origin of the demand that could not
	// move, and that episode is the only tie between the two rows. An order
	// created without one is its own defect (origin_class 'orphan'), and what it
	// costs here is a corridor released a scan early rather than handed over.
	_, err := db.Exec(`UPDATE orders SET origin_id = $1, origin_class = 'demand' WHERE id = $2`,
		"55555555-5555-5555-5555-555555555555", demand.ID)
	testutil.MustNoErr(t, err, "stamp the demand's origin")
	demand, err = db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the demand with its origin")

	// ── THE BURIAL ────────────────────────────────────────────────────────
	d.handleComplexBuriedOnReplay(demand, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	// (1) THE DEMAND DID NOT MOVE. This is the assertion the whole batch is for.
	parked, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the demand after the burial")
	if parked.Status != StatusQueued {
		t.Fatalf("demand status = %q, want %q — it was re-parented into its own dig again, which is "+
			"the status excursion the two-shape ruling deletes", parked.Status, StatusQueued)
	}
	if parked.QueueCause != string(CauseIntakeBuried) {
		t.Errorf("queue_cause = %q, want %q — the operator has to be able to read why it is waiting",
			parked.QueueCause, CauseIntakeBuried)
	}
	if strings.TrimSpace(parked.QueueReason) == "" {
		t.Error("queue_reason is blank — this is the sentence on the board while the dig runs")
	}
	if kids, _ := db.ListChildOrders(demand.ID); len(kids) != 0 {
		t.Fatalf("the demand owns %d legs — it became the dig instead of being served by one", len(kids))
	}

	// (2) A SERVICE DIG EXISTS, and it belongs to the demand's episode — which is
	// the only tie between them now that there is no requester pointer (§R.40).
	dig := serviceDigFor(t, db, demand)
	if dig.OriginID != demand.OriginID || dig.OriginClass != demand.OriginClass {
		t.Errorf("dig origin = (%q, %q), want the demand's (%q, %q) — the cost of digging belongs to "+
			"the episode that caused it", dig.OriginID, dig.OriginClass, demand.OriginID, demand.OriginClass)
	}
	legs, err := db.ListChildOrders(dig.ID)
	testutil.MustNoErr(t, err, "list the dig's legs")
	if len(legs) != 1 {
		t.Fatalf("the dig has %d leg(s), want 1 (the single blocker) — and it must carry no retrieve: "+
			"the demand owns its own pickup", len(legs))
	}
	if legs[0].BinID == nil || *legs[0].BinID != blocker.ID {
		t.Errorf("the dig's leg moves %v, want the blocker %d", legs[0].BinID, blocker.ID)
	}
	if legs[0].VendorOrderID == "" {
		t.Errorf("the dig's leg was never dispatched (queue_cause %q) — the group was quiet",
			legs[0].QueueCause)
	}

	// (3) THE EXCAVATION COMPLETES. The blocker lands in its shuffle slot and the
	// dig goes terminal, which is the ordinary compound path — nothing about this
	// batch changes how legs pipeline.
	driveDigToCompletion(t, db, d, dig)

	// (4) THE TARGET IS REACHABLE, AND THE CORRIDOR HAS CHANGED HANDS.
	//
	// This assertion has been through three shapes and the differences are the
	// argument, so all three are here:
	//
	//	THE EXPOSE BRIDGE transferred the lock to the COMPLEX PARENT, parked the
	//	fact in pending_lane_extensions, and released it when a re-parented demand
	//	came back through ResumeCompound. A side table, and a resumption path that
	//	existed only for this case.
	//
	//	THE DIG-SIDE HOLD kept the lock on the dig until the bin left, keyed on a
	//	physical fact so no order's death could strand it. It closed the exposure
	//	and opened a worse hole: the dig never terminated, and when the episode
	//	ended without collecting the bin, the corridor was shut with no releaser in
	//	the world. Five at once on the lane-stress rig.
	//
	//	THE HANDOFF, which is this. The lane goes to the live demand in the dig's
	//	episode as that demand's OWN outbound hold, and the dig finishes. Outbound
	//	excludes exactly what has to be excluded — a drop into the lane, the only
	//	way the uncovered bin can be re-buried — and the hold now sits inside an
	//	ordinary order's lifetime, so it cannot outlive the work it was taken for.
	acc, err := db.IsSlotAccessible(slots[1].ID)
	testutil.MustNoErr(t, err, "ask whether the target is reachable now")
	if !acc {
		t.Fatal("the target slot is still walled after the dig completed — the excavation did not " +
			"actually excavate")
	}
	if d.laneLock.IsLocked(lane.ID) {
		t.Fatalf("lane %s is still DIG-locked after the excavation finished. The digging is over; "+
			"what remains is the demand's own pickup, and a corridor held by a dig with nothing left "+
			"to do in it is an order that can never terminate", lane.Name)
	}
	holders, err := reservations.ActiveMouthRows(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's mouth holds after the dig finished")
	if len(holders) != 1 || holders[0].OrderID != demand.ID || holders[0].Mode != reservations.ModeOutbound {
		t.Fatalf("lane %s holds %+v after its dig finished, want one OUTBOUND row for the demand %d. "+
			"Releasing outright leaves the bin the demand is queued for standing at an open mouth, "+
			"with the slots the dig just emptied as the cheapest shuffle candidates in the group",
			lane.Name, holders, demand.ID)
	}
	finished, err := db.GetOrder(dig.ID)
	testutil.MustNoErr(t, err, "reload the dig")
	if !protocol.IsTerminal(finished.Status) {
		t.Fatalf("the dig is %q with its corridor already handed over. It owes nothing and holds "+
			"nothing, and a row parked in `reshuffling` forever is one every census and every stall "+
			"checker has to learn to ignore", finished.Status)
	}

	// (5) THE DEMAND IS STILL EXACTLY WHERE IT WAS, and still readable. It was
	// never touched by any of the above: no resume, no status change, no
	// re-parenting. The scanner re-resolves it from here on the ordinary
	// lane-clearing events, and it dispatches its OWN plan.
	after, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "reload the demand after the dig finished")
	if after.Status != StatusQueued {
		t.Fatalf("demand status = %q after the dig, want %q — something moved it", after.Status, StatusQueued)
	}
	if kids, _ := db.ListChildOrders(demand.ID); len(kids) != 0 {
		t.Fatalf("the demand acquired %d legs while the dig ran", len(kids))
	}

	// ZERO STATUS EXCURSIONS, asserted against the history rather than the row:
	// the row only shows where it ended up, and the claim is about the whole trip.
	history, err := db.ListOrderHistory(demand.ID)
	testutil.MustNoErr(t, err, "list the demand's history")
	for _, h := range history {
		if h.Status == protocol.StatusReshuffling {
			t.Errorf("the demand passed through %q (history %d) — a complex demand is a CUSTOMER of "+
				"a dig now and must never be re-planned into one", h.Status, h.ID)
		}
	}
}

// driveDigToCompletion runs the dig's legs to terminal the way the fleet would,
// so the test asserts on a finished excavation rather than on a plan.
func driveDigToCompletion(t *testing.T, db *store.DB, d *Dispatcher, dig *orders.Order) {
	t.Helper()
	legs, err := db.ListChildOrders(dig.ID)
	testutil.MustNoErr(t, err, "list legs to drive")
	for _, leg := range legs {
		// CORE CHOOSES THE DESTINATION FIRST. An unbury leg is dispatched with none
		// and dwells in the lane it is digging until the release picks a slot, so a
		// harness that "drives the dig" has to drive that step too — otherwise it is
		// resolving an empty node name and reporting it as a missing row.
		leg = releaseDwell(t, d, db, leg)
		// The blocker physically arrives at the leg's destination.
		dest, err := db.GetNodeByDotName(leg.DeliveryNode)
		testutil.MustNoErr(t, err, "resolve the leg's destination")
		if leg.BinID != nil {
			testutil.MustNoErr(t, db.MoveBinClearingStaging(*leg.BinID, dest.ID, false), "land the blocker")
		}
		// CONFIRMED, not merely completed. AdvanceCompoundOrder's terminal block
		// asks whether every child reached a TERMINAL STATUS; setting completed_at
		// without moving the status leaves the compound looking unfinished, so the
		// terminal block never runs and the lane is never unlocked — which reads,
		// from the outside, exactly like the lock-outliving-its-compound bug this
		// test is here to prove is gone. Direct write because the harness is
		// fast-forwarding through several legal states at once.
		testdb.SeedOrderStatus(t, db, leg.ID, string(StatusConfirmed), "test harness")
	}
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(dig.ID), "advance the dig past its last leg")
}

// TestServiceDig_StaleDigDissolves_DemandUntouched is the isolation property, and
// it is the other half of "a dig is a service".
//
// A service that fails must fail ALONE. Under the old shape this question could
// not even be asked: the demand WAS the compound, so dissolving the dig and
// dissolving the demand were the same act. Now a dig can be dissolved — by the
// stale-compound sweep, by an operator, by its own re-plan — and the demand it
// was serving must be exactly as it was, still waiting, still readable, and free
// to have another dig proposed for it on the next scan.
//
// MUTATION (verified): give the demand the dig's parentage back — i.e. re-parent
// it — and the dissolve takes the demand's status with it, failing the first
// assertion.
func TestServiceDig_StaleDigDissolves_DemandUntouched(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-DIS-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-DIS-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	demand := mkQueuedComplexParent(t, db, "uuid-dissolve-isolation", bp.Code)
	buried := &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID}

	d.handleComplexBuriedOnReplay(demand, buried)
	dig := serviceDigFor(t, db, demand)

	before, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "read the demand before the dissolve")

	// ── THE DIG GOES STALE AND DISSOLVES ──────────────────────────────────
	testutil.MustNoErr(t, d.dissolveCompound(dig.ID, "test: the dig went stale"), "dissolve the dig")

	// (1) THE DEMAND IS UNTOUCHED. Same status, same cause, same sentence.
	after, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "read the demand after the dissolve")
	if after.Status != before.Status {
		t.Fatalf("the demand went %q → %q when the dig dissolved. A dig is a SERVICE: it fails alone, "+
			"and the demand it was serving is not part of its failure", before.Status, after.Status)
	}
	if after.QueueCause != before.QueueCause {
		t.Errorf("the demand's cause changed %q → %q on a dissolve that was not about it",
			before.QueueCause, after.QueueCause)
	}
	if protocol.IsTerminal(after.Status) {
		t.Fatalf("the demand is terminal (%q) — a stale dig took the work it existed to serve with it",
			after.Status)
	}

	// (2) AND IT RE-PROPOSES. The isolation claim is only half a claim without
	// this: a demand nothing will dig for again is stranded, however untouched.
	// The lane is quiet now (the dissolve released it), so the next scan plans a
	// fresh dig — which is what the scanner does on every lane-clearing event.
	d.handleComplexBuriedOnReplay(after, buried)
	fresh := serviceDigFor(t, db, after)
	if fresh.ID == dig.ID {
		t.Fatalf("the re-proposal found the dissolved dig %d rather than a new one", dig.ID)
	}
	legs, err := db.ListChildOrders(fresh.ID)
	testutil.MustNoErr(t, err, "list the fresh dig's legs")
	if len(legs) == 0 {
		t.Error("the fresh dig has no legs — the demand re-proposed but nothing was planned")
	}
}
