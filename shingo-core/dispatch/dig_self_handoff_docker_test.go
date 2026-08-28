//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
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

// TestSelfHandoff_ACollectedDemandReleasesInsteadOfConverting is the ORDERING
// case: the demand dispatches and lifts BEFORE the release evaluates.
//
// ── WHY THE OBVIOUS GUARD IS INERT, AND WHAT THE REAL ONE KEYS ON ─────────
//
// Control reaches handOffDugLane only because legStillNeedsLane already returned
// false, and that predicate is CLAIM-keyed. It returns false in both states that
// arrive here, and they want opposite answers:
//
//	NAKED TARGET      — the demand has not dispatched, holds no claim yet, and its
//	                    uncovered bin stands at an open mouth. MUST hold.
//	ALREADY COLLECTED — the demand dispatched, claimed, and lifted the bin OUT.
//	                    Nothing stands at the mouth. Converting pins the corridor
//	                    for the whole transport leg, and permanently when the
//	                    holder cannot terminate.
//
// So the discriminator is DISPATCH STATE, never a live claim, and the predicate
// is swapLegCommittedToFleet — the shape already written for this same mistake in
// the mirror direction, whose `reshuffling` arm rules the mid-dig case
// not-committed and which this guard inherits rather than re-decides.
//
// OBSERVED LIVE (houseserver, main 1a6b6d23, 2026-08-28; wall clock): the
// conversion fires while the holder is ALREADY in_transit — order 88 held Lane_14
// as dig through `reshuffling` and `dispatched`, and the row flipped to
// outbound/dighandoff one second after `in_transit`, then pinned Lane_14 for the
// entire drive to Lane_15. On the collision draw it wedges: order 142 held
// Lane_15 outbound with its bin already at _TRANSIT while its swap sibling 143
// sat in `sourcing` under `lane-held-traffic` needing to deliver INTO Lane_15 —
// each waiting on the other, with nothing physically in the corridor.
func TestSelfHandoff_ACollectedDemandReleasesInsteadOfConverting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status protocol.Status
	}{
		{"dispatched", StatusDispatched},
		{"intransit", StatusInTransit},
	} {
		t.Run(tc.name+": the corridor is released, not converted", func(t *testing.T) {
			t.Parallel()
			db := testDBShared(t)
			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
			lane, _, _, _, _ := clearLaneFixture(t, db, "SELFHC"+tc.name[:4])

			demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID = "selfhc-" + tc.name
				o.Coordinated = true
				o.Status = tc.status
			})
			if !d.laneLock.TryLock(lane.ID, demand.ID) {
				t.Fatal("the demand could not take the lane it excavated")
			}

			if handed := d.handOffDugLane(demand, lane.ID); handed {
				t.Fatalf("demand %d is %s — it has already collected and its bin is out of lane %s — "+
					"yet handOffDugLane kept the corridor as its own outbound hold. There is no naked "+
					"target left to protect: the row now pins an empty corridor for the whole transport "+
					"leg, and forever when the holder cannot terminate.", demand.ID, tc.status, lane.Name)
			}

			// AND THE LANE ENDS HELD BY NOBODY. handOffDugLane only reports; the
			// caller's release arm is what acts on the report, so the end state is
			// the claim that matters to the plant.
			d.maybeReleaseDigOnLastBlockerOut(lane.ID)
			holders, err := reservations.ActiveMouthRows(db.DB, lane.ID)
			testutil.MustNoErr(t, err, "read the lane's holders")
			if len(holders) != 0 {
				t.Fatalf("lane %s still holds %+v after a collected demand's release evaluated. A row "+
					"surviving here IS the leak: a mouth hold owned by an order that already took its "+
					"bin out of this lane, excluding every inbound comer until it terminates.",
					lane.Name, holders)
			}
		})
	}
}

// TestSelfHandoff_AHolderThatDropsBackIntoTheLaneKeepsIt is the Caution, and it
// is the one place the release guard could be worse than the hold it replaces.
//
// A plan may pick from AND later drop back into the SAME lane — resolveOrderLaneHolds
// and resolvePlanLaneHolds both deduplicate that to ONE row with dig as the
// stronger mode, because "an order that both picks from and drops into a lane owns
// it for the whole visit". Mouth holds are acquired once at dispatch, not per
// step, so that single row is all the corridor protection the drop-back has.
//
// Such a holder is COMMITTED by the time it lifts, so the dispatch-state guard
// alone would release the lane it is about to drive back down. legStillNeedsLane
// cannot see it either — the drop-back bin is in the GRIPPER, not in the lane —
// and holderStillOwesTheLane checked only DeliveryNode, which names the FINAL
// destination and not an intermediate dropoff step.
//
// So the question is asked of the REMAINING STEPS. Reachable through the operator
// bin-move door, which is how the whole-visit shape surfaced in the first place.
func TestSelfHandoff_AHolderThatDropsBackIntoTheLaneKeepsIt(t *testing.T) {
	t.Parallel()

	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	lane, _, wallSlots, parkSlots, _ := clearLaneFixture(t, db, "SELFHV")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "selfhv-demand"
		o.Coordinated = true
		o.Status = StatusInTransit
		// Picks from this lane and drops BACK into it: the whole-visit shape.
		// The FINAL destination is a different lane on purpose — DeliveryNode is
		// the only thing the predicate used to read, so a plan whose drop-back is
		// an INTERMEDIATE step is exactly the shape that slipped past it.
		o.StepsJSON = `[{"action":"pickup","node":"` + wallSlots[0].Name + `"},` +
			`{"action":"dropoff","node":"` + wallSlots[2].Name + `"},` +
			`{"action":"pickup","node":"` + parkSlots[0].Name + `"},` +
			`{"action":"dropoff","node":"` + parkSlots[1].Name + `"}]`
		o.DeliveryNode = parkSlots[1].Name
	})

	owes, why := d.holderStillOwesTheLane(demand, lane.ID)
	if !owes {
		t.Fatalf("demand %d still has a DROPOFF into lane %s ahead of it (%s), and its bin is in the "+
			"gripper where no bin-position predicate can see it — yet holderStillOwesTheLane said it "+
			"owes the lane nothing (%q). Releasing here opens the corridor in the gap before the robot "+
			"drives back down it, which is the re-burial window entered from the inbound side.",
			demand.ID, lane.Name, wallSlots[2].Name, why)
	}
}
