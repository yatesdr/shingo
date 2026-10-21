package engine

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// A position of the RUNNING style may change when no bin is left behind
// (refuseRunningPositionMove). The fixture runs FLOW-FROM on FLOW-SWAP,
// FLOW-SAME and FLOW-GONE, every one staging at FLOW-STG; FLOW-ADD is free.

// runningMove is the running style's flow with FLOW-GONE re-pointed at to, or
// dropped when to is "".
func runningMove(t *testing.T, db *store.DB, styleID int64, to string) []domain.FlowCell {
	t.Helper()
	var out []domain.FlowCell
	for _, c := range cellsOf(t, db, styleID) {
		if c.CoreNodeName == "FLOW-GONE" {
			if to == "" {
				continue
			}
			c.CoreNodeName = to
		}
		out = append(out, c)
	}
	return out
}

// saveRunning saves cells to the running style through the HMI door.
func saveRunning(t *testing.T, eng *Engine, processID, styleID int64, cells []domain.FlowCell) error {
	t.Helper()
	fp, err := eng.FlowFingerprint(processID, styleID)
	testutil.MustNoErr(t, err, "fingerprint")
	_, err = eng.SaveFlow(processID, FlowSaveRequest{
		Source: domain.ClaimSourceHMI, ToStyleID: styleID, Cells: cells, Fingerprint: fp, CalledBy: "Press 400",
	})
	return err
}

func bareRow(node string) NodeBinInfo { return NodeBinInfo{NodeName: node} }

func binRow(node, label, part string) NodeBinInfo {
	return NodeBinInfo{NodeName: node, Occupied: true, BinLabel: label, PayloadCode: part}
}

// holdActableBins has Core answer the node-bins door with rows, as the save's
// read asks it on this tree (runningMoveOccupancy); a node not among them is
// answered as an empty node, as Core answers.
func holdActableBins(t *testing.T, eng *Engine, rows ...NodeBinInfo) {
	t.Helper()
	m := map[string]NodeBinInfo{}
	for _, r := range rows {
		m[r.NodeName] = r
	}
	eng.coreClient = stubCoreClient(ksNodeBinsStub(t, m).URL)
}

func runningMoveRig(t *testing.T) (*Engine, *store.DB, int64, int64) {
	t.Helper()
	db := testEngineDB(t)
	processID, fromStyleID, _ := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	enableComposer(t, db, processID)
	return eng, db, processID, fromStyleID
}

func TestRunningMove_ABareLeavingPositionSaves(t *testing.T) {
	t.Parallel()
	eng, db, processID, styleID := runningMoveRig(t)
	holdActableBins(t, eng, bareRow("FLOW-GONE"), bareRow("FLOW-ADD"))

	err := saveRunning(t, eng, processID, styleID, runningMove(t, db, styleID, "FLOW-ADD"))
	testutil.MustNoErr(t, err, "move a bare running position")
	claims, err := db.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "reload")
	var names []string
	for _, c := range claims {
		names = append(names, c.CoreNodeName)
	}
	if strings.Contains(strings.Join(names, ","), "FLOW-GONE") || !strings.Contains(strings.Join(names, ","), "FLOW-ADD") {
		t.Errorf("running style's positions after the move = %v, want FLOW-ADD and no FLOW-GONE", names)
	}
}

func TestRunningMove_ALeavingPositionHoldingABinIsRefusedWithTheCoreFirstWords(t *testing.T) {
	t.Parallel()
	eng, db, processID, styleID := runningMoveRig(t)
	holdActableBins(t, eng, binRow("FLOW-GONE", "BIN-7", "PART-GONE"), bareRow("FLOW-ADD"))
	before := snapshotFlowState(t, db, styleID)

	err := saveRunning(t, eng, processID, styleID, runningMove(t, db, styleID, "FLOW-ADD"))
	if !errors.Is(err, ErrRunningPositionMove) {
		t.Fatalf("err = %v, want ErrRunningPositionMove", err)
	}
	want := "FLOW-GONE holds BIN-7. Move the bin in Core first (Bins → Move to FLOW-ADD), then change the position here."
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal = %q, want it to say %q", err, want)
	}
	if after := snapshotFlowState(t, db, styleID); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused save wrote something:\n before %+v\n after  %+v", before, after)
	}
}

// Core first: the bin was moved in Core to FLOW-ADD, so the leaving position is
// bare and the arriving one holds the position's own part. That saves.
func TestRunningMove_TheBinMovedInCoreFirstSaves(t *testing.T) {
	t.Parallel()
	eng, db, processID, styleID := runningMoveRig(t)
	holdActableBins(t, eng, bareRow("FLOW-GONE"), binRow("FLOW-ADD", "BIN-7", "PART-GONE"))

	testutil.MustNoErr(t, saveRunning(t, eng, processID, styleID, runningMove(t, db, styleID, "FLOW-ADD")),
		"move onto the node the bin was moved to in Core")
}

