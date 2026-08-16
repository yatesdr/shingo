//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// acceptance_dig_docker_test.go — §R.104a's ordering, as a law with a test.
//
//	lock → dig chapter (if needed) → append
//
// The ruling puts it plainly: an append that precedes its lock is a defect, and
// summon-before-lock would readmit mid-excavation traffic. The middle term needs
// a guard of its own — nothing may append a tail to a robot whose OWN diggers are
// still working the lane it is about to drive into.

// stageDwellerBehindAWall builds the proven F-11 shape: a deeper store holds the
// mouth so a shallower one dwells at the mark (Tier 2), an unclaimed wall arrives,
// and then the deeper store places so the ONLY reason left to hold the dweller is
// the physical wall. Copied from the window-3 fixture rather than re-derived,
// because it is the one arrangement known to produce a genuinely walled dweller.
func stageDwellerBehindAWall(t *testing.T, db *store.DB, d *Dispatcher, tag string) (
	wall *nodes.Node, dweller *orders.Order, blocker *bins.Bin) {
	t.Helper()
	wallLane, _, w, _, bp := clearLaneFixture(t, db, tag)
	line := lineNode(t, db, tag+"-LINE")

	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = w[2].Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(deep, line, w[2], EntryFreshBin); err != nil || !adm {
		t.Fatalf("the deeper store must take its mouth row: adm=%v err=%v", adm, err)
	}
	testutil.MustNoErr(t, db.UpdateOrderVendor(deep.ID, tag+"-deep", "RUNNING", ""), "deep vendor")

	dweller = stageGatedStore(t, db, d, line, w[1], nil)
	if !IsGateStaged(dweller) {
		t.Fatalf("the dweller must be gate-staged (wait_index=%d)", dweller.WaitIndex)
	}
	markStaged(t, db, dweller.ID)

	blocker = createTestBinAtNode(t, db, bp.Code, w[0].ID, "BIN-"+tag+"-WALL")
	if blocker.ClaimedBy != nil {
		t.Fatal("fixture bug: the walling bin must be UNCLAIMED")
	}
	d.ReleaseInboundLaneForOrder(deep.ID, w[2].Name)
	return wallLane, dweller, blocker
}

// TestAcceptance_NoAppendWhileItsOwnChapterIsOpen is the middle term.
//
// The trap is specific and it is not hypothetical: the wall a dweller is waiting
// on can disappear for a reason that has nothing to do with its dig — another
// order carries the bin out, an operator moves it — and the evaluator's next pass
// then finds a candidate whose classifier says CLEAR. Appending there sends the
// robot into a corridor its own dig legs are still working, nose to tail, which
// is the collision the lane lock exists to prevent, arrived at from inside.
//
// MUTATION (verified): drop the hasOpenDigChapter skip at the top of the
// candidate loop. The dweller is appended while its own leg is still pending and
// the "no tail" assertion fires.
func TestAcceptance_NoAppendWhileItsOwnChapterIsOpen(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	wall, dweller, blocker := stageDwellerBehindAWall(t, db, d, "ACC-ORDER")

	// The oracle asks, answers BURIED, and the dweller summons its own dig.
	d.EvaluateLaneReleases(wall.ID)
	if !d.hasOpenDigChapter(dweller.ID) {
		t.Fatal("the dweller did not summon a dig — this test is about what happens while one runs")
	}
	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller got %d tail append(s) before its dig even started", n)
	}

	// ── AND NOW THE WALL GOES AWAY BY ITSELF, mid-excavation. Somebody else
	// carried it out; an operator moved it. The classifier will now say CLEAR
	// about a lane this dweller's own legs are still working.
	testutil.MustNoErr(t, db.MoveBinToTransit(blocker.ID, transitNode(t, db, "ACC-ORDER-TRANSIT").ID),
		"somebody else takes the wall out")

	d.EvaluateLaneReleases(wall.ID)

	if n := appendsTo(backend, dweller.VendorOrderID); n != 0 {
		t.Fatalf("the dweller got %d tail append(s) while its OWN dig legs were still in the lane. "+
			"lock -> dig chapter -> append: the middle term is not optional because the wall "+
			"happened to clear early, and a robot sent in now drives up behind its own diggers", n)
	}
	fresh, err := db.GetOrder(dweller.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	if !IsGateStaged(fresh) {
		t.Fatalf("the dweller left the mark as %s while its chapter was open", fresh.Status)
	}
}

// And the same guard read from the other side: a dweller with an open chapter is
// not a candidate for ANYTHING the pass does — including proposing a second
// excavation of the lane it is already excavating. That is the audit's item 10,
// and it is what keeps the acceptance arm from stacking digs on one lane.
func TestAcceptance_ADiggingDwellerProposesNoSecondDig(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	wall, dweller, _ := stageDwellerBehindAWall(t, db, d, "ACC-ONCE")

	d.EvaluateLaneReleases(wall.ID)
	before := childCount(t, db, dweller.ID)
	if before == 0 {
		t.Fatal("the dweller did not summon a dig")
	}

	// Every later pass re-asks. None of them may raise a second chapter.
	for i := 0; i < 3; i++ {
		d.EvaluateLaneReleases(wall.ID)
	}
	if after := childCount(t, db, dweller.ID); after != before {
		t.Fatalf("the dweller's chapter grew from %d to %d legs across re-asks — one excavation per "+
			"lane per dweller, and a digging dweller is not a candidate for another", before, after)
	}
}

func childCount(t *testing.T, db *store.DB, parentID int64) int {
	t.Helper()
	kids, err := db.ListChildOrders(parentID)
	testutil.MustNoErr(t, err, "list children")
	return len(kids)
}
