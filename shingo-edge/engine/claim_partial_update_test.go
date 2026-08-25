package engine

import (
	"slices"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// ---------------------------------------------------------------------------
// A caller that edits one field must not erase the fields it never mentioned.
//
// updateClaim writes every column it is handed, and a value type cannot say "I
// have no opinion" — it says 0, "" or false. The columns no single writer owns
// are therefore pointer-typed on NodeClaimInput, and nil means "leave the
// stored value alone". That contract was written for four columns after the
// claims editor was found resetting board order and reorder provenance on every
// save; six more columns arrived afterwards and did not get it.
//
// The reachable caller is the replenishment admin page: PUT
// /api/replenishment/cell-reorder → UpdateCellReorder → processClaimToInput,
// which reads the whole claim and re-sends a subset. Changing a reorder point
// wiped the press's evacuation positions, its evacuation destination, the loader
// card flag and the key route.
// ---------------------------------------------------------------------------

// fullyConfiguredClaim seeds a claim with every optional column set to a
// non-zero value, so that anything a partial update drops shows up.
func fullyConfiguredClaim(t *testing.T, db *store.DB) (claimID int64, styleID int64) {
	t.Helper()

	processID, err := db.CreateProcess("PARTIAL-PROC", "partial update", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	_, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PU-FRONT", Code: "PUF", Name: "Front", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	styleID, err = db.CreateStyle("PARTIAL-STYLE", "partial", processID)
	testutil.MustNoErr(t, err, "create style")

	yes := true
	claimID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PU-FRONT", Role: protocol.ClaimRoleProduce,
		SwapMode:       protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: "PU-BACK",
		PayloadCode:    "PART-X", UOPCapacity: 100, ReorderPoint: 10,
		InboundSource: "MARKET", OutboundDestination: "MARKET",

		ChangeoverEvacNodes:       &[]string{"PU-FRONT", "PU-BACK"},
		ChangeoverEvacDestination: strPtr("TOOLING-BAY"),
		IndexRobotSupplies:        &yes,
		KeyRoute:                  &[]string{"WP_AISLE_N", "WP_AISLE_S"},
		KeyTask:                   strPtr("load"),
	})
	testutil.MustNoErr(t, err, "seed claim")
	return claimID, styleID
}

func strPtr(s string) *string { return &s }

// assertSixSurvive reads the claim back and checks every optional column the
// partial-update callers do not mention.
func assertSixSurvive(t *testing.T, db *store.DB, claimID int64, after string) {
	t.Helper()
	got, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read claim back")

	if !slices.Equal(got.ChangeoverEvacNodes, []string{"PU-FRONT", "PU-BACK"}) {
		t.Errorf("%s: changeover_evac_nodes = %v, want [PU-FRONT PU-BACK]", after, got.ChangeoverEvacNodes)
	}
	if got.ChangeoverEvacDestination != "TOOLING-BAY" {
		t.Errorf("%s: changeover_evac_destination = %q, want TOOLING-BAY", after, got.ChangeoverEvacDestination)
	}
	if !got.IndexRobotSupplies {
		t.Errorf("%s: index_robot_supplies = false, want true", after)
	}
	if !slices.Equal(got.KeyRoute, []string{"WP_AISLE_N", "WP_AISLE_S"}) {
		t.Errorf("%s: key_route = %v, want [WP_AISLE_N WP_AISLE_S]", after, got.KeyRoute)
	}
	if got.KeyTask != "load" {
		t.Errorf("%s: key_task = %q, want load", after, got.KeyTask)
	}
}

// TestUpdateCellReorder_LeavesUnmentionedColumnsAlone is the reachable defect:
// an engineer changing a reorder point on the replenishment admin page must not
// silently reconfigure the press.
func TestUpdateCellReorder_LeavesUnmentionedColumnsAlone(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	claimID, _ := fullyConfiguredClaim(t, db)
	eng := testEngine(t, db)

	testutil.MustNoErr(t, eng.UpdateCellReorder(CellReorderInput{
		ClaimID:      claimID,
		ReorderPoint: 25,
		Source:       "manual",
		AutoReorder:  true,
	}), "update cell reorder")

	// The edit itself must land.
	got, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read claim back")
	if got.ReorderPoint != 25 {
		t.Fatalf("reorder_point = %d, want 25 — the edit under test did not apply", got.ReorderPoint)
	}

	assertSixSurvive(t, db, claimID, "after a reorder-point edit")
}

// TestPartialClaimUpdate_LeavesUnmentionedColumnsAlone is the contract itself,
// independent of any one caller: an UpsertClaim that names none of the six
// leaves all six as they were. Any future partial-update path is covered by
// this the day it is written.
func TestPartialClaimUpdate_LeavesUnmentionedColumnsAlone(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	claimID, styleID := fullyConfiguredClaim(t, db)

	// The same claim, re-sent by a writer with no opinion about the six.
	_, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PU-FRONT", Role: protocol.ClaimRoleProduce,
		SwapMode:       protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: "PU-BACK",
		PayloadCode:    "PART-X", UOPCapacity: 100, ReorderPoint: 99,
		InboundSource: "MARKET", OutboundDestination: "MARKET",
	})
	testutil.MustNoErr(t, err, "partial upsert")

	assertSixSurvive(t, db, claimID, "after a partial upsert that named none of them")
}

// TestClaimUpdate_ExplicitEmptyStillClears is the other half of the pointer
// contract, and the reason nil-versus-empty has to be representable: a writer
// that DOES speak, and says "none", must clear the column. Without this the fix
// for the stomp would make the fields unclearable.
func TestClaimUpdate_ExplicitEmptyStillClears(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	claimID, styleID := fullyConfiguredClaim(t, db)

	no := false
	_, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PU-FRONT", Role: protocol.ClaimRoleProduce,
		SwapMode:       protocol.SwapModeTwoRobotPressIndex,
		PairedCoreNode: "PU-BACK",
		PayloadCode:    "PART-X", UOPCapacity: 100, ReorderPoint: 10,
		InboundSource: "MARKET", OutboundDestination: "MARKET",

		ChangeoverEvacNodes:       &[]string{},
		ChangeoverEvacDestination: strPtr(""),
		IndexRobotSupplies:        &no,
		KeyRoute:                  &[]string{},
		KeyTask:                   strPtr(""),
	})
	testutil.MustNoErr(t, err, "clearing upsert")

	got, err := db.GetStyleNodeClaim(claimID)
	testutil.MustNoErr(t, err, "read claim back")
	if len(got.ChangeoverEvacNodes) != 0 {
		t.Errorf("changeover_evac_nodes = %v, want empty", got.ChangeoverEvacNodes)
	}
	if got.ChangeoverEvacDestination != "" {
		t.Errorf("changeover_evac_destination = %q, want empty", got.ChangeoverEvacDestination)
	}
	if got.IndexRobotSupplies {
		t.Error("index_robot_supplies = true, want false")
	}
	if len(got.KeyRoute) != 0 {
		t.Errorf("key_route = %v, want empty", got.KeyRoute)
	}
	if got.KeyTask != "" {
		t.Errorf("key_task = %q, want empty", got.KeyTask)
	}
}
