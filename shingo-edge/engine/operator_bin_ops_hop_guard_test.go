// operator_bin_ops_hop_guard_test.go — regression tests for the Hopkinsville
// press-index guards on the bin load/count path (hop A1, 2026-07-23).
package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestLoadBin_RejectsStampOnPairedNode is the hop A1 regression: LOAD refuses to
// stamp a part number onto a press-index paired / on-deck position (those hold
// empties only). The guard runs before the manual_swap/claim gates, so the
// operator gets the clear on-deck message, and Core is never asked to stamp.
func TestLoadBin_RejectsStampOnPairedNode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, err := db.CreateProcess("A1-PROC", "a1", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := db.CreateStyle("A1-STYLE", "a1", processID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active style")

	// Front (core) node + its press-index claim naming BACK as the paired
	// (on-deck) position. BACK itself carries no claim.
	_, err = db.CreateProcessNode(processes.NodeInput{ProcessID: processID, CoreNodeName: "A1-FRONT", Code: "F", Name: "Front", Sequence: 1, Enabled: true})
	testutil.MustNoErr(t, err, "create front node")
	backNodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, CoreNodeName: "A1-BACK", Code: "B", Name: "Back", Sequence: 2, Enabled: true})
	testutil.MustNoErr(t, err, "create back node")
	_, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "A1-FRONT", Role: "produce",
		SwapMode: protocol.SwapModeTwoRobotPressIndex, PayloadCode: "WIDGET", UOPCapacity: 100,
		PairedCoreNode: "A1-BACK", InboundSource: "EMPTY", OutboundDestination: "OUT",
	})
	testutil.MustNoErr(t, err, "upsert press-index claim")

	eng := testEngine(t, db)
	err = eng.LoadBin(backNodeID, "WIDGET", 10, []protocol.IngestManifestItem{{PartNumber: "P1", Quantity: 10}})
	if err == nil {
		t.Fatal("expected LoadBin to reject a part stamp on the paired/on-deck node")
	}
	if !strings.Contains(err.Error(), "on-deck") {
		t.Errorf("LoadBin error = %q, want the clear on-deck / paired-position message", err.Error())
	}
}
