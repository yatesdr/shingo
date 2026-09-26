//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// child_failure_lifted_docker_test.go — the two other doors that closed a chapter
// by cancelling every live leg: a leg failing (HandleChildOrderFailure) and the
// chapter-end arm finding a leg cancelled without the marker
// (chapterStoppedShort). The dissolve was fixed first
// (dissolve_lifted_docker_test.go); these are the same bug, and the same rule:
// no door cancels a leg whose claimed bin is at _TRANSIT.

// digWithACarrierAndAStoppedLeg plans a two-blocker dig, lifts leg 1's bin and
// ends leg 2 with `stopped` and `detail`. Leg 3 is still pending.
func digWithACarrierAndAStoppedLeg(t *testing.T, db *store.DB, d *Dispatcher, prefix string,
	stopped protocol.Status, detail string) (
	parent *orders.Order, lane *nodes.Node, legs []*orders.Order, transit *nodes.Node) {
	t.Helper()
	parent, lane, slots, legs := planDigToDissolve(t, db, d, prefix, 2)
	transit, err := db.GetNodeByDotName("_TRANSIT")
	testutil.MustNoErr(t, err, "resolve _TRANSIT")
	liftBin(t, db, d, legs[0], slots[0], transit)
	_, err = db.TerminalizeOrder(legs[1].ID, stopped, detail)
	testutil.MustNoErr(t, err, "stop leg 2")
	return parent, lane, legs, transit
}

// assertCarrierKept checks the chapter kept leg 1 and its bin, cancelled leg 3
// under the chapter-end marker, and cancelled nothing carrying a bin (soakstat's
// mid-lift rule, over the fixture).
func assertCarrierKept(t *testing.T, db *store.DB, parentID int64, legs []*orders.Order, transit *nodes.Node) *orders.Order {
	t.Helper()
	carrier, err := db.GetOrder(legs[0].ID)
	testutil.MustNoErr(t, err, "reload the carrying leg")
	if protocol.IsTerminal(carrier.Status) {
		t.Fatalf("the carrying leg %d is %q (%s) — a robot is holding its bin; cancelling the only "+
			"order that owns it leaves the bin on the robot with nowhere to go",
			carrier.ID, carrier.Status, carrier.ErrorDetail)
	}
	bin, err := db.GetBin(*legs[0].BinID)
	testutil.MustNoErr(t, err, "reload the lifted bin")
	if bin.ClaimedBy == nil || *bin.ClaimedBy != carrier.ID {
		t.Errorf("the lifted bin is claimed by %v, want leg %d", bin.ClaimedBy, carrier.ID)
	}
	rest, err := db.GetOrder(legs[2].ID)
	testutil.MustNoErr(t, err, "reload leg 3")
	if rest.Status != protocol.StatusCancelled || rest.ErrorDetail != reshuffleLegFailedDetail {
		t.Errorf("leg 3 is %q (%q), want cancelled under the chapter-end marker — it had not lifted "+
			"anything, and the chapter is over", rest.Status, rest.ErrorDetail)
	}
	for _, c := range legsOf(t, db, parentID) {
		if c.Status != protocol.StatusCancelled || c.BinID == nil {
			continue
		}
		b, err := db.GetBin(*c.BinID)
		testutil.MustNoErr(t, err, "reload a cancelled leg's bin")
		if b.NodeID != nil && *b.NodeID == transit.ID {
			t.Errorf("mid-lift cancel: leg %d was cancelled with its bin %d at _TRANSIT", c.ID, b.ID)
		}
	}
	return carrier
}

// assertChapterClosesOnLanding: the lane stays held while the robot is in it,
// and the chapter closes, the lane goes and the demand re-plans once it lands.
func assertChapterClosesOnLanding(t *testing.T, db *store.DB, d *Dispatcher, parentID int64, lane *nodes.Node, carrier *orders.Order) {
	t.Helper()
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parentID), "the cancel wiring's re-drive")
	if !d.laneLock.IsLocked(lane.ID) {
		t.Fatal("the chapter released the lane with a robot still in it carrying a blocker — the " +
			"re-burial window")
	}
	landLeg(t, d, db, carrier)
	testutil.MustNoErr(t, d.AdvanceCompoundOrder(parentID), "the landing's re-drive")
	if d.laneLock.IsLocked(lane.ID) {
		t.Error("the lane is still held after the carrying leg landed and the chapter closed")
	}
	after, err := db.GetOrder(parentID)
	testutil.MustNoErr(t, err, "reload the parent")
	if !protocol.IsAcquiring(after.Status) {
		t.Errorf("parent is %q after the chapter closed, want an acquiring status", after.Status)
	}
}

// TestChildFailure_NeverCancelsALiftedSibling: a leg failing closes the rest of
// its chapter, except a sibling carrying its bin, which sets it down first.
//
// When the failed leg is itself the carrier (a robot faulted mid-carry) it is
// already terminal and this door does not touch it; it goes the stranded-transit
// way it always has. Only its live siblings are kept.
//
// MUTATION: drop the lifted split in HandleChildOrderFailure. Leg 1 is cancelled
// under the chapter-end marker with its bin at _TRANSIT and the first assertion
// fires.
func TestChildFailure_NeverCancelsALiftedSibling(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, legs, transit := digWithACarrierAndAStoppedLeg(t, db, d, "CF-LIFT", protocol.StatusFailed, "fleet fault")

	d.HandleChildOrderFailure(parent.ID, legs[1].ID)

	carrier := assertCarrierKept(t, db, parent.ID, legs, transit)
	assertChapterClosesOnLanding(t, db, d, parent.ID, lane, carrier)
}

// TestChapterStoppedShort_NeverCancelsALiftedLeg: the chapter-end arm, finding a
// leg cancelled without the chapter-end marker (an operator or the fleet ending
// one leg), closes the rest of the chapter except a leg carrying its bin.
//
// That is the only way into the arm's cancel loop. A FAILED leg is a chapter
// boundary (endsItsChapter), so it is not in the open set the loop reads, and
// its siblings were HandleChildOrderFailure's to close. It is driven through
// advanceCompoundChapterEnd, the arm's only caller, so the pending leg 3 reaches
// the loop rather than being dispatched.
//
// MUTATION: drop the lifted split in chapterStoppedShort. Leg 1 is cancelled with
// its bin at _TRANSIT and the first assertion fires.
func TestChapterStoppedShort_NeverCancelsALiftedLeg(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent, lane, legs, transit := digWithACarrierAndAStoppedLeg(t, db, d, "CS-LIFT", protocol.StatusCancelled, "cancelled by an operator")

	testutil.MustNoErr(t, d.advanceCompoundChapterEnd(parent.ID), "the chapter-end arm")

	carrier := assertCarrierKept(t, db, parent.ID, legs, transit)
	assertChapterClosesOnLanding(t, db, d, parent.ID, lane, carrier)
}
