package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// processes_reach_test.go — what the Replenishment page could do that
// Processes must, before the page goes: clear a reorder point, switch a line's
// auto-reorder during a changeover, and reach every claim the sweep can ask
// for.

// TestProcesses_AReorderPointClearsToZero: a save whose Advanced block names a
// point of 0 stores 0, the opt-out on either role (the popover's Clear the
// point sends it).
func TestProcesses_AReorderPointClearsToZero(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read the node")
	claim, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read the claim")
	if claim.ReorderPoint == 0 {
		t.Fatal("fixture drifted: the claim must start with a point")
	}
	cell := domain.Collapse(*claim)
	cell.Advanced = domain.AdvancedOf(claim)
	cell.Advanced.ReorderPoint, cell.Advanced.AutoReorder = 0, false
	fp, err := eng.FlowFingerprint(node.ProcessID, claim.StyleID)
	testutil.MustNoErr(t, err, "fingerprint")
	_, err = eng.SaveFlow(node.ProcessID, FlowSaveRequest{ToStyleID: claim.StyleID, Fingerprint: fp,
		Source: domain.ClaimSourceAdmin, CalledBy: "test", Cells: []domain.FlowCell{cell}})
	testutil.MustNoErr(t, err, "save the cleared point")
	after, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read the claim again")
	if after.ReorderPoint != 0 || after.AutoReorder {
		t.Errorf("after the clear: point %d, auto-reorder %v; want 0 and off", after.ReorderPoint, after.AutoReorder)
	}
}

// TestFlipAutoReorder_WorksWhileAChangeoverIsOpen: a save is refused during a
// changeover, and the line's switch is not, so auto-reorder can still be
// switched off (and on) while one runs.
func TestFlipAutoReorder_WorksWhileAChangeoverIsOpen(t *testing.T) {
	t.Parallel()
	l := newIdleLine(t, time.Minute)
	_, err := l.db.DB.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, started_at) VALUES (?, ?, 'running', ?)`,
		l.processID, l.styleID, time.Now().UTC().Format("2006-01-02 15:04:05"))
	testutil.MustNoErr(t, err, "open a changeover")
	testutil.MustNoErr(t, l.eng.FlipAutoReorder(l.processID, false, "Pat", domain.FlipFromProcesses), "switch off mid-changeover")
	if got := l.hold(t); got != domain.ReorderHoldPersonOff {
		t.Errorf("the position holds %q, want a person's off", got)
	}
	testutil.MustNoErr(t, l.eng.FlipAutoReorder(l.processID, true, "Pat", domain.FlipFromProcesses), "switch on mid-changeover")
}

// TestLevelSweep_AClaimAtNoLivePositionIsNeverAskedFor: the sweep walks the
// process's live positions, so an armed, breached claim at a node that is not
// one is never asked for. Processes cannot show such a claim (composer-model.js
// toCells walks positions), and has nothing to reach there that would ask.
func TestLevelSweep_AClaimAtNoLivePositionIsNeverAskedFor(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, claimID := keeperFixture(t)
	setLevel(t, db, nodeID, claimID, 40) // the fixture's own position is full
	node, err := db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "read the node")
	proc, err := db.GetProcess(node.ProcessID)
	testutil.MustNoErr(t, err, "read the process")
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: *proc.ActiveStyleID, CoreNodeName: "ALN_ELSEWHERE", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PANEL-B",
		UOPCapacity: 40, ReorderPoint: 25, AutoReorder: domain.Ptr(true),
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_E05", OutboundStaging: "SLN_E06",
	})
	testutil.MustNoErr(t, err, "a claim at no position")
	eng.sweepCellLevels()
	if got := countOrders(t, db); got != 0 {
		t.Errorf("the sweep asked %d orders; the only breach is a claim at no position", got)
	}
}
