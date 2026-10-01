//go:build docker

package engine

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestReplayedPickup_SingleBinFallbackIgnoresWhereTheBinIs confirms round 4's
// russet #10 (S3: confirm or drop). After a Core restart the poller re-tracks
// an open order with no memory of its blocks, so every block already FINISHED
// is reported again. A replayed pickup that the junction and claimed-bin arms
// cannot name falls to resolvePickupBin's single-bin fallback, which answers
// order.BinID wherever that bin is: here a bin the order already delivered and
// Core stored at the line is named as lifted at storage, and
// handlePickupBlockCompleted would move it back to _TRANSIT until the replayed
// dropoff stores it again.
//
// Pinned RED: the fix (a location check on the fallback, or the poller keeping
// block memory across a restart) changes which pickups resolve and is put to
// the orc rather than built here.
func TestReplayedPickup_SingleBinFallbackIgnoresWhereTheBinIs(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, line, bp := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, bp.Code, line.ID, "SYN-BIN-R10") // already delivered to the line
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.Status = "r10", "line-1", "in_transit"
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(o.ID, bin.ID), "order bin")
	eng := newUnstartedEngine(t, db, simulator.New())
	binID, _, from, ok := eng.resolvePickupBin(o.ID, storage.Name) // the replayed pickup at storage
	got := fmt.Sprintf("ok=%v bin=%v from_line=%v", ok, binID == bin.ID, from == line.ID)
	const today, want = "ok=true bin=true from_line=true", "ok=false bin=false from_line=false"
	if got != today {
		t.Fatalf("bug:russet-10 characterisation moved: got %q, today was %q (want after a fix: %q)", got, today, want)
	}
	t.Logf("bug:russet-10 RED as expected: got %q, want %q", got, want)
}
