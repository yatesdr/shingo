package store

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// running_bin_guard_test.go — the store refuses to strand a carrier, at every
// door.
//
// SaveFlow refuses a position move on the running style, and it was the ONLY
// door that did: the legacy POST/DELETE /api/style-node-claims and the
// replenishment page's write both reach DeleteStyleNodeClaim without passing
// it. A rule enforced at one of three doors is a rule the other two break.
//
// GATED ON THE BIN, not on "is this style running", and that distinction is
// what lets it live in the store. The hazard is a stranded CARRIER — take the
// position off the flow while a bin stands on it and nothing refills it,
// nothing counts the parts made off it, and every delivered handler looks past
// it, because they all resolve by (active_style_id, core_node_name). With no
// bin there is nothing to strand, and refusing anyway would forbid the
// ordinary setup sequence every seeder, the sim and 500-odd tests use: create
// the process, set its active style, then edit its positions.
//
// The composer's own guard stays and stays STRICTER: it sees the whole flow,
// so it refuses the move before anything is written and can say "PLN_01 is
// running — move it to PLN_03 after the next changeover". This is the half
// that cannot be bypassed.

// seedRunningCell builds a process running one style on PLN_01, with a
// runtime row for the position and no carrier on it yet.
func seedRunningCell(t *testing.T, db *DB) (processID, styleID, nodeID int64) {
	t.Helper()
	processID, styleID = seedClaimProcess(t, db, "P-RunningBin")
	var err error
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PLN_01", Code: "PLN_01", Name: "PLN_01", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateProcessNode: %v", err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	if err := db.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("SetActiveStyle: %v", err)
	}
	return processID, styleID, nodeID
}

func claimOn(t *testing.T, db *DB, styleID int64, payload string) int64 {
	t.Helper()
	id, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PLN_01", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: payload,
		InboundStaging: "PLN_02", InboundSource: "SMN", OutboundDestination: "SMN_OUT",
		Source: domain.ClaimSourceAdmin, CalledBy: "test",
	})
	if err != nil {
		t.Fatalf("UpsertStyleNodeClaim: %v", err)
	}
	return id
}

func TestDeleteClaim_RefusesToStrandARunningBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, styleID, nodeID := seedRunningCell(t, db)

	// The style IS the process's active one and the position has no carrier.
	// Deleting is configuration, not a live edit, and is allowed.
	id := claimOn(t, db, styleID, "PART-A")
	if err := db.DeleteStyleNodeClaim(id); err != nil {
		t.Fatalf("deleting a position with no bin on it was refused: %v", err)
	}

	// A carrier arrives. Now the same delete strands it.
	id = claimOn(t, db, styleID, "PART-A")
	if _, err := db.Exec(`UPDATE process_node_runtime_states SET active_bin_id = 4242
		WHERE process_node_id = ?`, nodeID); err != nil {
		t.Fatalf("place a bin: %v", err)
	}
	err := db.DeleteStyleNodeClaim(id)
	if !errors.Is(err, domain.ErrRunningPositionMove) {
		t.Fatalf("DeleteStyleNodeClaim = %v, want ErrRunningPositionMove — the bin on PLN_01 would be "+
			"left with no live claim, so nothing refills it and nothing counts it", err)
	}
	if !strings.Contains(err.Error(), "PLN_01") {
		t.Errorf("the refusal does not name the position: %v", err)
	}

	// A REFUSAL WITH A SIDE EFFECT IS WORSE THAN NO REFUSAL: the caller reads
	// an error and the flow has changed anyway.
	got, err := db.GetStyleNodeClaim(id)
	if err != nil || got == nil || got.RetiredAt != nil {
		t.Errorf("the refused delete removed or retired the claim anyway (err=%v, claim=%+v)", err, got)
	}
}

// TestDeleteClaim_ScopedToTheRunningStyleNotTheBin: the guard is on the
// CLAIM'S STYLE, not on "this position has a carrier".
//
// The sharp case, and the one the other two do not separate: the running style
// claims PLN_01 AND has a bin standing on it, and a DIFFERENT style — one the
// press is not running — also claims PLN_01. Dropping that other style's claim
// strands nothing: the runtime resolves by (active_style_id, core_node_name),
// so the bin on PLN_01 belongs to the running flow and stays on it, refilled,
// counted and evacuated exactly as before.
//
// Refusing here would be the broad rule that cost 527 tests: an engineer
// cannot edit ANY part's flow while the press is running, which is most of the
// day and is the work the composer exists for. The move that IS refused is the
// running style's own, and that is refused one layer up with the whole flow in
// view (engine.refuseRunningPositionMove).
func TestDeleteClaim_ScopedToTheRunningStyleNotTheBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	processID, runningID, nodeID := seedRunningCell(t, db)

	// The running style is ON the position, with a carrier standing there.
	runningClaim := claimOn(t, db, runningID, "PART-RUNNING")
	if _, err := db.Exec(`UPDATE process_node_runtime_states SET active_bin_id = 4242
		WHERE process_node_id = ?`, nodeID); err != nil {
		t.Fatalf("place a bin: %v", err)
	}

	// A part the press is not running, claiming the same position.
	other, err := db.CreateStyle("OTHER", "", processID)
	if err != nil {
		t.Fatalf("CreateStyle: %v", err)
	}
	otherClaim := claimOn(t, db, other, "PART-B")

	if err := db.DeleteStyleNodeClaim(otherClaim); err != nil {
		t.Fatalf("dropping a NON-running style's position was refused because the RUNNING style has a bin on it: %v\n"+
			"  the guard is on the claim's style, not on the position", err)
	}

	// And the running style's own claim on that same position is still
	// refused, so this is scoping and not a hole.
	if err := db.DeleteStyleNodeClaim(runningClaim); err == nil {
		t.Error("dropping the RUNNING style's position with a bin on it was allowed")
	} else if !errors.Is(err, domain.ErrRunningPositionMove) {
		t.Errorf("refused with %v, want ErrRunningPositionMove", err)
	}
}

// TestDeleteClaim_AllowsAStyleNobodyIsRunning: a bin on a position of a style
// the press is NOT running strands nothing, because that flow is not what the
// runtime resolves against.
func TestDeleteClaim_AllowsAStyleNobodyIsRunning(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	processID, _, nodeID := seedRunningCell(t, db)

	other, err := db.CreateStyle("OTHER", "", processID)
	if err != nil {
		t.Fatalf("CreateStyle: %v", err)
	}
	id := claimOn(t, db, other, "PART-B")
	if _, err := db.Exec(`UPDATE process_node_runtime_states SET active_bin_id = 4242
		WHERE process_node_id = ?`, nodeID); err != nil {
		t.Fatalf("place a bin: %v", err)
	}
	if err := db.DeleteStyleNodeClaim(id); err != nil {
		t.Fatalf("deleting a position of a style nobody is running was refused: %v", err)
	}
}
