//go:build docker

package dispatch

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// complex_no_shuffle_slot_docker_test.go — the SPLIT at the complex reshuffle
// planners' error arm: which planning failures may kill a complex parent.
//
// ErrNoShuffleSlot means the dig has nowhere to park its blockers RIGHT NOW. A
// shuffle slot frees the moment any other order clears one, so it is congestion
// and congestion waits. The SIMPLE retrieve path has said so since sim order 21
// died of it on 2026-07-10 — planning_service.go's codeNoShuffleSlot arm, pinned
// by engine's TestBuriedRetrieve_NoShuffleSlot_WaitsThenReshuffles.
//
// The COMPLEX path holds two copies of that call site —
// planBuriedReshuffleAtIntake and handleComplexBuriedOnReplay — and neither had
// the arm until 5be3910d. Both went straight to
// failOrderInternal("reshuffle_error", "cannot plan reshuffle: %v"), so one
// crowded lane queued a plain retrieve and KILLED a complex parent: the more
// expensive one to drop, since a two-robot swap hangs off it.
//
// Twice on the lane-stress rig, 2026-08-09, and reproduced from a unit fixture at
// both sites here before the fix landed:
//
//	complex|failed|SYN_STAMP|intake-buried|cannot plan reshuffle: find shuffle
//	   slots: no free shuffle slot: need 2 shuffle slots but only 1 available
//
// THE SHAPE IS WHY THIS FILE IS FOUR TESTS AND NOT ONE. It was never a missing
// concept — the concept was already written down, argued, and dated in
// planning_service.go. It was a correct fix applied at one of three call sites
// and never carried to the other two, and nothing in the gate could notice
// because each site reads reasonably on its own. The only thing that catches
// that class is a test AT each site, which is also why the two near-twins below
// are written out rather than folded into a table: they have now drifted twice.
//
// TWO PROPERTIES, and the second is the one that is easy to skip.
//
// The WAIT, at both sites, each with its liveness half — a wait nothing retries
// is a wedge with better logging, and the parked row alone cannot tell you which
// one you have.
//
// And the NEGATIVE: everything that is not ErrNoShuffleSlot stays terminal. The
// cheapest wrong way to have fixed this was to widen the arm to `if err != nil`,
// which reads as more cautious and is not. A target slot with no parent lane is
// geometry: no slot freeing anywhere will ever change it, so a parent parked on
// it waits forever while its station shows "storage rearranging" and nobody is
// told anything is wrong. That fault is driven through the SAME error the wait
// arm now inspects, one errors.Is call away, rather than through some other
// refusal — otherwise these would not be about the split at all.

// complexBuriedFixture builds a group whose only shuffle slot is occupied, a
// buried target behind one blocker, and a complex parent in the state each site's
// own caller hands it: `queued`.
//
// The fixture is the simple-path test's, deliberately: two lane slots and exactly
// one shuffle slot with a squatter in it. Identical congestion, so the only
// variable between the two paths is which planner is asking. squatter is returned
// for the liveness half of the wait tests (see the file header).
func complexBuriedFixture(t *testing.T, db *store.DB, prefix string) (
	sc *testdb.CompoundScenario, parent *orders.Order, buried *BuriedError, squatter *bins.Bin,
) {
	t.Helper()
	sc = testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix:      prefix,
		NumSlots:    2,
		NumShuffles: 1,
		TargetSlot:  2,
		TargetAge:   2 * time.Hour,
	})

	// The one shuffle slot is taken, so a dig here has nowhere to put the blocker.
	squatter = &bins.Bin{
		BinTypeID: sc.BinType.ID, Label: prefix + "-SQUAT",
		NodeID: &sc.ShuffleSlots[0].ID, Status: "available",
	}
	testutil.MustNoErr(t, db.CreateBin(squatter), "occupy the only shuffle slot")

	parent = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = prefix + "-complex"
		o.OrderType = OrderTypeComplex
		o.Status = StatusQueued
		o.PayloadCode = sc.Payload.Code
		o.DeliveryNode = sc.LineNode.Name
	})
	buried = &BuriedError{Bin: sc.TargetBin, Slot: sc.Slots[1], LaneID: sc.Lane.ID}
	return sc, parent, buried, squatter
}

