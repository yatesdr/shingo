package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestFindActiveClaim_AddNodeChangeoverFallback pins the 2026-05-12 fix:
// when a changeover adds a brand-new process_node (e.g. 1-node → 2-node
// topology), that node has NO claim on the active (from) style — it
// didn't exist there. Pre-fix, findActiveClaim returned nil and every
// downstream handler (handleNodeOrderDelivered, manual_swap completion,
// counter delta, status changed) silently short-circuited. Plant
// symptom: bin arrived at the new node with correct UOP in Core's
// bins table, but runtime.remaining_uop_cached stayed at 0 because
// SetProcessNodeRuntimeForDeliveredBin never ran. HMI rendered 0.
//
// The fix consults TargetStyleID when ActiveStyleID lookup returns
// nil — only during a changeover (target is non-nil), so existing-node
// flows are unaffected.
func TestFindActiveClaim_AddNodeChangeoverFallback(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, err := db.CreateProcess("ADD-PROC", "add-node test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	fromStyleID, err := db.CreateStyle("ADD-FROM", "from-style", processID)
	if err != nil {
		t.Fatalf("create from-style: %v", err)
	}
	toStyleID, err := db.CreateStyle("ADD-TO", "to-style", processID)
	if err != nil {
		t.Fatalf("create to-style: %v", err)
	}

	// Mid-changeover: active=from, target=to.
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active=from")
	testutil.MustNoErr(t, db.SetTargetStyle(processID, &toStyleID), "set target=to")

	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "ADD-NEW-NODE",
		Code:         "AN1",
		Name:         "Added node",
		Sequence:     2,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	// Claim exists ONLY on to-style — mirrors the add-node topology
	// where the new node didn't exist under from-style.
	toClaimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:      toStyleID,
		CoreNodeName: "ADD-NEW-NODE",
		Role:         protocol.ClaimRoleConsume,
		SwapMode:     protocol.SwapModeSingleRobot,
		PayloadCode:  "PART-ADD",
		UOPCapacity:  3600,
	})
	if err != nil {
		t.Fatalf("upsert to-style claim: %v", err)
	}

	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}

	got := findActiveClaim(db, node)
	if got == nil {
		t.Fatal("findActiveClaim returned nil for add-node during changeover — handlers like handleNodeOrderDelivered would short-circuit and leave runtime.remaining_uop_cached at 0")
	}
	if got.ID != toClaimID {
		t.Errorf("findActiveClaim returned claim %d, want to-style claim %d", got.ID, toClaimID)
	}
}

// TestFindActiveClaim_PrefersActiveOverTarget pins that the fallback
// only fires when the active-style lookup returns nil — never preempts
// an existing active-style claim. Existing-node flows during a
// changeover (where both styles claim the same node, but the from-style
// is the one that should govern until cutover) MUST keep returning
// the from-style claim.
func TestFindActiveClaim_PrefersActiveOverTarget(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _ := db.CreateProcess("PREF-PROC", "prefer-active test", "active_production", "", "", false, false)
	fromStyleID, _ := db.CreateStyle("PREF-FROM", "from", processID)
	toStyleID, _ := db.CreateStyle("PREF-TO", "to", processID)
	db.SetActiveStyle(processID, &fromStyleID)
	db.SetTargetStyle(processID, &toStyleID)

	nodeID, _ := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "PREF-NODE",
		Code:         "PR1",
		Name:         "existing node",
		Sequence:     1,
		Enabled:      true,
	})
	fromClaimID, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "PREF-NODE", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART-OLD", UOPCapacity: 100,
	})
	toClaimID, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "PREF-NODE", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART-NEW", UOPCapacity: 200,
	})

	node, _ := db.GetProcessNode(nodeID)
	got := findActiveClaim(db, node)
	if got == nil {
		t.Fatal("findActiveClaim returned nil despite from-style claim existing")
	}
	if got.ID != fromClaimID {
		t.Errorf("findActiveClaim returned claim %d (want from-style %d, to-style is %d) — fallback must NOT preempt active-style claim",
			got.ID, fromClaimID, toClaimID)
	}
}
