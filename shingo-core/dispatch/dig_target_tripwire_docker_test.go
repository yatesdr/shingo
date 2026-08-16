//go:build docker

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// dig_target_tripwire_docker_test.go — the hold nobody will ever end.
//
// Arm 2 keyed a service dig's release on a physical fact so that no order's
// death could strand it. One thing still can: nobody coming at all. The demand
// is cancelled, its episode ends, and the dig holds a corridor shut for a
// collection that will never happen.
//
// THE FIXTURE IS A GENUINELY FINISHED DIG, not a bare lock row. The parent has a
// confirmed leg, so it is the state the plant actually produces — excavation
// done, lane still held — rather than a lock with no compound behind it, which
// is a shape the plant cannot make and which would let the sweep pass for the
// wrong reason.

// digHoldingFor builds a service dig that has finished excavating and is holding
// laneID for the bin standing at target: a parent in `reshuffling` with one
// confirmed leg, an origin, and the lane's dig claim.
func digHoldingFor(t *testing.T, db *store.DB, d *Dispatcher, lane, target *nodes.Node,
	uuid, originID string) *orders.Order {
	t.Helper()
	parent := &orders.Order{
		EdgeUUID: uuid, StationID: "line-1", OrderType: OrderTypeMove,
		Status: protocol.StatusReshuffling, Quantity: 1,
		DigTargetNode: target.Name,
		OriginID:      originID, OriginClass: "demand",
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create the dig parent "+uuid)
	leg := &orders.Order{
		EdgeUUID: uuid + "-leg", StationID: "line-1", OrderType: OrderTypeMove,
		Status: protocol.StatusConfirmed, ParentOrderID: &parent.ID, Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(leg), "create the dig's confirmed leg")
	if !d.laneLock.TryLock(lane.ID, parent.ID) {
		t.Fatalf("the dig could not take lane %s", lane.Name)
	}
	return parent
}

// TestReshuffleTargetTripwire_FiresWhenTheEpisodeIsOverAndTheBinIsStillThere.
//
// The dig is holding correctly — the bin it uncovered has not moved — and the
// demand that caused it is cancelled. The hold is keyed on the bin LEAVING, and
// nothing is going to move it, so this is a lane shut forever by a mechanism
// working exactly as designed. That is precisely the case that has to reach a
// person.
//
// IT ASKS THROUGH THE ORIGIN because a dig has no requester pointer: §R.40 ruled
// one out (a dig serves a LANE, so the relation is one-to-many, and a stamp goes
// stale on the very event that matters here — the requester being cancelled).
// The origin names the EPISODE, which does not go stale.
//
// MUTATION (verified): drop the `live > 0` skip in SweepReshufflesHoldingTargets.
// The control dig fires too, and an alarm that goes off for every healthy dig in
// the plant is one people learn to close without reading.
func TestReshuffleTargetTripwire_FiresWhenTheEpisodeIsOverAndTheBinIsStillThere(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dead, alive, _, deadSlots, aliveSlots, bp := setupDwellGroup(t, db, "TWTGT", 2, true)

	// Both lanes hold a bin at the slot their dig uncovered. The ONLY difference
	// between the two digs is whether anything in their episode is still running,
	// which is what makes this a test of the predicate and not of the fixture.
	createTestBinAtNode(t, db, bp.Code, deadSlots[1].ID, "TWTGT-DEAD-BIN")
	createTestBinAtNode(t, db, bp.Code, aliveSlots[1].ID, "TWTGT-ALIVE-BIN")

	abandoned := digHoldingFor(t, db, d, dead, deadSlots[1], "twtgt-abandoned", "11111111-1111-1111-1111-111111111111")
	served := digHoldingFor(t, db, d, alive, aliveSlots[1], "twtgt-served", "22222222-2222-2222-2222-222222222222")

	// The abandoned dig's demand died. The served dig's is still queued, waiting
	// to be re-driven onto the slot the excavation just opened — the ordinary case
	// and by far the common one.
	mkDemand := func(uuid, originID string, status protocol.Status) {
		o := &orders.Order{EdgeUUID: uuid, StationID: "line-1", OrderType: OrderTypeRetrieve,
			Status: status, Quantity: 1, PayloadCode: bp.Code, OriginID: originID, OriginClass: "demand"}
		testutil.MustNoErr(t, db.CreateOrder(o), "create demand "+uuid)
	}
	mkDemand("twtgt-dead-demand", "11111111-1111-1111-1111-111111111111", protocol.StatusCancelled)
	mkDemand("twtgt-live-demand", "22222222-2222-2222-2222-222222222222", protocol.StatusQueued)

	if n := d.SweepReshufflesHoldingTargets(); n != 1 {
		t.Fatalf("the sweep recorded %d finding(s), want exactly 1 — the abandoned dig (%d) and not "+
			"the served one (%d)", n, abandoned.ID, served.ID)
	}

	actions, err := db.ListRecoveryActions(50)
	testutil.MustNoErr(t, err, "list recovery actions")
	var found *string
	for _, a := range actions {
		if a.Action != UnfetchedTargetAction {
			continue
		}
		if a.TargetID != abandoned.ID {
			t.Errorf("a %s row was filed against order %d, want the abandoned dig %d. The served dig "+
				"is holding for a demand that is still queued — alarming on it teaches people to "+
				"ignore this row", UnfetchedTargetAction, a.TargetID, abandoned.ID)
		}
		detail := a.Detail
		found = &detail
	}
	if found == nil {
		t.Fatalf("no %s row was written. The count said 1, so the sweep decided and then did not "+
			"record it — a finding nobody can read is not a finding", UnfetchedTargetAction)
	}
	// The person ruling this needs three things from the row: what is shut, what
	// is standing in it, and which episode died owing it.
	for _, want := range []string{dead.Name, deadSlots[1].Name, "11111111-1111-1111-1111-111111111111"} {
		if !strings.Contains(*found, want) {
			t.Errorf("the recovery detail does not name %q. It reads:\n%s", want, *found)
		}
	}

	// AND NOTHING WAS RELEASED, which is the ruling this instrument carries. The
	// obvious fix — drop the lane, nobody wants the bin — is fail-open, and
	// fail-open is the behaviour arm 2 replaced.
	if !d.laneLock.IsLocked(dead.ID) {
		t.Error("the sweep RELEASED the abandoned dig's lane. It is an alarm: releasing on this " +
			"signal re-exposes the uncovered bin, which is the whole thing arm 2 closed, and a human " +
			"rules the incident through the Core-side hard release")
	}
}

// TestReshuffleTargetTripwire_IsQuietWhenTheBinHasGone is law 9's half: the
// normal plant, where digs hold briefly and their bins get collected.
//
// The episode is over here too — so the ONLY thing keeping this quiet is that
// the bin left. A sweep that alarmed on "the episode ended" alone would fire on
// every completed dig in the plant.
func TestReshuffleTargetTripwire_IsQuietWhenTheBinHasGone(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, _, dugSlots, _, _ := setupDwellGroup(t, db, "TWQUIET", 2, true)

	// No bin at the target slot: the excavation happened and the bin was taken.
	digHoldingFor(t, db, d, dug, dugSlots[1], "twquiet-dig", "33333333-3333-3333-3333-333333333333")

	if n := d.SweepReshufflesHoldingTargets(); n != 0 {
		t.Errorf("the sweep recorded %d finding(s) against a dig whose bin has already left. It owes "+
			"nothing, and an instrument that speaks on the healthy case is one nobody reads", n)
	}
}
