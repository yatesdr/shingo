//go:build sim

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// SimMachineReady is the one spelling of "can this process's machine cycle
// right now" — the fake PLC's readiness gate delegates to it. These tests pin
// its fail-open direction and the cell shapes it must read correctly. The two
// measured deadlocks in its header are why the extraction happened; the
// operator-gate half that briefly grew alongside it is recorded for its next
// attempt in NOTES-release-gate-attempt-four.md at the GitHub root.

// machineFixture builds a process with a consume node and a produce node — the
// smallest shape that can express "this cell is stopped by its OTHER node",
// which is the deadlock a single-node fixture cannot reach.
type machineFixture struct {
	db        *store.DB
	consumeID int64
	produceID int64
	styleID   int64
	processID int64
}

func newMachineFixture(t *testing.T) *machineFixture {
	t.Helper()
	db := testEngineDB(t)

	procID, err := db.CreateProcess("MACHINE-PROC", "release when done", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	consumeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "ALN_003", Code: "A03", Name: "ALN_003", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create consume node")
	produceID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "ALN_005", Code: "A05", Name: "ALN_005", Sequence: 2, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create produce node")

	styleID, err := db.CreateStyle("MACHINE-STYLE", "", procID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "set active style")

	consumeClaim, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ALN_003", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PANEL-B",
		UOPCapacity: 30, ReorderPoint: 15, AutoReorder: domain.Ptr(true),
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_003", OutboundStaging: "SLN_004",
	})
	testutil.MustNoErr(t, err, "upsert consume claim")
	produceClaim, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ALN_005", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "ASSY",
		UOPCapacity:   20,
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_007", OutboundStaging: "SLN_008",
	})
	testutil.MustNoErr(t, err, "upsert produce claim")

	f := &machineFixture{
		db:        db,
		consumeID: consumeID, produceID: produceID, styleID: styleID, processID: procID,
	}
	// A running cell: consume half-drawn, produce half-filled, both bound.
	f.setRuntime(t, consumeID, consumeClaim, 23, 15)
	f.setRuntime(t, produceID, produceClaim, 39, 10)
	return f
}

// setRuntime binds a carrier and sets the cached count. binID 0 means NO carrier
// on the position — the state ALN_005 was in when the produce arm deadlocked.
func (f *machineFixture) setRuntime(t *testing.T, nodeID, claimID, binID int64, cached int) {
	t.Helper()
	_, err := f.db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	var bin *int64
	if binID != 0 {
		bin = &binID
	}
	testutil.MustNoErr(t, f.db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, bin, cached), "set runtime")
}

func (f *machineFixture) claimID(t *testing.T, nodeID int64) int64 {
	t.Helper()
	node, err := f.db.GetProcessNode(nodeID)
	testutil.MustNoErr(t, err, "get node")
	claim := requestedClaimAtNode(f.db, node)
	if claim == nil {
		t.Fatal("no active claim at node")
	}
	return claim.ID
}

// TestSimMachineReady_IsTheSameAnswerTheFakePLCGets pins the sharing itself.
// makeReadinessGate (cmd/shingoedge) delegates here; if this ever grows a second
// spelling the two drift, and a drift at the zero boundary is what deadlocks.
func TestSimMachineReady_IsTheSameAnswerTheFakePLCGets(t *testing.T) {
	t.Parallel()
	f := newMachineFixture(t)

	if !SimMachineReady(f.db.DB, f.processID, f.styleID) {
		t.Fatal("a fully bound, mid-count cell read as NOT ready — the fake PLC would refuse to tick it")
	}
}

// TestSimMachineReady_FailsOpen pins the direction of the unknown: an
// unresolvable read must be READY, because the fail-open protects the PLC's
// tick, and a DB blip must not stop the line.
func TestSimMachineReady_FailsOpen(t *testing.T) {
	t.Parallel()
	f := newMachineFixture(t)

	// An unknown process has no nodes: nothing refuses, so the machine reads
	// ready and the PLC keeps ticking.
	if !SimMachineReady(f.db.DB, 999999, f.styleID) {
		t.Error("an unknown process read as NOT ready")
	}
}
