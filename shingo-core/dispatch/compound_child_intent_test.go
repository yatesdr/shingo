//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestCompoundChild_CarriesTheLocalSourceIntent pins that a reshuffle child says
// how it sources its bin: locally, by name.
//
// source_intent classifies how an order finds a bin — a payload-matched full one
// through the finder, an empty carrier, or a specific bin at a known node.
// Children were written with "", which is the DEFAULT-FULL value, not an unset
// one. That reads as "find me any full bin of this payload, plant-wide", which
// is the opposite of what a reshuffle child does: it names its exact bin, and
// there is nothing to find.
func TestCompoundChild_CarriesTheLocalSourceIntent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-CSI-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-CSI-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent := mkQueuedComplexParent(t, db, "uuid-child-intent", bp.Code)
	d.planBuriedReshuffleAtIntake(parent, bp.Code, "line-1",
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	// THE LEGS MOVED HOUSE, THE ASSERTION DID NOT. They used to hang off the
	// demand, which was its own dig's parent; a complex demand is now a customer
	// of a lane-clear dig and the legs hang off THAT. What this test is about —
	// what a reshuffle leg carries — is unchanged, so it follows the legs.
	children := serviceDigChildren(t, db, parent)

	for _, c := range children {
		if c.SourceIntent != SourceIntentLocal {
			t.Errorf("child %d (seq %d) source_intent = %q, want %q — it names its bin; there is nothing to find",
				c.ID, c.Sequence, c.SourceIntent, SourceIntentLocal)
		}
		// The value must agree with what the type says, or the two become
		// separate opinions about the same order.
		if want := SourceIntentForType(c.OrderType); c.SourceIntent != want {
			t.Errorf("child %d source_intent = %q but SourceIntentForType(%q) = %q — the column and the type disagree",
				c.ID, c.SourceIntent, c.OrderType, want)
		}
	}
}

// TestCompoundChild_StaysOutOfTheSourceFinder is the guard that makes the stamp
// above safe to have made.
//
// The finder's scanner skips any order with a parent (scanner.go). That skip is
// what keeps a reshuffle child out of plant-wide FIFO source selection, and it
// is INTENDED behavior rather than an accident of the current predicate — same
// spirit as the compound capacity-exemption pin.
//
// Without this, a later "helpful" change to the intent stamp could quietly route
// children into the finder, where they would be handed a different bin than the
// one they were planned to move.
func TestCompoundChild_StaysOutOfTheSourceFinder(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-CSF-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-CSF-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent := mkQueuedComplexParent(t, db, "uuid-child-finder", bp.Code)
	d.planBuriedReshuffleAtIntake(parent, bp.Code, "line-1",
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	// THE LEGS MOVED HOUSE, THE ASSERTION DID NOT. They used to hang off the
	// demand, which was its own dig's parent; a complex demand is now a customer
	// of a lane-clear dig and the legs hang off THAT. What this test is about —
	// what a reshuffle leg carries — is unchanged, so it follows the legs.
	children := serviceDigChildren(t, db, parent)

	acquiring, err := db.ListAcquiringOrders()
	testutil.MustNoErr(t, err, "list the orders the finder would consider")
	inFinder := map[int64]bool{}
	for _, o := range acquiring {
		inFinder[o.ID] = true
	}
	for _, c := range children {
		if inFinder[c.ID] {
			t.Errorf("reshuffle child %d (seq %d) is visible to the source finder. It already names bin %v; the finder would pick a different one.",
				c.ID, c.Sequence, c.BinID)
		}
		if c.ParentOrderID == nil {
			t.Errorf("child %d has no parent_order_id — that column is what the scanner's skip keys on", c.ID)
		}
	}
}
