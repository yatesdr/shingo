//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// TestDigFits_ADemandWhoseDropNeedsTheLastFreeSlotDoesNotDig: the dig verdict
// counts the demand's own drop into the dug group as owed, beside the blockers
// and what running digs owe: blockers + owed + the demand's drop <= reachable.
//
// The demo jam of 2026-10-08 (c2b-demo-after-b267fc26-20m): order 765 dug
// Lane_04 open for bin 24 while its own drop was bound for SYN_MARKET, the
// group it was digging in, which had one free slot left. Its dig's shuffles
// spent it, and when the dig ended 765's drop found "no available slot in node
// group SYN_MARKET": a dug-for demand holding its uncovered bin, waiting on a
// slot it had spent on its own dig.
//
// Fixture: the dug lane [S1 empty, S2 blocker, S3 the target], a one-slot
// sibling lane, empty: one free parking slot for one blocker. A complex demand
// buried at S3 whose last step drops into the group asks for its dig. It must
// not start; it waits under no-shuffle-slot. With a two-slot sibling, the same
// demand digs.
func TestDigFits_ADemandWhoseDropNeedsTheLastFreeSlotDoesNotDig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		sibDepth int
		wantDig  bool
	}{
		{"the last free slot", 1, false},
		{"room for both", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
			prefix := "DOD1"
			if tc.wantDig {
				prefix = "DOD2"
			}
			grp, dug, _, _, dugSlots, _, bp := setupDwellGroup(t, db, prefix, tc.sibDepth, false)
			createTestBinAtNode(t, db, bp.Code, dugSlots[1].ID, prefix+"-BLK")
			target := createTestBinAtNode(t, db, bp.Code, dugSlots[2].ID, prefix+"-TGT")

			demand := mkQueuedComplexParent(t, db, prefix+"-demand", bp.Code)
			steps, err := json.Marshal([]resolvedStep{
				{Action: protocol.ActionPickup, Node: dugSlots[2].Name},
				{Action: protocol.ActionDropoff, Node: "LINE-1"},
				{Action: protocol.ActionPickup, Node: "LINE-1"},
				{Action: protocol.ActionDropoff, Node: grp.Name, Group: grp.Name},
			})
			testutil.MustNoErr(t, err, "marshal the demand's steps")
			testutil.MustNoErr(t, db.UpdateOrderStepsJSON(demand.ID, string(steps)), "the demand's steps")
			demand, err = db.GetOrder(demand.ID)
			testutil.MustNoErr(t, err, "reload the demand")

			d.planBuriedReshuffleAtIntake(demand, bp.Code, "line-1",
				&BuriedError{Bin: target, Slot: dugSlots[2], LaneID: dug.ID})

			after, err := db.GetOrder(demand.ID)
			testutil.MustNoErr(t, err, "read the demand back")
			legs := legsOf(t, db, demand.ID)
			switch {
			case tc.wantDig && len(legs) == 0:
				t.Fatalf("with room for the blocker and the drop, the dig did not start (%q, %q)",
					after.QueueCause, after.QueueReason)
			case !tc.wantDig && len(legs) > 0:
				t.Fatalf("the dig started with %d leg(s) though the demand's own drop needs the group's last "+
					"free slot, the one its blocker will take", len(legs))
			case !tc.wantDig && QueueCause(after.QueueCause) != CauseNoShuffleSlot:
				t.Fatalf("refused, but parked under %q, want %q", after.QueueCause, CauseNoShuffleSlot)
			}
		})
	}
}
