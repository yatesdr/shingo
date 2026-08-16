//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// gated_leg_occupancy_docker_test.go — THE PHANTOM ROW, AND THE CYCLE IT CLOSED.
//
// A compound leg passes its two ENDPOINTS to TakeLaneOccupancy, which is not the
// same set every other dispatch path passes: the others pass what they ENTER. On
// an ungated lane those are the same set and the difference never showed. On a
// GATED lane they are not: the create sends the robot to the mark, OUTSIDE the
// corridor, and the row belongs to the tail append that actually goes in
// (fleet_handover.go states the rule; appendGateTail implements it).
//
// So a leg bound for a gated lane declared itself inside a corridor it was
// standing next to, and that row refused every other order the lane.
//
// ── WHY THIS IS A DEADLOCK AND NOT A WAIT ─────────────────────────────────
//
// Measured on the lane-stress rig 2026-08-12 and frozen for 997 seconds
// (EVIDENCE-rig-wedge-frozen-state-2026-08-12.txt, PLAN §R.54/R.55). Four orders
// of one episode, and the phantom row is the edge that closes the ring:
//
//	order 10  dig leg, place bin 7 at LSD_009      → lane-deeper-pending, waiting on order 7
//	order 7   place a bin at the deeper LSD_010    → dropoff-capacity, waiting on bin 6 to leave
//	order 1   pick bin 6 out of LSD_010            → LANE-OCCUPIED, waiting on order 10's row
//
// Order 1 is the only order that can empty LSD_010, and the thing refusing it is
// a row belonging to a robot that is not in the lane. Deleting that single row on
// the live rig unwedged all four in two minutes: two legs confirmed, order 7
// dispatched, order 1 released. This test is that finding turned into a fixture
// (law 11) — the two-order half, which needs no demand at all.
//
// MUTATION (verified): drop the laneIsGated skip in TakeLaneOccupancy — i.e.
// restore the unconditional take — and assertion (2) fires with the gated lane
// holding a row for a robot at its mark, followed by (3): the second order is
// refused CauseLaneOccupied. That is the frozen shape, reproduced.
func TestGatedLeg_TakesNoOccupancyOnTheLaneItStandsOutsideOf(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// The rig's orientation exactly: pick out of an UNGATED lane, place into a
	// GATED one. healLaneFixture marks `wall` and leaves `park` unmarked.
	wall, park, w, p, bp := healLaneFixture(t, db, "PHANTOM")
	createTestBinAtNode(t, db, bp.Code, p[0].ID, "BIN-PHANTOM")

	// THE LANE MUST BE CONTENDED, or the leg never dwells and there is no phantom
	// to have: the valve appends an admitted order's tail back to back with its
	// create, precisely so a robot with a clear lane never waits
	// (lane_gate_dispatch.go). The rig's contention was order 7 — an undispatched
	// store aimed at a DEEPER slot — which parks the leg on Tier-2 ordering
	// (lane-deeper-pending, lane_entry.go). Same shape here, and deliberately an
	// ORDERING cause rather than an occupancy one: contending with the very fact
	// under test would make the assertions circular.
	deeper := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = StatusQueued
		o.DeliveryNode = w[2].Name // deeper than the leg's w[0], and not yet dispatched
	})
	_ = deeper

	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = StatusReshuffling
	})
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.Sequence = 1
		o.Status = StatusPending
		o.SourceNode = p[0].Name // ungated: the robot really does go in here
		o.DeliveryNode = w[0].Name
	})

	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parent.ID), "dispatch the dig's first leg")

	// (0) THE LEG IS ACTUALLY DWELLING AT THE MARK. Without this, "no row on the
	// gated lane" is equally consistent with a leg that never dispatched, which is
	// a test that passes for the wrong reason — the failure mode this program has
	// already been bitten by once (PLAN §R.51).
	sent, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "reload the leg after dispatch")
	if sent.VendorOrderID == "" {
		t.Fatalf("the leg was never dispatched (status %q, cause %q) — nothing is standing at any mark, "+
			"so this fixture proves nothing about occupancy", sent.Status, sent.QueueCause)
	}
	if !IsGateStaged(sent) {
		t.Fatalf("the leg is not gate-staged (wait_index=%d) — it drove straight into %s, so the "+
			"gated case this test is about did not happen", sent.WaitIndex, wall.Name)
	}

	// (1) THE LANE IT IS GENUINELY IN STILL HOLDS A ROW. Asserted FIRST and
	// separately, because the whole risk of this fix is over-correction: a change
	// that simply stopped recording presence would satisfy (2) and (3) while
	// reopening F-12 — a robot inside a corridor that reads empty to the next
	// entrant, which is what Hold B exists to prevent.
	if !occupies(t, db, park.ID, leg.ID) {
		t.Errorf("the leg holds no occupancy row on %s, the UNGATED lane it was sent into. Presence "+
			"must still be recorded where the robot actually is — skipping the gated lane is not "+
			"licence to skip the other one", park.Name)
	}

	// (2) AND NONE ON THE LANE IT IS STANDING OUTSIDE OF. The phantom.
	//
	// Errorf rather than Fatalf so (3) still runs: the row and the refusal it
	// causes are one finding, and a mutation should report both halves rather than
	// stopping at the symptom that happens to be checked first.
	if occupies(t, db, wall.ID, leg.ID) {
		t.Errorf("the leg holds an occupancy row on %s while dwelling at that lane's mark. It is NOT "+
			"in the corridor: the tail append is the moment it goes in, and appendGateTail takes the "+
			"row there. This row is the one that froze four orders for 997 seconds on the rig", wall.Name)
	}

	// (3) SO THE LANE IS STILL ENTERABLE — which is the consequence, and the only
	// half of this that a deadlocked plant can feel. An unrelated order asking
	// admission for the same gated lane must be admitted, not refused for an
	// occupant that is standing outside.
	other := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = StatusQueued
		o.DeliveryNode = w[1].Name
	})
	v, err := d.admitLane(admissionSituation{order: other}, w[1], false)
	testutil.MustNoErr(t, err, "ask admission for the gated lane")
	if !v.Admitted() {
		t.Fatalf("admission refused order %d into %s with %q — the cycle can still form: the only "+
			"order able to clear the deeper slot is held out by a robot that is not in the lane",
			other.ID, wall.Name, v.Cause())
	}
}

// occupies reports whether this order holds an occupancy row on this lane.
//
// Reads the reservation rows rather than any dispatcher state: the row IS the
// fact admission arbitrates on (admitLane's occupancy arm calls the same
// OccupantsOf), so asserting on anything else would be asserting on a mirror.
func occupies(t *testing.T, db *store.DB, laneID, orderID int64) bool {
	t.Helper()
	occ, err := reservations.OccupantsOf(db.DB, laneID)
	testutil.MustNoErr(t, err, "read occupants")
	for _, o := range occ {
		if o == orderID {
			return true
		}
	}
	return false
}
