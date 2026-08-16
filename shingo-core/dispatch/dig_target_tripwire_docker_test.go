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
	"shingocore/store/reservations"
)

// dig_target_tripwire_docker_test.go — who gets the corridor when the digging
// stops, and what is said when the answer is nobody.
//
// An excavation ends with a bin standing at an open lane mouth and the demand it
// was dug for not yet dispatched. The slots in front of that bin are the cheapest
// shuffle candidates in the group, so the corridor cannot simply open; and it
// cannot be held by the dig either, because a dig holding a lane for a bin is a
// finished order that never terminates. It is HANDED to the live demand in the
// episode the dig was raised for, as that demand's own outbound hold.
//
// THE FIXTURE IS A GENUINELY FINISHED DIG, not a bare lock row. The parent has a
// confirmed leg, so it is the state the plant actually produces — excavation
// done, lane still claimed — rather than a lock with no compound behind it, which
// is a shape the plant cannot make and which would let the handoff pass for the
// wrong reason.

// digHoldingFor builds a service dig that has finished excavating and still holds
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

// TestDigHandoff_GoesToTheLiveEpisodeAndNobodyElse is the mechanism and the
// defect it was built for, side by side in one group.
//
// TWO FINISHED DIGS, IDENTICAL IN EVERY RESPECT the machinery can see: both in
// `reshuffling`, both sealed, both with a bin standing at the slot they dug out.
// The only difference is whether anything in their episode is still live.
//
// THAT IS THE ONLY QUESTION AVAILABLE, and the fixture is built to say so. A
// buried demand records nothing about the bin it is waiting for — no claim, no
// reservation, no source_node pointing at the slot; it cannot claim what it
// cannot reach and it does not re-plan until the lane opens. Verified against all
// five digs running on the lane-stress rig 2026-08-13. So the collector is found
// through the ORIGIN, which is the tie a service dig does have: it inherits the
// origin of the demand that could not move.
//
// THE ABANDONED SIDE IS THE DEFECT THIS REPLACES. Its episode is over, so under
// the old hold — keep the lane until the bin is collected — that corridor stayed
// shut with no releaser in the world, because nothing was coming to collect it.
//
// MUTATION: drop the empty-dig_target_node clause from CollectorForDigTarget.
// The abandoned dig then hands its corridor to the OTHER DIG in its episode — a
// peer excavation, which will never collect anything — and the lane is shut by an
// order that has no reason to open it.
func TestDigHandoff_GoesToTheLiveEpisodeAndNobodyElse(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, wanted, movedOn, _, wantedSlots, movedOnSlots, bp := setupDwellGroup(t, db, "TWTGT", 2, true)

	// Both lanes hold a bin at the slot their dig uncovered.
	createTestBinAtNode(t, db, bp.Code, wantedSlots[1].ID, "TWTGT-WANTED-BIN")
	createTestBinAtNode(t, db, bp.Code, movedOnSlots[1].ID, "TWTGT-STRANDED-BIN")

	collected := digHoldingFor(t, db, d, wanted, wantedSlots[1], "twtgt-collected",
		"11111111-1111-1111-1111-111111111111")
	abandoned := digHoldingFor(t, db, d, movedOn, movedOnSlots[1], "twtgt-abandoned",
		"22222222-2222-2222-2222-222222222222")

	// One episode still has its demand waiting — `queued`, unresolved, holding
	// nothing, which is exactly the state a buried demand sits in while its dig
	// runs. The other's demand is cancelled, so nothing in that episode is coming.
	mkDemand := func(uuid, originID string, status protocol.Status) *orders.Order {
		o := &orders.Order{EdgeUUID: uuid, StationID: "line-1", OrderType: OrderTypeRetrieve,
			Status: status, Quantity: 1, PayloadCode: bp.Code,
			OriginID: originID, OriginClass: "demand"}
		testutil.MustNoErr(t, db.CreateOrder(o), "create demand "+uuid)
		return o
	}
	collector := mkDemand("twtgt-collector", "11111111-1111-1111-1111-111111111111", protocol.StatusQueued)
	mkDemand("twtgt-gone", "22222222-2222-2222-2222-222222222222", protocol.StatusCancelled)

	// AND A PEER DIG IN THE ABANDONED EPISODE, still running. One demand routinely
	// raises several excavations — two of the five on the rig shared one episode —
	// and a dig is not a collector: it is never going to take the bin. Without it
	// the abandoned case could pass by finding nobody for the wrong reason.
	testutil.MustNoErr(t, db.CreateOrder(&orders.Order{
		EdgeUUID: "twtgt-peer-dig", StationID: "line-1", OrderType: OrderTypeMove,
		Status: protocol.StatusReshuffling, Quantity: 1, DigTargetNode: movedOnSlots[0].Name,
		OriginID: "22222222-2222-2222-2222-222222222222", OriginClass: "demand",
	}), "create the peer dig in the abandoned episode")

	// ── THE WANTED SLOT: the corridor changes hands ──────────────────────────
	if handed := d.handOffDugLane(collected, wanted.ID); !handed {
		t.Fatalf("dig %d did not hand lane %s to order %d, the live demand in the episode it was dug "+
			"for. Releasing instead leaves the bin it uncovered exposed to the next shuffle search",
			collected.ID, wanted.Name, collector.ID)
	}
	holders, err := reservations.ActiveMouthRows(db.DB, wanted.ID)
	testutil.MustNoErr(t, err, "read the handed-over lane's holders")
	if len(holders) != 1 || holders[0].OrderID != collector.ID || holders[0].Mode != reservations.ModeOutbound {
		t.Fatalf("lane %s holds %+v, want one OUTBOUND row for the collector %d. Outbound excludes a "+
			"drop into the lane, which is the only way the uncovered bin can be re-buried, and is "+
			"idempotent against the collector's own dispatch", wanted.Name, holders, collector.ID)
	}

	// ── THE ABANDONED SLOT: nobody is coming, so the corridor opens ──────────
	if handed := d.handOffDugLane(abandoned, movedOn.ID); handed {
		t.Fatalf("dig %d handed lane %s to somebody, with its whole episode terminal. A corridor held "+
			"for an episode that is over has no releaser: nothing is coming to take the bin at %s, so "+
			"nothing will ever end the hold", abandoned.ID, movedOn.Name, movedOnSlots[1].Name)
	}

	// AND IT IS COUNTED. Not a wedge — the lane opens and the bin is an ordinary
	// reachable bin — but robot-minutes were spent excavating towards a bin nobody
	// took, and without this row that spend leaves no trace at all: the dig
	// confirms, its legs confirm, and every status reads healthy.
	actions, err := db.ListRecoveryActions(50)
	testutil.MustNoErr(t, err, "list recovery actions")
	var found *string
	for _, a := range actions {
		if a.Action != AbandonedExcavationAction {
			continue
		}
		if a.TargetID != abandoned.ID {
			t.Errorf("a %s row was filed against order %d, want the abandoned dig %d. The other dig's "+
				"episode is still live — alarming on it teaches people to ignore this row",
				AbandonedExcavationAction, a.TargetID, abandoned.ID)
		}
		detail := a.Detail
		found = &detail
	}
	if found == nil {
		t.Fatalf("no %s row was written for dig %d, which excavated %s for an episode that had ended",
			AbandonedExcavationAction, abandoned.ID, movedOn.Name)
	}
	// The person reading this needs three things: which corridor was worked, what
	// is standing in it, and which episode paid for the digging.
	for _, want := range []string{movedOn.Name, movedOnSlots[1].Name, "22222222-2222-2222-2222-222222222222"} {
		if !strings.Contains(*found, want) {
			t.Errorf("the recovery detail does not name %q. It reads:\n%s", want, *found)
		}
	}
}

