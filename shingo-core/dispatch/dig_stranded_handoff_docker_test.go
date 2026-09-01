//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// dig_stranded_handoff_docker_test.go — the pins maybeReleaseStrandedHandoff
// shipped without.
//
// ── WHY THIS FILE EXISTS ──────────────────────────────────────────────────
//
// b7b41ebd added the releaser the converted dighandoff row never had, and it is
// the only behavioural commit in its batch with zero test lines — and the one
// whose first cut shipped with the rule backwards. The review round found four
// defects in it, three of which are arms nothing here could have caught because
// nothing here existed.
//
// So each test below names one arm and says what the plant feels when that arm
// is wrong. Two of them (reshuffling, collected) pin behaviour that was already
// right; three drove the amendments red first.
//
// ── THE SHAPE UNDER TEST ──────────────────────────────────────────────────
//
// A coordinated demand excavates a lane, keeps the corridor as its own OUTBOUND
// row tagged `dighandoff` (handOffDugLane), and then goes somewhere the walk
// that created the row can no longer see: the row's only remaining releasers
// are the demand's own block progress at a node IN THIS LANE and its
// terminalization. A demand that re-resolves elsewhere produces neither, and the
// corridor stays shut for the rest of that order's life.

// strandedHandoffFixture builds exactly that state and hands back the pieces:
// a marked lane whose only reservation is one `dighandoff` row owned by a
// coordinated demand.
//
// The demand is created PRE-DISPATCH deliberately — that is the only state
// handOffDugLane converts in (gate 3 releases a committed or faulted holder
// rather than converting) — so a test that wants a stranded owner in some other
// status seeds it AFTER the conversion, which is the ordering the plant
// produces too.
//
// `shape` (nil for most tests) is handed the lane's slots as well as the order,
// because the one field that matters here — DeliveryNode — names a node the
// fixture has only just created.
func strandedHandoffFixture(t *testing.T, name string, shape func(*orders.Order, []*nodes.Node)) (
	*Dispatcher, *store.DB, *nodes.Node, []*nodes.Node, *payloads.Payload, *orders.Order,
) {
	t.Helper()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	lane, _, wallSlots, _, payload := clearLaneFixture(t, db, name)

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "stranded-" + name
		o.Coordinated = true // gate 2: only a coordinated demand keeps its corridor
		if shape != nil {
			shape(o, wallSlots)
		}
	})

	if !d.laneLock.TryLock(lane.ID, demand.ID) {
		t.Fatal("the demand could not take the lane it is about to excavate")
	}
	if handed := d.handOffDugLane(demand, lane.ID); !handed {
		t.Fatal("the fixture produced no dighandoff row — the setup is wrong, not the code under test")
	}
	if h := mouthHolders(t, db, lane.ID); len(h) != 1 || h[0].ReservedBy != reservations.ByDigHandoff {
		t.Fatalf("fixture: lane holds %+v, want one dighandoff row", h)
	}
	return d, db, lane, wallSlots, payload, demand
}

func mouthHolders(t *testing.T, db *store.DB, laneID int64) []reservations.MouthHold {
	t.Helper()
	holders, err := reservations.ActiveMouthRows(db.DB, laneID)
	testutil.MustNoErr(t, err, "read the lane's holders")
	return holders
}

// TestStrandedHandoff_AReshufflingOwnerKeepsIt is gate 2 doing its job, and it
// is the arm the whole exception exists for: the dig has finished a leg but the
// demand has not been handed back to the scanner, so its uncovered bin is
// standing at an open mouth with the slots the dig just emptied as the cheapest
// shuffle candidates in the group. Releasing here re-buries the bin the
// excavation was run to expose.
func TestStrandedHandoff_AReshufflingOwnerKeepsIt(t *testing.T) {
	t.Parallel()

	d, db, lane, _, _, demand := strandedHandoffFixture(t, "STRRE", nil)
	testdb.SeedOrderStatus(t, db, demand.ID, string(StatusReshuffling), "")

	d.maybeReleaseStrandedHandoff(lane.ID)

	if h := mouthHolders(t, db, lane.ID); len(h) != 1 || h[0].OrderID != demand.ID {
		t.Fatalf("lane %s holds %+v after a stranded walk, want demand %d's row KEPT. It is still "+
			"`reshuffling` — the dig has not finished and the demand has not been handed back to the "+
			"scanner — which is the naked-target gap this row exists to cover.",
			lane.Name, h, demand.ID)
	}
}

// TestStrandedHandoff_ACollectedOwnerReleases is gate 3's rule asked a second
// time. A demand that dispatched, lifted and drove off leaves nothing at the
// mouth to protect, and its route may never return here — so no per-visit
// release will ever fire and the row lives to terminalization, excluding every
// inbound comer from an empty corridor.
func TestStrandedHandoff_ACollectedOwnerReleases(t *testing.T) {
	t.Parallel()

	d, db, lane, _, _, demand := strandedHandoffFixture(t, "STRCO", nil)
	testdb.SeedOrderStatus(t, db, demand.ID, string(StatusInTransit), "")

	d.maybeReleaseStrandedHandoff(lane.ID)

	if h := mouthHolders(t, db, lane.ID); len(h) != 0 {
		t.Fatalf("lane %s still holds %+v. Demand %d is in_transit with its bin out of the corridor — "+
			"the row now pins an empty aisle until the order terminates, and the swap partner that "+
			"needs to drop INTO this lane is refused by it.", lane.Name, h, demand.ID)
	}
}

