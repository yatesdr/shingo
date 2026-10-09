//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/nodes"
)

// requireSoftHoldCovers asserts the C2b A.1b cover of the naked-target window:
// demandID holds a pending row on the bin standing at slotID, and both burial
// guards read it. A stranger's store is offered no slot in front of that bin,
// and the shuffle walk excludes every slot in front of it.
//
// It replaced the assertions that the lane was held as the demand's OUTBOUND
// row after its own dig. That row was a hold at rest for a pre-dispatch demand
// (v4 §2.6); the soft hold is what a wait may keep, and it protects the same set.
func requireSoftHoldCovers(t *testing.T, db *store.DB, demandID, slotID int64) {
	t.Helper()
	slot, err := db.GetNode(slotID)
	slot = testutil.Must(t, slot, err, "read the target slot")
	at, err := db.ListBinsByNode(slotID)
	testutil.MustNoErr(t, err, "read the bin at the target slot")
	if len(at) != 1 {
		t.Fatalf("slot %s holds %d bins, want the one the dig uncovered", slot.Name, len(at))
	}
	var soft int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM reservations
		WHERE order_id = $1 AND resource_kind = 'bin' AND bin_id = $2 AND state = 'pending'`,
		demandID, at[0].ID).Scan(&soft), "count the demand's soft hold")
	if soft != 1 {
		t.Fatalf("demand %d holds %d pending rows on bin %d at %s, want 1 — the soft hold its dig "+
			"took at birth is what covers the uncovered bin until its next pass", demandID, soft, at[0].ID, slot.Name)
	}

	got, err := db.FindStoreSlotInLaneExcluding(*slot.ParentID, 0)
	if err == nil && got.Depth != nil && slot.Depth != nil && *got.Depth < *slot.Depth {
		t.Fatalf("a stranger's store was offered %s, in front of bin %d at %s that demand %d is "+
			"coming for — the dig's emptied slots re-bury the bin it was run to expose",
			got.Name, at[0].ID, slot.Name, demandID)
	}

	lane, err := db.GetNode(*slot.ParentID)
	lane = testutil.Must(t, lane, err, "read the lane")
	blocked, err := db.SlotsBlockedByHardClaims(*lane.ParentID, 0)
	testutil.MustNoErr(t, err, "read the walk's blocked set")
	siblings, err := db.ListLaneSlots(lane.ID)
	testutil.MustNoErr(t, err, "list the lane's slots")
	for _, s := range siblings {
		if inFront(s, slot) && !blocked[s.ID] {
			t.Fatalf("the shuffle walk may park a blocker at %s, in front of bin %d that demand %d "+
				"is coming for", s.Name, at[0].ID, demandID)
		}
	}
}

func inFront(s, target *nodes.Node) bool {
	return s.Depth != nil && target.Depth != nil && *s.Depth < *target.Depth
}