// assertParkedOnShuffleSlot is the disposition half. Shared between the two
// sites because it IS the property under test, and stating it twice is how the
// two would come to disagree about it again without anyone noticing.
func assertParkedOnShuffleSlot(t *testing.T, db *store.DB, d *Dispatcher, parent *orders.Order, laneID int64) {
	t.Helper()
	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "read the parent back")

	if protocol.IsTerminal(after.Status) {
		t.Fatalf("the complex parent was TERMINALIZED (%q, %q) because no shuffle slot was free.\n"+
			"That is congestion, not a broken lane — a slot frees as soon as any other order releases "+
			"one, and the SIMPLE retrieve path queues for exactly this and has since sim order 21. A "+
			"complex parent is the more expensive one to drop: it is what a two-robot swap hangs off.",
			after.Status, after.ErrorDetail)
	}
	if after.Status != StatusQueued {
		t.Fatalf("status = %q, want %q — the scanner replays the acquiring set, so a parent parked "+
			"anywhere else is a parent nothing comes back for", after.Status, StatusQueued)
	}
	if after.QueueCode != string(protocol.QueueStorageRearranging) {
		t.Errorf("queue_code = %q, want %q", after.QueueCode, protocol.QueueStorageRearranging)
	}
	if after.QueueCause != string(CauseNoShuffleSlot) {
		t.Errorf("queue_cause = %q, want %q — the reshuffle waits clear on different signals and "+
			"must not share one tag; this one ends when any order frees a shuffle slot",
			after.QueueCause, CauseNoShuffleSlot)
	}
	if strings.TrimSpace(after.QueueReason) == "" {
		t.Error("queue_reason is blank — the operator surface has a parked complex order and nothing " +
			"to render for it")
	}

	// NOTHING STAYS HELD. A park that keeps the lane lock stops every other dig in
	// that lane for as long as it waits — and this wait ends on a slot freeing
	// somewhere else entirely, which the lane has no part in.
	if d.laneLock.IsLocked(laneID) {
		t.Error("the lane is still locked after a park that never planned anything")
	}
	kids, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list children after the park")
	if len(kids) != 0 {
		t.Errorf("the parked parent has %d child order(s) under it — the plan was refused, so a leg "+
			"here is one the retry would find orphaned", len(kids))
	}
}

// assertDugAfterSlotFreed is the liveness half.
func assertDugAfterSlotFreed(t *testing.T, db *store.DB, parent *orders.Order) {
	t.Helper()
	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "read the parent back after the retry")

	// THE LIVENESS CLAIM IS UNCHANGED; WHERE THE LEGS LIVE IS NOT. This asserted
	// that the demand itself had become a reshuffle with legs under it. Under the
	// two-shape ruling the demand never moves — the excavation is a separate
	// service dig — so the same claim ("the park had a real releaser, and pulling
	// it produced an actual excavation") is now read off the DIG.
	//
	// The demand-side half is kept and inverted, because it is the half that would
	// catch the regression: a demand that acquired legs again has been re-parented.
	kids := serviceDigChildren(t, db, after)
	if len(kids) == 0 {
		t.Fatalf("a shuffle slot freed and the retry still planned nothing — status %q, "+
			"queue_reason %q. The park has no releaser, which makes it a stall wearing a queue reason",
			after.Status, after.QueueReason)
	}
	if after.Status != StatusQueued {
		t.Errorf("demand status = %q with a dig running for it, want %q — the demand waits in the "+
			"acquiring set and is re-driven by the scanner", after.Status, StatusQueued)
	}
	demandKids, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list the demand's own children")
	if len(demandKids) != 0 {
		t.Errorf("the demand owns %d legs — it was re-parented into the dig", len(demandKids))
	}
}