// TestStrandedHandoff_AnOwnerStillOwingThisLaneADropKeepsIt is the SECOND
// QUESTION, and it is the one the stranded walk shipped without.
//
// The dig arm asks the holder both questions — legStillNeedsLane, then
// holderStillOwesTheLane — because the first is blind to the inbound direction:
// an order that dug a lane open in order to DROP into it is carrying its bin in
// the gripper, so no bin of its is anywhere in the corridor and every
// claim-keyed predicate reports the visit finished. What says otherwise is
// DeliveryNode, which names this lane.
//
// The stranded walk asked only the first, and then read "committed to the
// fleet" as "has already collected and left" — which is exactly wrong for the
// holder that is driving down this corridor right now. Releasing opens the lane
// in the gap before its own robot arrives: the re-burial window entered from the
// inbound side, which is §R.104's ordinary acceptance shape.
//
// RED before the amendment: the holder is in_transit, holds no claim the walk
// can see, and falls straight through to the release arm.
func TestStrandedHandoff_AnOwnerStillOwingThisLaneADropKeepsIt(t *testing.T) {
	t.Parallel()

	d, db, lane, wallSlots, _, demand := strandedHandoffFixture(t, "STRDB",
		func(o *orders.Order, slots []*nodes.Node) {
			// Picks somewhere, and sets the bin down INSIDE this lane. Right now
			// the bin is in the gripper, which is why nothing claim-keyed can see
			// that the corridor is still owed a drop.
			o.DeliveryNode = slots[2].Name
		})
	testdb.SeedOrderStatus(t, db, demand.ID, string(StatusInTransit), "")

	d.maybeReleaseStrandedHandoff(lane.ID)

	if h := mouthHolders(t, db, lane.ID); len(h) != 1 || h[0].OrderID != demand.ID {
		t.Fatalf("lane %s holds %+v, want demand %d's row KEPT. It still owes this lane a drop at %s — "+
			"its bin is in the gripper, not in the corridor — so no claim-keyed read can see it and "+
			"only DeliveryNode can. Releasing here opens the lane in the gap before its own robot "+
			"drives down it. The stranded walk must ask holderStillOwesTheLane, the same second "+
			"question the dig arm asks.", lane.Name, h, demand.ID, wallSlots[2].Name)
	}
}

// TestStrandedHandoff_AFaultedOwnerReleases is the asymmetry between the two
// askings.
//
// Gate 3 releases a faulted holder, locally, on an argument written out in full
// there: `faulted` is post-dispatch by construction — every inbound edge comes
// from acknowledged, dispatched, in_transit or staged — so a faulted holder that
// got past the claim walk has its bin out of this lane, and a jammed aisle is
// jammed by the ROBOT rather than by a row.
//
// The stranded walk's KEEP predicate was `len(claimed)==0 && !swapLegCommitted`,
// and swapLegCommittedToFleet rules faulted NOT-committed on purpose (its swap
// caller wants a faulted sibling to keep waiting). So the second asking re-keeps
// exactly what the first asking releases — and `faulted` is non-terminal,
// unreaped, and outside the runtime-stuck population, so nothing alarms on the
// corridor it holds.
//
// RED before the amendment: the holder is faulted with no claims, which is the
// pre-dispatch-and-undecided arm's shape, and the walk keeps the row forever.
func TestStrandedHandoff_AFaultedOwnerReleases(t *testing.T) {
	t.Parallel()

	d, db, lane, _, _, demand := strandedHandoffFixture(t, "STRFA", nil)
	testdb.SeedOrderStatus(t, db, demand.ID, string(StatusFaulted), "stranded-handoff pin")

	d.maybeReleaseStrandedHandoff(lane.ID)

	if h := mouthHolders(t, db, lane.ID); len(h) != 0 {
		t.Fatalf("lane %s still holds %+v. Demand %d is FAULTED — post-dispatch by construction, past "+
			"the claim walk, so its bin is out of this corridor — and gate 3 releases exactly this "+
			"holder at conversion time. Keeping it here re-keeps what the first asking released, on a "+
			"status that never terminates and that nothing alarms on.", lane.Name, h, demand.ID)
	}
}

// TestStrandedHandoff_TheFloorReleasesAQuiescedLane is F-22's own rule applied
// to the door that landed outside it.
//
// Every path to maybeReleaseStrandedHandoff is a lane-exit or teardown event,
// and the 60-second floor reached it only inside EvaluateLaneReleases' `if freed`
// arm. The wedge the releaser exists FOR is a corridor whose stranded row
// refuses every inbound comer: no admits, no exits, `freed` false by
// construction. So the fix was silent in precisely the state it was written for
// — the measured wedges (orders 163 and 202, eighteen sim-hours) are quiesced
// lanes, not busy ones.
//
// This drives the floor with nothing else happening on the plant at all: no
// waiter, no event, no traffic. One sweep must find the row and drop it.
//
// RED before the amendment on two counts — the floor's lane set is derived from
// the WAITING population, and SweepLaneWaiters returns before its loop when that
// population is empty.
func TestStrandedHandoff_TheFloorReleasesAQuiescedLane(t *testing.T) {
	t.Parallel()

	d, db, lane, _, _, demand := strandedHandoffFixture(t, "STRFL", nil)
	testdb.SeedOrderStatus(t, db, demand.ID, string(StatusInTransit), "")

	d.SweepLaneWaiters()

	if h := mouthHolders(t, db, lane.ID); len(h) != 0 {
		t.Fatalf("lane %s still holds %+v after a floor sweep. Nothing enters or leaves this corridor — "+
			"the stranded row itself is what refuses every comer — so no exit event will ever fire and "+
			"the event-driven door cannot reach it. The floor is the level-triggered releaser, and it "+
			"must visit a lane carrying a dighandoff row whether or not anybody is queued on it.",
			lane.Name, h)
	}
}
