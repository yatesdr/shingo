package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// TestSaveFlow_ARestatedReorderPointLeavesThePreviewCurrent: the replenishment
// page's Apply on a row whose reorder point and Auto reorder it does not
// change writes nothing the flow is made of, so a preview taken before it is
// still the plan and its save lands. It used to be refused PREVIEW STALE: the
// write stamped the row's reorder-point provenance, and the stamp was hashed
// into the fingerprint.
func TestSaveFlow_ARestatedReorderPointLeavesThePreviewCurrent(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)
	cells := cellsOf(t, db, toStyleID)
	fp, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fingerprint")

	same, err := db.GetStyleNodeClaimByNode(toStyleID, "FLOW-SAME")
	testutil.MustNoErr(t, err, "get claim")
	testutil.MustNoErr(t, eng.UpdateCellReorder(CellReorderInput{
		ClaimID: same.ID, ReorderPoint: same.ReorderPoint, AutoReorder: same.AutoReorder, CalledBy: "engineer",
	}), "restate the reorder point")
	after, err := db.GetStyleNodeClaim(same.ID)
	testutil.MustNoErr(t, err, "reread claim")
	if after.ReorderPoint != same.ReorderPoint || after.AutoReorder != same.AutoReorder {
		t.Fatalf("the restate moved the row: point %d auto %v, was %d %v",
			after.ReorderPoint, after.AutoReorder, same.ReorderPoint, same.AutoReorder)
	}
	if now, err := eng.FlowFingerprint(processID, toStyleID); err != nil || now != fp {
		t.Errorf("fingerprint after the restate = %q (%v), want the preview's %q", now, err, fp)
	}

	cells[0].PayloadCode = "PART-NEWER"
	if _, err := eng.SaveFlow(processID, FlowSaveRequest{
		Source: domain.ClaimSourceHMI, ToStyleID: toStyleID, Cells: cells, Fingerprint: fp, CalledBy: "Press 400",
	}); err != nil {
		t.Errorf("save over the preview taken before the restate: %v, want it saved", err)
	}
}
