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
//
// ── THE RESIDUAL WINDOW, DOCUMENTED AND NOT ENGINEERED (owner's ruling) ───
//
// The status is READ, and the conversion happens a couple of database round trips
// later. A demand that dispatches inside that gap is converted on a status that
// was true when it was read and is not any more — so it degrades to exactly the
// pre-fix behaviour, for milliseconds. That is the whole cost, and closing it
// would mean holding a transaction across the walk.
//
// And it is narrower than it first looks: the converted row is reaped normally by
// the per-block handoff at the demand's own pickup (releaseOrderLaneFor →
// ReleaseLaneHandoff, which deletes any non-dig row for that owner on that lane).
// What orphans a row is the LIFT having already happened, since that is the event
// whose passing leaves nothing to fire the release. So the row only leaks if the
// dispatch AND the lift both land inside the window — and if they do, the next
// blocker-out pass on that lane finds a committed holder and gate 3 releases it.
func TestSelfHandoff_ACollectedDemandReleasesInsteadOfConverting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status protocol.Status
	}{
		{"dispatched", StatusDispatched},
		{"intransit", StatusInTransit},
		// FAULTED IS THE THIRD ARRIVAL, AND IT IS POST-DISPATCH BY CONSTRUCTION.
		// Every inbound edge to `faulted` comes from acknowledged, dispatched,
		// in_transit or staged (protocol/types.go's transition table), so a
		// faulted holder has already been handed to the fleet and — to be here
		// at all, past the claim walk — has already taken its bin out of the
		// lane. A jammed aisle is jammed by the robot, not by a row; an empty
		// aisle held for a faulted order is this same leak wearing a different
		// status, and it is worse than the others because `faulted` is outside
		// the runtime-stuck population, so nothing alarms on it.
		//
		// The bin-comes-back case needs no gate here: a bin set down again in
		// the lane is a bin sitting in the lane, and legStillNeedsLane sees it
		// on the very next evaluation, before control can reach this function.
		{"faulted", StatusFaulted},
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

// TestSelfHandoff_AHolderMovingWithinTheLaneKeepsIt is the Caution as it stands
// after the route-scan was deleted, and it pins TWO things at once.
//
// ── THE SHAPE ─────────────────────────────────────────────────────────────
//
// An order that picks from a lane and puts its bin back down IN THE SAME LANE
// needs the corridor for the whole of that, and no bin-position predicate can see
// why: between the lift and the drop its bin is in the GRIPPER, so
// legStillNeedsLane finds nothing of the holder's in the lane and reports the
// visit finished. What says otherwise is DeliveryNode, which names this lane.
//
// This is the real two-steps-one-lane shape — the operator bin-move door builds
// it, and it is structurally two steps, so its destination IS the lane. The other
// shape, the one that picks from a lane and drops back into it on the way to
// somewhere ELSE, is refused at plan time now
// (TestLaneGate_ResolveHoldsRefusesAPlanThatRevisitsALaneItLeaves): nothing builds
// it, and one mouth row cannot honestly span a robot leaving and returning.
//
// ── AND THE ORDERING, WHICH USED TO LIVE ONLY IN A COMMENT ────────────────
//
// The holder here is `in_transit`, so gate 3 would release this corridor on the
// spot — its whole job is to give back a lane a committed holder has left. What
// stops it is that the caller asks holderStillOwesTheLane FIRST and returns
// before ever reaching the gate. That ordering is the entire safety argument for
// gate 3 and it was asserted nowhere.
//
// So this drives the CALLER, not the predicate. Reorder the population walk, drop
// the holder's second question, or put gate 3 in front of it, and this test is
// what says so — a lane opened in the gap before its own robot drives back down
// it, which is the re-burial window entered from the inbound side.
func TestSelfHandoff_AHolderMovingWithinTheLaneKeepsIt(t *testing.T) {
	t.Parallel()

	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	lane, _, wallSlots, _, _ := clearLaneFixture(t, db, "SELFHV")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "selfhv-demand"
		o.Coordinated = true
		o.Status = StatusInTransit // committed: gate 3 alone would hand the lane back
		// Picks from this lane and sets the bin down again inside it. The bin is
		// in the gripper right now, which is why nothing else in the walk can see
		// that the corridor is still owed.
		o.StepsJSON = `[{"action":"pickup","node":"` + wallSlots[0].Name + `"},` +
			`{"action":"dropoff","node":"` + wallSlots[2].Name + `"}]`
		o.DeliveryNode = wallSlots[2].Name
	})
	if !d.laneLock.TryLock(lane.ID, demand.ID) {
		t.Fatal("the demand could not take the lane it is moving a bin within")
	}

	d.maybeReleaseDigOnLastBlockerOut(lane.ID)

	holders, err := reservations.ActiveMouthRows(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's holders")
	if len(holders) != 1 || holders[0].OrderID != demand.ID {
		t.Fatalf("lane %s holds %+v after a release pass, want demand %d's own row kept. It lifted a "+
			"bin at %s and is carrying it to %s IN THIS LANE — the corridor is owed a drop, and the "+
			"only thing that knows is DeliveryNode, asked BEFORE gate 3 gets to release a committed "+
			"holder. If this fails, check that ordering first.",
			lane.Name, holders, demand.ID, wallSlots[0].Name, wallSlots[2].Name)
	}
}