func TestRunningMove_AMoveOntoAnotherPartsBinIsRefused(t *testing.T) {
	t.Parallel()
	eng, db, processID, styleID := runningMoveRig(t)
	holdActableBins(t, eng, bareRow("FLOW-GONE"), binRow("FLOW-ADD", "BIN-9", "PART-OTHER"))

	err := saveRunning(t, eng, processID, styleID, runningMove(t, db, styleID, "FLOW-ADD"))
	if !errors.Is(err, ErrRunningPositionMove) || !strings.Contains(err.Error(), "FLOW-ADD holds BIN-9 of PART-OTHER") {
		t.Fatalf("err = %v, want the move onto another part's bin refused by name", err)
	}
}

func TestRunningMove_DeleteIsRefusedTheSameWayAndSavesWhenBare(t *testing.T) {
	t.Parallel()
	eng, db, processID, styleID := runningMoveRig(t)
	holdActableBins(t, eng, binRow("FLOW-GONE", "BIN-7", "PART-GONE"))
	err := saveRunning(t, eng, processID, styleID, runningMove(t, db, styleID, ""))
	want := "FLOW-GONE holds BIN-7. Move the bin in Core first (Bins → Move), then change the position here."
	if !errors.Is(err, ErrRunningPositionMove) || !strings.Contains(err.Error(), want) {
		t.Fatalf("delete with a bin: err = %v, want %q", err, want)
	}

	holdActableBins(t, eng, bareRow("FLOW-GONE"))
	testutil.MustNoErr(t, saveRunning(t, eng, processID, styleID, runningMove(t, db, styleID, "")), "delete a bare position")
}

// A partner or staging change that stops naming a node holding a carrier
// strands it as surely as a moved head: nothing reads that node again. The
// whole claim's places are diffed, so it is refused with the same words.
func TestRunningMove_AStagingChangeThatUnnamesACarrierIsRefused(t *testing.T) {
	t.Parallel()
	eng, db, processID, styleID := runningMoveRig(t)
	id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, CoreNodeName: "FLOW-STG2", Code: "FLOW-STG2",
		Name: "FLOW-STG2", Sequence: 9, Enabled: true})
	testutil.MustNoErr(t, err, "create FLOW-STG2")
	db.EnsureProcessNodeRuntime(id)
	holdActableBins(t, eng, binRow("FLOW-STG", "BIN-3", "PART-SAME"), bareRow("FLOW-STG2"))

	cells := cellsOf(t, db, styleID)
	for i := range cells {
		cells[i].InboundStaging = "FLOW-STG2"
	}
	err = saveRunning(t, eng, processID, styleID, cells)
	if !errors.Is(err, ErrRunningPositionMove) || !strings.Contains(err.Error(), "FLOW-STG holds BIN-3.") {
		t.Fatalf("err = %v, want the staging node holding a carrier refused by name", err)
	}
}

// With no answer from Core to decide from, a position change is refused with a
// retry, never saved on a guess.
func TestRunningMove_NoCopyIsARetryNotAGuess(t *testing.T) {
	t.Parallel()
	eng, db, processID, styleID := runningMoveRig(t)
	err := saveRunning(t, eng, processID, styleID, runningMove(t, db, styleID, "FLOW-ADD"))
	if !errors.Is(err, ErrRunningPositionMove) || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("err = %v, want a refusal asking to try again", err)
	}
}

// A press's partner change: the running PRESS-1 indexes from FLOW-BACK; the
// draft re-pairs it with FLOW-BACK2 while FLOW-BACK still holds a carrier. No
// claim would name FLOW-BACK again (the orphaned pair), so it is refused.
func TestRunningMove_APartnerChangeThatUnnamesACarrierIsRefused(t *testing.T) {
	t.Parallel()
	eng, db, processID, styleID := runningMoveRig(t)
	for i, name := range []string{"PRESS-1", "FLOW-BACK", "FLOW-BACK2"} {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, CoreNodeName: name, Code: name,
			Name: name, Sequence: 10 + i, Enabled: true})
		testutil.MustNoErr(t, err, "create "+name)
		db.EnsureProcessNodeRuntime(id)
	}
	_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PRESS-1", Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
		PayloadCode: "PART-PRESS", PairedCoreNode: "FLOW-BACK", InboundSource: "FLOW-SRC", OutboundDestination: "FLOW-DST",
	})
	testutil.MustNoErr(t, err, "claim PRESS-1")
	holdActableBins(t, eng, binRow("FLOW-BACK", "BIN-5", ""), bareRow("FLOW-BACK2"))

	cells := cellsOf(t, db, styleID)
	for i := range cells {
		if cells[i].CoreNodeName == "PRESS-1" {
			cells[i].PairedCoreNode = "FLOW-BACK2"
		}
	}
	err = saveRunning(t, eng, processID, styleID, cells)
	want := "FLOW-BACK holds BIN-5. Move the bin in Core first (Bins → Move to FLOW-BACK2), then change the position here."
	if !errors.Is(err, ErrRunningPositionMove) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}
