//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestOccupancy_ReleasedWhenTheRobotLeavesNotWhenItDrops is the five-robot jam,
// reproduced and then prevented.
//
// ── WHAT WENT WRONG ───────────────────────────────────────────────────────
//
// Occupancy — the "a robot is inside this corridor" row — was released only when
// a DROPOFF block completed. That is right for an order whose visit ends in a
// drop, and wrong for one that enters, picks, and drives out to do something
// else: the row outlives the visit, and the lane reads occupied by an order that
// has gone.
//
// Measured on the lane-stress rig 2026-08-10: one robot picked from a lane and
// drove on to its next gate point. Its row stayed. Four more queued at that
// lane's mouth were refused with lane-occupied — by an order that had left — and
// the holder was itself queued behind its own stale row, self-exempting, so
// nothing in the set could move it. Five robots in front of an EMPTY lane.
//
// ── WHY NOT JUST WAIT FOR THE DROP ────────────────────────────────────────
//
// The next drop can be a line delivery ten or twenty minutes out, and the
// corridor is falsely occupied for every second of it. The owner's ruling was to
// release on the pickup and watch for the consequence — see
// HandleTransitForLaneGate for the trade and for what to build if it bites.
//
// ── MUTATIONS RUN (both fire) ─────────────────────────────────────────────
//
//  1. drop the releaseOccupancyOnExit call from HandleTransitForLaneGate →
//     (a) the row survives the exit and (c) the waiting order is still refused.
//     That is the jam, exactly.
//  2. keep the release but drop its EvaluateLaneReleases → (a) and (b) pass,
//     (c) fails: the lane is genuinely free and the robot at its mark is never
//     told. A bookkeeping correction that wakes nobody is not a fix.
func TestOccupancy_ReleasedWhenTheRobotLeavesNotWhenItDrops(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	lane, park, w, p, _ := healLaneFixture(t, db, "EXITOCC")
	line := lineNode(t, db, "EXITOCC-LINE")

	// THE LEAVER: an order that entered the lane and is picking a bin OUT of it.
	// Its own dropoff is elsewhere and minutes away, which is the whole point.
	leaver := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = w[0].Name
		o.DeliveryNode = line.Name
		o.Status = "in_transit"
	})
	if err := d.TakeLaneOccupancy(leaver.ID, w[0]); err != nil {
		t.Fatalf("seed the leaver's occupancy: %v", err)
	}
	occ, err := reservations.OccupantsOf(db.DB, lane.ID)
	if err != nil || len(occ) != 1 {
		t.Fatalf("precondition: lane should have exactly one occupant, got %v (err %v)", occ, err)
	}

	// THE WAITER: gate-staged at the same lane's mark, refused by the leaver.
	waiter := stageGatedStore(t, db, d, line, w[1], nil)
	if !IsGateStaged(waiter) {
		t.Fatalf("the waiter is not gate-staged (wait_index=%d vendor=%q)",
			waiter.WaitIndex, waiter.VendorOrderID)
	}
	markStaged(t, db, waiter.ID)
	d.EvaluateLaneReleases(lane.ID)

	parked, err := db.GetOrder(waiter.ID)
	if err != nil {
		t.Fatalf("reload waiter: %v", err)
	}
	if parked.QueueCause != string(CauseLaneOccupied) {
		t.Fatalf("waiter cause = %q, want %q — the fixture is not reproducing a lane-occupied refusal",
			parked.QueueCause, CauseLaneOccupied)
	}
	if n := appendsTo(backend, waiter.VendorOrderID); n != 0 {
		t.Fatalf("the waiter already has %d tail append(s) — it is not actually waiting", n)
	}

	// ── THE LEAVER PICKS AND DRIVES OUT. No dropoff — that is minutes away. ──
	//
	// NO BIN IS SEEDED IN THE LANE, deliberately. An unclaimed bin sitting in
	// front of the waiter's slot is a WALL, and the evaluator correctly answers a
	// wall with a heal dig — which takes the lane's dig lock and refuses the
	// waiter for a completely different and legitimate reason. That would make
	// this test green or red for reasons that have nothing to do with occupancy.
	// The exit signal resolves its lane from the node id alone.
	d.HandleTransitForLaneGate(leaver.ID, w[0].ID)

	// (a) THE ROW GOES WITH THE ROBOT.
	occ, err = reservations.OccupantsOf(db.DB, lane.ID)
	if err != nil {
		t.Fatalf("read occupants: %v", err)
	}
	for _, o := range occ {
		if o == leaver.ID {
			t.Errorf("the leaver still holds occupancy on the lane it just picked out of. Its next " +
				"drop is at a line node minutes away, and until then this row refuses every other " +
				"entrant to a corridor nobody is in")
		}
	}

	// (b) AND ONLY THAT LANE'S ROW. An order can be inside two corridors across
	// its plan, and leaving one says nothing about the other — so the release is
	// per-lane. ReleaseAllOccupancy here would drop a presence that is still true,
	// which is the same error as the stale row in the other direction.
	if err := d.TakeLaneOccupancy(leaver.ID, w[0], p[0]); err != nil {
		t.Fatalf("take occupancy on both lanes for the scoping check: %v", err)
	}
	d.HandleTransitForLaneGate(leaver.ID, w[0].ID) // exits the WALL lane only

	stillIn, err := reservations.OccupantsOf(db.DB, park.ID)
	if err != nil {
		t.Fatalf("read park-lane occupants: %v", err)
	}
	held := false
	for _, o := range stillIn {
		if o == leaver.ID {
			held = true
		}
	}
	if !held {
		t.Errorf("leaving lane %s also dropped the leaver's presence in %s, which it has not left. "+
			"Presence is per-corridor; a per-order release makes a lane read empty while a robot is "+
			"still in it — the collision the row exists to prevent", lane.Name, park.Name)
	}

	// (c) THE WAITER GOES IN. This is the assertion that matters: the lane being
	// free is invisible until somebody re-asks, and the release is the last thing
	// that happens in the leaver's visit — every event it emitted fired while the
	// row was still there.
	if n := appendsTo(backend, waiter.VendorOrderID); n == 0 {
		after, _ := db.GetOrder(waiter.ID)
		t.Fatalf("the waiter never went in after the lane emptied — status %s, cause %q. The "+
			"corridor is free and the robot at its mark was not told; on a quiet lane nothing else "+
			"will tell it", after.Status, after.QueueCause)
	}
}

