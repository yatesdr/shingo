//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestCompoundChild_CarriesTheBinsPayloadCode pins that a reshuffle child names
// the payload it is actually moving.
//
// The column was blank on every child ever written. Nothing broke, because
// nothing read it — but two things read it now that they can. Analytics cannot
// answer "how much does payload X move in reshuffling" from a blank column, and
// bin-history joins do not reconstruct it reliably over time. And the dispatcher
// feeds PayloadCode to loadSequenceForPayload, which picks the robot's bin-task
// sequence; a blank one takes the default.
//
// This is safe to land as one step rather than two because reshuffling has never
// run in production — there is no incumbent robot behavior to protect. The
// hazard would have been the other order: shipping blanks and filling them in
// later, which changes handling under a running plant.
func TestCompoundChild_CarriesTheBinsPayloadCode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	blocker := createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-CPC-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-CPC-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent := mkQueuedComplexParent(t, db, "uuid-child-payload", bp.Code)
	d.planBuriedReshuffleAtIntake(parent, bp.Code, "line-1",
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list reshuffle children")
	if len(children) == 0 {
		t.Fatal("no reshuffle children created — this test asserted nothing")
	}

	for _, c := range children {
		if c.BinID == nil {
			t.Errorf("child %d (seq %d) has no bin — every reshuffle child names the bin it moves", c.ID, c.Sequence)
			continue
		}
		bin, err := db.GetBin(*c.BinID)
		testutil.MustNoErr(t, err, "read the child's bin")
		if c.PayloadCode != bin.PayloadCode {
			t.Errorf("child %d (seq %d) payload_code = %q, want %q — the code on the bin it is moving",
				c.ID, c.Sequence, c.PayloadCode, bin.PayloadCode)
		}
		if c.PayloadCode == "" {
			t.Errorf("child %d (seq %d) payload_code is blank: reshuffle volume by payload is unanswerable from these rows, and the robot gets the default load sequence",
				c.ID, c.Sequence)
		}
	}
	_ = blocker
}

