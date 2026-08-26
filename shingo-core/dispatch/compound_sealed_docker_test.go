//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
)

// TestCompound_OpenParentIsNotFinished is the PRIMARY sealedness guard — the one
// on the path the plant actually takes.
//
// "All children terminal" answers three different questions in one block of
// AdvanceCompoundOrder, and only the last of them needs sealedness:
//
//	did anything go wrong?        -> fail the parent      (no)
//	is anything running now?      -> wait                 (no)
//	is this reshuffle FINISHED?   -> resume or complete    (YES)
//
// The third is sound today only because every compound writes all of its
// children at once, so no-pending means no-more. Under the fold, all-terminal is
// the ordinary state BETWEEN moves, and finishing there completes a half-dug
// lane and drops its dig hold with blockers still standing in it.
//
// THE SWEEP IS NOT THE ONLY READER, which is what this test exists to say. The
// brief had AdvanceStuckReshuffleParents as the thing to guard; it is the
// secondary one. This path is reached by the poller and the event chain
// (wiring_completion.go -> HandleChildOrderComplete -> compound.go) with no
// sweep involved, so guarding only the sweep would have shipped the concept
// with its main consumer unprotected.
//
// DESIGN §16 rule 7: the seal check is the FIRST thing that can refuse here, and
// the fixture is built so nothing upstream gets there first — the children are
// terminal and CONFIRMED (a failed or cancelled one would take the fail arm two
// checks earlier), none is pending (or the wait arm fires), and the parent
// loads. Seeded terminal via testdb.SeedOrderStatus rather than driven, because
// what is under test is the read of a terminal child set, not how it got there.
//
// THE FIXTURE IS AHEAD OF PRODUCTION. Nothing opens a compound yet — the fold
// does that in 5c — so this open parent is constructed deliberately through the
// production writer. The coverage is real; the trigger is not live. Until it is,
// this guard is a no-op in the plant, which is exactly what makes it safe to
// land before the fold instead of during it.
//
// MUTATION (verified): delete the `parent.OpenForChildren` guard. The first
// assertion fires — the parent completes to `confirmed` while still open, which
// under the fold is a reshuffle declared finished with its lane half dug.
func TestCompound_OpenParentIsNotFinished(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent, children, _, _ := twoLegCompound(t, db, "SEALED")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Every leg done, nothing pending — today's "the reshuffle is over" shape.
	for _, c := range children {
		testdb.SeedOrderStatus(t, db, c.ID, string(protocol.StatusConfirmed), "leg delivered")
	}

	if err := db.SetCompoundOpen(parent.ID, true); err != nil {
		t.Fatalf("open the compound: %v", err)
	}

	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance an open compound must be a no-op, not an error: %v", err)
	}
	got, err := db.GetOrder(parent.ID)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if got.Status != protocol.StatusReshuffling {
		t.Fatalf("parent is %s, want still %s — every leg so far is terminal but the reshuffle is OPEN, "+
			"so it is between moves, not finished. Completing here ends a dig that has more to do and "+
			"releases the lane with blockers still in it",
			got.Status, protocol.StatusReshuffling)
	}

	// And the guard is sealedness, not paralysis: seal it and the same call finishes.
	if err := db.SetCompoundOpen(parent.ID, false); err != nil {
		t.Fatalf("seal the compound: %v", err)
	}
	if err := d.AdvanceCompoundOrder(parent.ID); err != nil {
		t.Fatalf("advance a sealed compound: %v", err)
	}
	got, err = db.GetOrder(parent.ID)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if got.Status != protocol.StatusConfirmed {
		t.Fatalf("parent is %s after sealing, want %s — a guard that never lets go is a wedge, not a "+
			"gate, and this is the arm that tells the two apart", got.Status, protocol.StatusConfirmed)
	}
}
