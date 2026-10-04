package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// TestSaveFlow_KeptSpotMovedBetweenCellsInOneSave: the composer upserts every
// cell before it deletes the ones the flow left out, so a spot handed from a
// removed cell to another cell is named by both until the delete. The
// dedicated-spot check runs after the deletes and accepts the save; a check
// inside each upsert would have refused it.
func TestSaveFlow_KeptSpotMovedBetweenCellsInOneSave(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)

	swap, err := db.GetStyleNodeClaimByNode(toStyleID, "FLOW-SWAP")
	testutil.MustNoErr(t, err, "get FLOW-SWAP")
	in := domain.InputFromClaim(*swap)
	in.InboundStaging, in.KeepStagedNode = "KEEP-SPOT", domain.Ptr("KEEP-SPOT")
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
	testutil.MustNoErr(t, err, "keep a spare at FLOW-SWAP")

	fp, err := eng.FlowFingerprint(processID, toStyleID)
	testutil.MustNoErr(t, err, "fingerprint")
	same, err := db.GetStyleNodeClaimByNode(toStyleID, "FLOW-SAME")
	testutil.MustNoErr(t, err, "get FLOW-SAME")
	var cells []domain.FlowCell
	for _, c := range cellsOf(t, db, toStyleID) {
		switch c.CoreNodeName {
		case "FLOW-SWAP":
			continue // left out: deleted after the upserts
		case "FLOW-SAME":
			c.InboundStaging = "KEEP-SPOT"
			c.Advanced = domain.AdvancedOf(same)
			c.Advanced.KeepStagedNode = "KEEP-SPOT"
		}
		cells = append(cells, c)
	}

	res, err := eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: toStyleID,
		Cells: cells, Fingerprint: fp, CalledBy: "Press 400"})
	testutil.MustNoErr(t, err, "save that moves the spot from FLOW-SWAP to FLOW-SAME")
	if res.Deleted != 1 {
		t.Errorf("deleted %d, want FLOW-SWAP's claim", res.Deleted)
	}
	got, err := db.GetStyleNodeClaimByNode(toStyleID, "FLOW-SAME")
	testutil.MustNoErr(t, err, "get FLOW-SAME after")
	if got.KeepStagedNode != "KEEP-SPOT" {
		t.Errorf("FLOW-SAME keeps its spare at %q, want KEEP-SPOT", got.KeepStagedNode)
	}
}