// TestSelfHandoff_ASecondReleaseKeepsTheRowTheFirstOneCreated is the concurrency
// hole beside the gate, driven sequentially.
//
// ── WHY "THE DIG ROW IS GONE" IS NOT "THE LANE IS YOURS TO RELEASE" ───────
//
// maybeReleaseDigOnLastBlockerOut runs OUTSIDE the lane evaluator's mutex, on
// purpose — waking a lane from inside it is a self-deadlock. So two blocker-out
// events on one lane can both read the same dig owner and both walk. Only the
// conversion is serialized, by HandOffLaneToPicker's advisory lock on the lane.
//
// The first arrival converts: dig row deleted, outbound row inserted for the same
// owner. The second arrival's DELETE then matches ZERO rows — and that used to be
// reported to the caller as a plain "not handed", which the caller reads as
// permission to release. Its release is LaneLock.Unlock → ReleaseLane, whose
// predicate is owner + resource_kind and is MODE-BLIND, so it deletes the outbound
// row the first arrival had just created for that same owner. The naked target
// then stands at an open mouth with nothing holding the corridor — in exactly the
// window the whole exception exists to cover.
//
// This test is the sequential shadow of that interleaving: the two calls are
// ordered rather than raced, which is the same thing the advisory lock does to
// them, and it needs no hook, no goroutine and no sleep to be deterministic.
func TestSelfHandoff_ASecondReleaseKeepsTheRowTheFirstOneCreated(t *testing.T) {
	t.Parallel()

	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	lane, _, _, _, _ := clearLaneFixture(t, db, "SELFHR")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "selfhr-demand"
		o.Coordinated = true // and pre-dispatch: the naked target, the arm that converts
	})
	if !d.laneLock.TryLock(lane.ID, demand.ID) {
		t.Fatal("the demand could not take the lane it excavated")
	}

	if handed := d.handOffDugLane(demand, lane.ID); !handed {
		t.Fatalf("the first arrival did not convert lane %s — the fixture is wrong, not the code "+
			"under test", lane.Name)
	}

	// THE SECOND ARRIVAL. It finds no dig row, because the first arrival's
	// conversion took it.
	if handed := d.handOffDugLane(demand, lane.ID); !handed {
		t.Fatalf("a second release pass over lane %s reported the lane as the caller's to release. "+
			"There is no dig row left because the FIRST pass converted it into demand %d's outbound "+
			"hold — so releasing here deletes that fresh row (ReleaseLane is mode-blind and matches "+
			"the same owner), and the bin the excavation uncovered is left standing at an open mouth.",
			lane.Name, demand.ID)
	}

	// AND THE ROW IS STILL THERE. The return value is the mechanism; this is the
	// consequence, and it is the one the plant feels.
	holders, err := reservations.ActiveMouthRows(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's holders")
	if len(holders) != 1 || holders[0].OrderID != demand.ID ||
		holders[0].Mode != reservations.ModeOutbound {
		t.Fatalf("lane %s holds %+v after a second release pass, want the ONE OUTBOUND row the first "+
			"pass created for demand %d. A second pass must not be able to undo the handoff it finds.",
			lane.Name, holders, demand.ID)
	}
}

// TestSelfHandoff_ADispatchedHolderWithItsBinStillInTheLaneKeepsIt pins the
// CALLER FILTER that gate 3's `dispatched` arm depends on and never states.
//
// `dispatched` does not mean "has collected". It means the fleet has the order;
// the robot may still be driving in, with the target bin standing at the mouth.
// Releasing there is the naked-target re-burial window, entered on the arm that
// is supposed to close it.
//
// What makes gate 3 safe is entirely upstream: control reaches handOffDugLane
// only because legStillNeedsLane returned false, and that predicate is claim-keyed
// over the order's bins — so a dispatched demand whose bin is still in the lane
// never gets there. That guarantee is transitive, and until this test nothing
// asserted it: both gate-3 pins call handOffDugLane directly against an EMPTY
// lane, where the filter is satisfied trivially.
//
// So this one drives the whole walk with a bin actually in the corridor. Move the
// holderStillOwesTheLane/legStillNeedsLane calls, reorder the population walk, or
// add a second caller of handOffDugLane, and this is what says so.
func TestSelfHandoff_ADispatchedHolderWithItsBinStillInTheLaneKeepsIt(t *testing.T) {
	t.Parallel()

	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	lane, _, wallSlots, _, payload := clearLaneFixture(t, db, "SELFHF")

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "selfhf-demand"
		o.Coordinated = true
		o.Status = StatusDispatched // committed to the fleet — but not yet arrived
	})
	// Its target is still sitting at the mouth, claimed, waiting to be collected.
	target := testdb.CreateBinAtNode(t, db, payload.Code, wallSlots[0].ID, "SELFHF-TARGET")
	testdb.ClaimBinForTest(t, db, target.ID, demand.ID)

	if !d.laneLock.TryLock(lane.ID, demand.ID) {
		t.Fatal("the demand could not take the lane it resolved onto")
	}

	d.maybeReleaseDigOnLastBlockerOut(lane.ID)

	holders, err := reservations.ActiveMouthRows(db.DB, lane.ID)
	testutil.MustNoErr(t, err, "read the lane's holders")
	if len(holders) != 1 || holders[0].OrderID != demand.ID {
		t.Fatalf("lane %s holds %+v after a release pass, want demand %d's own row kept. Its bin %d is "+
			"STILL SITTING at %s — the robot has not arrived yet — so there is nothing transported and "+
			"nothing collected, and opening the corridor here lets the next shuffle bury the bin this "+
			"demand is on its way to fetch.",
			lane.Name, holders, demand.ID, target.ID, wallSlots[0].Name)
	}
}