// TestComplexIntake_NoShuffleSlot_WaitsThenReshuffles is the intake site.
//
// MUTATION (verified): delete the `errors.Is(err, ErrNoShuffleSlot)` arm from
// planBuriedReshuffleAtIntake so the shortfall falls into failOrderInternal. The
// parent comes back `failed` carrying "cannot plan reshuffle: find shuffle slots:
// no free shuffle slot: need 1 shuffle slots but only 0 available" — the rig's
// row, reproduced — and the first assertion fires. The replay test stays green,
// which is what makes this a per-site pin rather than one test covering two
// copies of the same mistake.
func TestComplexIntake_NoShuffleSlot_WaitsThenReshuffles(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc, parent, buried, squatter := complexBuriedFixture(t, db, "CXNOSHUF")
	d, emitter := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	d.planBuriedReshuffleAtIntake(parent, sc.Payload.Code, "line-1", buried)

	assertParkedOnShuffleSlot(t, db, d, parent, sc.Lane.ID)

	// THE STATION IS TOLD, which is this site's difference from its twin and not a
	// detail: intake is answering a request that just arrived, so an operator is
	// looking at the station now. Its own neighbouring arms (targets-occupied,
	// lane-locked, lane-lock-race, blocker-claimed) all announce the park, and a
	// silent fifth would be the one wait the surface cannot show.
	if len(emitter.queued) != 1 {
		t.Errorf("queued events = %d, want 1 — the intake site announces its parks", len(emitter.queued))
	}
	if len(emitter.failed) != 0 {
		t.Errorf("failed events = %d, want 0 — a failure event against a parked order raises an "+
			"alarm for congestion", len(emitter.failed))
	}

	// A shuffle slot frees: some other order cleared it. Nothing is reset — the
	// same parent, the same burial, the same call.
	mustExecDispatch(t, db, `DELETE FROM bins WHERE id=$1`, squatter.ID)
	reloaded, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the parked parent")
	d.planBuriedReshuffleAtIntake(reloaded, sc.Payload.Code, "line-1", buried)

	assertDugAfterSlotFreed(t, db, parent)
}

// TestComplexReplay_NoShuffleSlot_WaitsThenReshuffles is the scanner-path twin.
//
// It is entered with a parent that already exists and is already acquiring, so
// leaving it queued with a cause IS the retry — the releaser is the scan that
// brought us here. That makes the liveness half a faithful re-drive rather than a
// contrivance: it is the same call the scanner makes on its next pass.
//
// MUTATION (verified): delete the `errors.Is(err, ErrNoShuffleSlot)` arm from
// handleComplexBuriedOnReplay. This test fails on the first assertion with the
// parent `failed`; the intake test stays green.
func TestComplexReplay_NoShuffleSlot_WaitsThenReshuffles(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc, parent, buried, squatter := complexBuriedFixture(t, db, "CXRPNOSHUF")
	d, emitter := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	d.handleComplexBuriedOnReplay(parent, buried)

	assertParkedOnShuffleSlot(t, db, d, parent, sc.Lane.ID)

	// AND THIS SITE IS SILENT, matching ITS neighbours. Every park in
	// handleComplexBuriedOnReplay writes the row and emits nothing: the parent was
	// already queued and already announced, and re-announcing it on every scan
	// would put one event per tick on the station for an order that has not moved.
	if len(emitter.queued) != 0 {
		t.Errorf("queued events = %d, want 0 — the replay site parks silently, so a scan loop over a "+
			"crowded group does not become an event storm", len(emitter.queued))
	}
	if len(emitter.failed) != 0 {
		t.Errorf("failed events = %d, want 0", len(emitter.failed))
	}

	mustExecDispatch(t, db, `DELETE FROM bins WHERE id=$1`, squatter.ID)
	reloaded, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "reload the parked parent")
	d.handleComplexBuriedOnReplay(reloaded, buried)

	assertDugAfterSlotFreed(t, db, parent)
}

