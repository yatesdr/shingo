package engine

import (
	"context"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/domain/flowspec"
	"shingoedge/store/processes"
)

// TWO STYLES MAY BRING A PRESS ITS NEXT CARRIER WITH DIFFERENT ROBOTS
// (owner, 2026-11-14: "thats the whole point of styles. it can change by
// part"). The preview and the save said otherwise: a warning on the press,
// "set it the same on every style". A press whose styles differ previews and
// saves with no next-carrier finding at all.
func TestFlow_TwoStylesMayDifferOnTheNextCarrier(t *testing.T) {
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
	preview, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: runStyle, Cells: cells})
	testutil.MustNoErr(t, err, "preview")
	if got := refillWarnings(preview.Findings); len(got) != 0 {
		t.Errorf("the preview: next-carrier findings %+v, want none: two styles may differ", got)
	}
	_, err = eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceAdmin, ToStyleID: runStyle, Cells: cells,
		Fingerprint: preview.Fingerprint, CalledBy: "engineer"})
	testutil.MustNoErr(t, err, "the save goes through")
	claims, err := db.ListStyleNodeClaims(runStyle)
	testutil.MustNoErr(t, err, "claims")
	for _, c := range claims {
		if c.CoreNodeName == "RB-PRESS" && c.RefillBy.OrDefault() != protocol.RefillByIndexRobot {
			t.Errorf("RB-PRESS saved %q, want this style's Robot 2 kept", c.RefillBy)
		}
	}
}
