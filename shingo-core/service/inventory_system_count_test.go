//go:build docker

package service

import (
	"context"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/store/nodes"
)

// TestSystemUOPForPayload_ExcludesOutOfLoop: flagged, maintenance and retired
// bins are out of the kanban loop and their parts must not count toward the
// in-loop total (outOfLoopFilterSQL). Each status verified on its own.
func TestSystemUOPForPayload_ExcludesOutOfLoop(t *testing.T) {
	t.Parallel()
	for _, status := range []domain.BinStatus{domain.BinStatusFlagged, domain.BinStatusMaintenance, domain.BinStatusRetired} {
		t.Run(string(status), func(t *testing.T) {
			db := testDB(t)
			const payload = "PART-LOOP"
			at := &nodes.Node{Name: "LOOP-" + string(status), Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(at), "create node")
			createTestBin(t, db, at.ID, "IN-LOOP", payload, 10)
			out := createTestBin(t, db, at.ID, "OUT-OF-LOOP", payload, 100)
			testutil.MustNoErr(t, db.UpdateBinStatus(out.ID, status), "set status")

			res, err := NewInventoryService(db).SystemUOPForPayload(context.Background(), []string{payload})
			testutil.MustNoErr(t, err, "SystemUOPForPayload")
			if len(res.Counts) != 1 || res.Counts[0].BinUOP != 10 {
				t.Errorf("BinUOP with one %s bin = %+v, want 10 (only the in-loop bin)", status, res.Counts)
			}
		})
	}
}