// TestComplexIntake_PlannerFaultStillFails is the intake site's terminal arm.
//
// MUTATION (verified): in planBuriedReshuffleAtIntake, drop the errors.Is so the
// congestion arm reads `if err != nil` and swallows every planning failure. The
// parent comes back `queued` for a fault nothing will ever clear, and the status
// assertion fires. The two wait tests stay green — this catches the
// OVER-application, which is the opposite failure from the one 5be3910d fixed
// and the one a later hand is most likely to introduce while "being safe".
//
// Applied at this site alone, the replay fault test stays green too.
func TestComplexIntake_PlannerFaultStillFails(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc, parent, buried, _ := complexBuriedFixture(t, db, "CXFAULT")
	d, emitter := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A target slot detached from its lane: planUnbury refuses on geometry before
	// it ever looks for a shuffle slot, with a bare error that is not
	// ErrNoShuffleSlot.
	buried.Slot = detachedSlot(t, db, sc.Slots[1].ID)

	d.planBuriedReshuffleAtIntake(parent, sc.Payload.Code, "line-1", buried)

	assertPlannerFaultTerminal(t, db, parent)
	if len(emitter.failed) != 1 {
		t.Errorf("failed events = %d, want 1 — a fault the station is never told about is a fault "+
			"nobody goes and fixes", len(emitter.failed))
	}
}

// TestComplexReplay_PlannerFaultStillFails is the scanner-path twin.
//
// A separate test rather than a table row over the two: these are near-twins that
// have already drifted once — the missing wait arm is the second disposition they
// disagree on — and a shared body would let the next divergence hide inside a
// loop.
//
// MUTATION (verified): the same widening in handleComplexBuriedOnReplay, and it
// fires here on the status assertion. Applied at BOTH sites at once, both fault
// tests fire and both wait tests stay green — four tests, four separate facts.
func TestComplexReplay_PlannerFaultStillFails(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc, parent, buried, _ := complexBuriedFixture(t, db, "CXRPFAULT")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	buried.Slot = detachedSlot(t, db, sc.Slots[1].ID)

	d.handleComplexBuriedOnReplay(parent, buried)

	assertPlannerFaultTerminal(t, db, parent)
}

// detachedSlot returns the slot as the planner will see it with no parent lane.
// The in-memory struct is what matters and the row is left alone: planUnbury
// reads targetSlot.ParentID off the value the BuriedError carries, which is how a
// slot whose lane was reconfigured mid-flight reaches it.
func detachedSlot(t *testing.T, db *store.DB, slotID int64) *nodes.Node {
	t.Helper()
	s, err := db.GetNode(slotID)
	testutil.MustNoErr(t, err, "load the slot")
	s.ParentID = nil
	return s
}

// mustExecDispatch runs a raw statement for fixture manipulation the store API
// deliberately has no verb for — here, some other order clearing a shuffle slot.
func mustExecDispatch(t *testing.T, db *store.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func assertPlannerFaultTerminal(t *testing.T, db *store.DB, parent *orders.Order) {
	t.Helper()
	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "read the parent back")
	if after.Status != protocol.StatusFailed {
		t.Fatalf("parent status = %q, want %q — this is geometry, not congestion. No shuffle slot "+
			"freeing anywhere will give this target slot a parent lane, so a park here waits forever "+
			"under a queue reason that says storage is being rearranged", after.Status, protocol.StatusFailed)
	}
	if !strings.Contains(after.ErrorDetail, "cannot plan reshuffle") {
		t.Errorf("error_detail = %q, want the planner's own account of what it could not do",
			after.ErrorDetail)
	}
	// The squatter is still sitting in the only shuffle slot, so this fixture could
	// reach the congestion arm as well. It must not: a fault test that is really
	// exercising the wait proves nothing about the split.
	if strings.Contains(after.ErrorDetail, "no free shuffle slot") {
		t.Errorf("error_detail = %q — the geometry refusal has to come first, or this test is "+
			"asserting the wrong arm", after.ErrorDetail)
	}
}
