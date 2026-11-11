package engine

import (
	"context"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/domain/flowspec"
	"shingoedge/store/processes"
)

// A PRESS WHOSE STYLES DISAGREE ABOUT WHO BRINGS ITS NEXT CARRIER IS SHOWN,
// NEVER REFUSED (owner ruling 5). The setting describes the press's robots, so
// a plant sets it the same on every style; a disagreement is an engineer who
// edited one style and not the other. The preview carries a warning on the
// press, the save goes through and its answer carries the same warning, and a
// press whose styles agree carries none. It was a log line alone.
func TestFlow_APressWhoseStylesDisagreeOnTheRefillIsWarnedNotRefused(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, err := db.CreateProcess("RB-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	for i, name := range []string{"RB-PRESS", "RB-BACK", "RB-OTHER"} {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, CoreNodeName: name, Code: name, Name: name, Sequence: i + 1, Enabled: true})
		testutil.MustNoErr(t, err, "node "+name)
		db.EnsureProcessNodeRuntime(id)
	}
	runStyle, err := db.CreateStyle("RB-RUN", "", processID)
	testutil.MustNoErr(t, err, "run style")
	altStyle, err := db.CreateStyle("RB-ALT", "", processID)
	testutil.MustNoErr(t, err, "alt style")
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &runStyle), "active style")
	press := func(styleID int64, node, back string, refill protocol.RefillBy) {
		_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: node, Role: protocol.ClaimRoleProduce,
			SwapMode: protocol.SwapModeTwoRobotPressIndex, PayloadCode: "RB-PART", PairedCoreNode: back,
			InboundSource: "RB-EMPTIES", OutboundDestination: "RB-MARKET", RefillBy: domain.Ptr(refill),
		})
		testutil.MustNoErr(t, err, "claim "+node)
	}
	press(runStyle, "RB-PRESS", "RB-BACK", protocol.RefillByIndexRobot)
	press(altStyle, "RB-PRESS", "RB-BACK", protocol.RefillByClearingRobot)
	press(runStyle, "RB-OTHER", "RB-OTHER-BACK", protocol.RefillByClearingRobot)
	press(altStyle, "RB-OTHER", "RB-OTHER-BACK", protocol.RefillByClearingRobot)
	eng := testEngine(t, db)
	cells := cellsOf(t, db, runStyle)

	refillWarnings := func(fs []domain.NodeFinding) []domain.NodeFinding {
		var out []domain.NodeFinding
		for _, f := range fs {
			if f.Field == flowspec.RefillBy {
				out = append(out, f)
			}
		}
		return out
	}
	check := func(what string, fs []domain.NodeFinding) {
		t.Helper()
		got := refillWarnings(fs)
		if len(got) != 1 || got[0].CoreNodeName != "RB-PRESS" || got[0].Severity != domain.SeverityWarning ||
			!strings.Contains(got[0].Message, "RB-ALT says Robot 1") || !strings.Contains(got[0].Message, "Robot 2") {
			t.Errorf("%s: refill findings = %+v, want one warning on RB-PRESS naming RB-ALT's Robot 1 against this style's Robot 2", what, got)
		}
	}

	preview, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: runStyle, Cells: cells})
	testutil.MustNoErr(t, err, "preview")
	check("the preview", preview.Findings)

	res, err := eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceAdmin, ToStyleID: runStyle, Cells: cells,
		Fingerprint: preview.Fingerprint, CalledBy: "engineer"})
	testutil.MustNoErr(t, err, "the save goes through")
	check("the save's answer", res.Notes)
}
