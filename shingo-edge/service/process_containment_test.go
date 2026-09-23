package service

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// The Quality Hold settings toggle's write path: SetContainment stamps or
// clears the containment destination on a process's PRODUCE claims (all live
// styles), and ContainmentState derives the toggle's read back from them.
// The claim is the one storage — these tests pin that the stamp touches only
// produce rows, survives the idempotent replay, and clears cleanly.
func TestProcessContainment_SetAndClear(t *testing.T) {
	db := testdb.Open(t)
	svc := NewProcessService(db)

	pid, err := db.CreateProcess("HOLD-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleA, err := db.CreateStyle("HOLD-A", "", pid)
	testutil.MustNoErr(t, err, "create style A")
	styleB, err := db.CreateStyle("HOLD-B", "", pid)
	testutil.MustNoErr(t, err, "create style B")

	seed := func(styleID int64, node string, role protocol.ClaimRole) {
		t.Helper()
		_, err := processes.UpsertClaim(db.DB, processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: node, Role: role,
			SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PART-H",
			OutboundDestination: "FG-9", InboundStaging: "STG-1",
		})
		testutil.MustNoErr(t, err, "seed claim "+node)
	}
	seed(styleA, "PLN-1", protocol.ClaimRoleProduce)
	seed(styleA, "ALN-9", protocol.ClaimRoleConsume)
	seed(styleB, "PLN-2", protocol.ClaimRoleProduce)

	// Enabled without a destination is refused before any claim is touched.
	if err := svc.SetContainment(pid, true, "", "tester"); err == nil ||
		!strings.Contains(err.Error(), "destination is required") {
		t.Fatalf("err = %v, want the destination-required refusal", err)
	}

	testutil.MustNoErr(t, svc.SetContainment(pid, true, "HOLD-1", "tester"), "enable")

	// Produce claims across BOTH styles carry the route; the consume claim
	// (a swap return's outbound) is untouched — stamping one would be inert
	// at best, so the stamp deliberately never does.
	for _, styleID := range []int64{styleA, styleB} {
		claims, err := db.ListStyleNodeClaims(styleID)
		testutil.MustNoErr(t, err, "list claims")
		for _, c := range claims {
			if c.Role == protocol.ClaimRoleProduce && c.ContainmentDestination != "HOLD-1" {
				t.Errorf("produce claim %s containment = %q, want HOLD-1", c.CoreNodeName, c.ContainmentDestination)
			}
			if c.Role == protocol.ClaimRoleConsume && c.ContainmentDestination != "" {
				t.Errorf("consume claim %s was stamped (%q) — the stamp must never touch swap returns",
					c.CoreNodeName, c.ContainmentDestination)
			}
		}
	}

	// The derived read agrees.
	enabled, dest, err := svc.ContainmentState(pid)
	testutil.MustNoErr(t, err, "containment state")
	if !enabled || dest != "HOLD-1" {
		t.Errorf("state = (%v, %q), want (true, HOLD-1)", enabled, dest)
	}

	// Replay is a no-op (already there).
	testutil.MustNoErr(t, svc.SetContainment(pid, true, "HOLD-1", "tester"), "replay")

	// Clear: the route goes from every produce claim; the read flips off.
	testutil.MustNoErr(t, svc.SetContainment(pid, false, "", "tester"), "clear")
	enabled, dest, err = svc.ContainmentState(pid)
	testutil.MustNoErr(t, err, "containment state after clear")
	if enabled || dest != "" {
		t.Errorf("state after clear = (%v, %q), want (false, \"\")", enabled, dest)
	}
	claims, err := db.ListStyleNodeClaims(styleA)
	testutil.MustNoErr(t, err, "list claims after clear")
	for _, c := range claims {
		if c.ContainmentDestination != "" {
			t.Errorf("claim %s still carries %q after clear", c.CoreNodeName, c.ContainmentDestination)
		}
	}
}
