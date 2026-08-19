//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dig_self_handoff_docker_test.go — the named pin on handOffDugLane's SURVIVING
// arm, written before the limb around it was deleted.
//
// ── WHY THIS FILE EXISTS AT ALL ───────────────────────────────────────────
//
// §R.106 boarded handOffDugLane for deletion on the sentence "the handoff finds
// no collector; no-op". That sentence was already stale when it was written — the
// self-handoff landed at eb7d4b63, BEFORE the funeral — and the boarding inherited
// it. §R.111 caught it 5/5 and §R.112 confirmed it at the tree. It is the second
// time this program has called a dormant path dead in this same seam
// (ResumeCompound, §R.90), and the standing rule that came out of it is that
// deletions get a second reader.
//
// This is that second reader, written as an assertion instead of an opinion.
//
// ── WHAT WAS ACTUALLY TRUE ABOUT THE COVERAGE, MEASURED ───────────────────
//
// The brief said "nothing pins it today". That is not quite right, and the
// correction matters more than the claim: driving the mutation (make
// handOffDugLane return false unconditionally) against the whole docker suite
// fails FOUR tests, and two of them survive the deletion —
// TestServiceDig_BuriedComplexDemand_DigsThenDispatchesItsOwnPlan
// (lane_clear_end_to_end_docker_test.go:149, "want one OUTBOUND row for the
// demand") and TestStacked_BuriedIrreplaceableNeed_InALockedLane_InAFullGroup.
// So the arm IS pinned, end to end, by tests whose names are about something
// else.
//
// What was NOT true is the note at dig_dwell_docker_test.go that claimed this
// same mutation fires its DRIVE-OUT assertion. Re-driven this session: it does
// not. That is a FOURTH stale `MUTATION` note on this branch, found the only way
// they can be found — by running it.
//
// So this file is not the arm's only cover. It is the cover that NAMES it: after
// the deletion, the two end-to-end tests are the only things standing between
// this function and the next person who greps for callers, finds one, and
// concludes it is nearly dead. A pin that says what it protects is what stops
// that reading a third time.
//
// ── WHAT IT PROTECTS ──────────────────────────────────────────────────────
//
// The naked-target window. When a coordinated demand finishes excavating its own
// lane, releasing the corridor outright leaves the bin it dug for standing at an
// open mouth with the slots the dig just emptied as the cheapest shuffle
// candidates in the group — so the next order wanting one re-buries the bin the
// excavation was run to expose, in the gap before the demand's own dispatch. The
// hold is converted to OUTBOUND rather than kept as a dig, because what has to be
// excluded is precisely a DROP into that lane.
//
// MUTATION (driven this session, fires): make handOffDugLane return false
// unconditionally. The first sub-test reports the lane holding nothing.
func TestSelfHandoff_ACoordinatedDemandKeepsItsOwnCorridorAsOutbound(t *testing.T) {
	t.Parallel()

	t.Run("coordinated: the dig row becomes the demand's own outbound hold", func(t *testing.T) {
		t.Parallel()
		db := testDBShared(t)
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
		lane, _, _, _, _ := clearLaneFixture(t, db, "SELFHO")

		demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = "selfho-demand"
			o.Coordinated = true
		})
		if !d.laneLock.TryLock(lane.ID, demand.ID) {
			t.Fatal("the demand could not take the lane it is about to excavate")
		}

		if handed := d.handOffDugLane(demand, lane.ID); !handed {
			t.Fatalf("demand %d finished its own excavation of %s and handOffDugLane said it had "+
				"nothing to hand over. The caller reads that as 'the lane is mine to release', and "+
				"releasing it opens the corridor with the uncovered bin standing at the mouth — the "+
				"naked-target re-burial window this arm exists to close.", demand.ID, lane.Name)
		}

		holders, err := reservations.ActiveMouthRows(db.DB, lane.ID)
		testutil.MustNoErr(t, err, "read the lane's holders")
		if len(holders) != 1 || holders[0].OrderID != demand.ID ||
			holders[0].Mode != reservations.ModeOutbound {
			t.Fatalf("lane %s holds %+v, want ONE OUTBOUND row for the demand %d itself. Outbound is "+
				"the mode because what must be excluded is a DROP into this lane; it also makes the "+
				"demand's own dispatch idempotent, since AcquireLanes asks for outbound and finds "+
				"this row.", lane.Name, holders, demand.ID)
		}
	})

	t.Run("plain: a demand whose fetch was its own leg has nothing standing", func(t *testing.T) {
		t.Parallel()
		db := testDBShared(t)
		d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
		lane, _, _, _, _ := clearLaneFixture(t, db, "SELFHOP")

		demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID = "selfhop-demand"
			o.Coordinated = false
		})
		if !d.laneLock.TryLock(lane.ID, demand.ID) {
			t.Fatal("the demand could not take the lane")
		}

		if handed := d.handOffDugLane(demand, lane.ID); handed {
			t.Fatalf("a PLAIN demand kept the corridor on lane %s. Its fetch is one of its own legs — "+
				"the bin leaves the lane inside the compound — so nothing is left standing at the "+
				"mouth to protect, and holding here is a lane shut with nothing running in it.",
				lane.Name)
		}
	})
}