// TestOccupancy_ExitReleasesTheLEGsRowNotItsParents is the same jam on the
// population that actually produces most of it: a compound dig's legs.
//
// ── WHY THE SIBLING TEST ABOVE COULD NOT CATCH THIS ───────────────────────
//
// Its leaver is a plain, PARENTLESS order, so laneOwnerFor returns the order's
// own id and "release by owner" and "release by holder" are the same statement.
// Every occupancy test in this package was parentless the same way. The exit
// release therefore shipped keyed on the compound PARENT — and the delete is
// `WHERE order_id=$1`, so for a dig leg it matched nothing at all. The row
// survived to the dropoff exactly as it had before the fix existed: the whole
// commit was inert on the population it mattered most for.
//
// ── THE TWO OWNERSHIPS, WHICH IS THE THING TO REMEMBER ────────────────────
//
// A compound's MOUTH row is taken by the parent and released by the parent —
// the legs share one inbound hold, so routing a leg's release to `owner` is
// correct there, and that line is unchanged. OCCUPANCY is taken per LEG
// (compound.go's TakeLaneOccupancy(next.ID)) because presence is about the robot
// that is physically inside, and one compound has several of those across its
// plan. Two holds, two ownerships, one function — which is exactly how the wrong
// routing got copied onto the right neighbour.
//
// MUTATION RUN (fires): restore `d.releaseOccupancyOnExit(owner, node)` in
// HandleTransitForLaneGate → assertion (a) fails with the leg's row still on the
// lane, and (b) fails with the waiter still refused. That is the production
// symptom, on the leg population, verbatim.
func TestOccupancy_ExitReleasesTheLEGsRowNotItsParents(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	lane, _, w, _, _ := healLaneFixture(t, db, "LEGOCC")
	line := lineNode(t, db, "LEGOCC-LINE")

	// A dig: a parent in `reshuffling` and the leg that is actually in the lane.
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = w[0].Name
		o.DeliveryNode = line.Name
		o.Status = StatusReshuffling
	})
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.SourceNode = w[0].Name
		o.DeliveryNode = line.Name
		o.Status = "in_transit"
		o.ParentOrderID = &parent.ID
		o.Sequence = 1
	})
	if leg.ParentOrderID == nil || *leg.ParentOrderID != parent.ID {
		t.Fatalf("fixture: the leg is not a child (parent=%v) — this test is only a test while it is",
			leg.ParentOrderID)
	}

	// Occupancy is taken the way compound.go takes it: keyed on the LEG.
	if err := d.TakeLaneOccupancy(leg.ID, w[0]); err != nil {
		t.Fatalf("seed the leg's occupancy: %v", err)
	}

	waiter := stageGatedStore(t, db, d, line, w[1], nil)
	markStaged(t, db, waiter.ID)
	d.EvaluateLaneReleases(lane.ID)
	parked, err := db.GetOrder(waiter.ID)
	if err != nil {
		t.Fatalf("reload waiter: %v", err)
	}
	if parked.QueueCause != string(CauseLaneOccupied) {
		t.Fatalf("waiter cause = %q, want %q — the fixture is not reproducing a lane-occupied refusal",
			parked.QueueCause, CauseLaneOccupied)
	}

	// The LEG picks and drives out. The event carries the leg's id, as the real
	// emitter does — the bin was picked by the leg, not by its parent.
	d.HandleTransitForLaneGate(leg.ID, w[0].ID)

	// (a) THE LEG'S ROW GOES.
	occ, err := reservations.OccupantsOf(db.DB, lane.ID)
	if err != nil {
		t.Fatalf("read occupants: %v", err)
	}
	for _, o := range occ {
		if o == leg.ID {
			t.Errorf("dig leg %d (parent %d) still holds occupancy on the lane it just picked out "+
				"of. Releasing under the PARENT's id deletes nothing — the row is the leg's — so the "+
				"corridor stays occupied by a robot that has gone, which is the five-robot jam on the "+
				"population that produces most of it", leg.ID, parent.ID)
		}
	}

	// (b) AND THE WAITER GOES IN. The row being gone is bookkeeping; this is the
	// floor behaviour the whole fix exists for.
	if n := appendsTo(backend, waiter.VendorOrderID); n == 0 {
		after, _ := db.GetOrder(waiter.ID)
		t.Fatalf("the waiter never went in after the dig leg left — status %s, cause %q",
			after.Status, after.QueueCause)
	}
}