// TestDigHandoff_UnattributedDigIsNotFiledAsAbandoned is the split: a dig with no
// episode files its OWN row and must not be counted as a wasted excavation.
//
// CollectorForDigTarget returns (nil, nil) for two different facts. One is "I
// asked the episode and every order in it is terminal" — nobody is coming, the
// digging was waste, and that is the finding the abandoned row exists for. The
// other is "there is no origin, so I did not ask" — which says nothing whatever
// about whether anybody is coming, and the bin may well be collected.
//
// Filed under one name, the second drowned the first. On run 5 all four alarms
// were the second kind: a 100% fire rate on NULL-origin digs, and a row that
// asserted "every other order in episode  has reached a terminal status" — with
// an empty episode id — about orders it had never looked at. An instrument
// reporting absence of input as presence of a finding, which is the exact shape
// this house has a rule against.
//
// THE ASSERTION THAT MATTERS IS THE NEGATIVE ONE. Writing the new row is easy;
// what the split is for is that a reader counting abandoned excavations no
// longer counts these.
//
// MUTATION (verified): make recordExcavationWithNobodyComing file
// AbandonedExcavationAction unconditionally — i.e. un-split it — and the
// negative assertion fires with the run-5 population back in the bucket.
func TestDigHandoff_UnattributedDigIsNotFiledAsAbandoned(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, orphaned, _, _, orphanedSlots, _, bp := setupDwellGroup(t, db, "TWORPH", 2, true)

	// A bin IS standing at the uncovered slot, so the handoff gets past
	// DigStillOwesItsTarget and reaches the question this test is about. Without
	// it the dig would be silent for the unrelated reason the test above pins.
	createTestBinAtNode(t, db, bp.Code, orphanedSlots[1].ID, "TWORPH-EXPOSED-BIN")

	// The dig carries NO ORIGIN — origin_class 'orphan', which is what Core
	// stamps on an order that reached it naming no episode.
	parent := &orders.Order{
		EdgeUUID: "tworph-dig", StationID: "line-1", OrderType: OrderTypeMove,
		Status: protocol.StatusReshuffling, Quantity: 1,
		DigTargetNode: orphanedSlots[1].Name,
		OriginID:      "", OriginClass: "orphan",
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create the unattributed dig")
	testutil.MustNoErr(t, db.CreateOrder(&orders.Order{
		EdgeUUID: "tworph-dig-leg", StationID: "line-1", OrderType: OrderTypeMove,
		Status: protocol.StatusConfirmed, ParentOrderID: &parent.ID, Quantity: 1,
	}), "create the dig's confirmed leg")
	if !d.laneLock.TryLock(orphaned.ID, parent.ID) {
		t.Fatalf("the dig could not take lane %s", orphaned.Name)
	}

	if handed := d.handOffDugLane(parent, orphaned.ID); handed {
		t.Fatalf("dig %d handed lane %s to somebody. With no origin there is no episode to look a "+
			"collector up in, so the lane must be released — a scan early, which is the recoverable "+
			"direction", parent.ID, orphaned.Name)
	}

	actions, err := db.ListRecoveryActions(50)
	testutil.MustNoErr(t, err, "list recovery actions")
	var unattributed *string
	for _, a := range actions {
		if a.TargetID != parent.ID {
			continue
		}
		// THE NEGATIVE ASSERTION — the whole point of the split.
		if a.Action == AbandonedExcavationAction {
			t.Errorf("dig %d was filed as %s, but nothing was ever asked about it: it has no origin, so "+
				"CollectorForDigTarget declined to guess and returned nil without running a query. "+
				"Counting it as a wasted excavation reports the absence of an input as a finding, and "+
				"it is what made all four run-5 alarms false.", parent.ID, AbandonedExcavationAction)
		}
		if a.Action == UnattributedExcavationAction {
			detail := a.Detail
			unattributed = &detail
		}
	}
	if unattributed == nil {
		t.Fatalf("no %s row was written for dig %d. The split must not go the other way either — a dig "+
			"that cleared a lane and could not be attributed is still worth a row, because the missing "+
			"origin is the defect it is evidence of", UnattributedExcavationAction, parent.ID)
	}
	// It still names the corridor and what is standing in it — a reader needs
	// those whichever of the two rows they are looking at.
	for _, want := range []string{orphaned.Name, orphanedSlots[1].Name} {
		if !strings.Contains(*unattributed, want) {
			t.Errorf("the unattributed detail does not name %q. It reads:\n%s", want, *unattributed)
		}
	}
	// And it must NOT claim the episode ended, which is the sentence that was
	// false every time the old row was written for this case.
	if strings.Contains(*unattributed, "has reached a terminal status") {
		t.Errorf("the unattributed detail claims the episode is over. It never looked at an episode; "+
			"there is no origin on this order. It reads:\n%s", *unattributed)
	}
}

// TestDigHandoff_IsSilentWhenTheDigUncoveredNothing is law 9's half: the ordinary
// excavation, which hands over nothing and says nothing.
//
// A dig whose target slot is empty uncovered no bin — either it was clearing a
// slot for somebody to drop into, or the bin has already gone. There is nothing
// to protect and nothing to report, and an instrument that speaks here is one
// nobody reads.
func TestDigHandoff_IsSilentWhenTheDigUncoveredNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, dug, _, _, dugSlots, _, _ := setupDwellGroup(t, db, "TWQUIET", 2, true)

	// No bin at the target slot: the excavation happened and the bin was taken.
	parent := digHoldingFor(t, db, d, dug, dugSlots[1], "twquiet-dig", "33333333-3333-3333-3333-333333333333")

	if handed := d.handOffDugLane(parent, dug.ID); handed {
		t.Errorf("the dig handed lane %s to a collector with no bin standing at %s. There is nothing "+
			"in that corridor to protect", dug.Name, dugSlots[1].Name)
	}
	actions, err := db.ListRecoveryActions(50)
	testutil.MustNoErr(t, err, "list recovery actions")
	for _, a := range actions {
		if a.Action == AbandonedExcavationAction {
			t.Errorf("a %s row was written for a dig that uncovered nothing. Nothing was excavated "+
				"for nothing here, and an alarm on the healthy case is one people learn to close "+
				"without reading", AbandonedExcavationAction)
		}
	}
}
