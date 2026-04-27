package store

import (
	"path/filepath"
	"testing"

	"shingoedge/store/processes"
)

// seedSwapReadyFixture wires up the minimum rows needed to exercise
// ComputeSwapReady: a process with a node, a style with a two_robot claim,
// two orders whose IDs we track on the runtime, and a runtime row pointing
// at them. It returns the DB handle plus the bits the caller may want to
// mutate (claim, runtime, order A id, order B id).
func seedSwapReadyFixture(t *testing.T) (db *DB, claim *processes.NodeClaim, runtime *processes.RuntimeState, orderA, orderB int64) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sv.db")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	processID, err := d.CreateProcess("SWAP-PROC", "swap test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := d.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "SWAP-NODE",
		Code:         "SN1",
		Name:         "Swap Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := d.CreateStyle("SWAP-STYLE", "swap style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := d.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	claimID, err := d.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        "SWAP-NODE",
		Role:                "produce",
		SwapMode:            "two_robot",
		PayloadCode:         "WIDGET",
		UOPCapacity:         100,
		InboundStaging:      "SWAP-IN-STAGING",
		OutboundStaging:     "SWAP-OUT-STAGING",
		OutboundDestination: "SWAP-OUT",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	gotClaim, err := d.GetStyleNodeClaimByNode(styleID, "SWAP-NODE")
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}

	aID, err := d.CreateOrder("uuid-a", "complex", &nodeID, false, 1, "", "", "", "", false, "WIDGET")
	if err != nil {
		t.Fatalf("create order A: %v", err)
	}
	bID, err := d.CreateOrder("uuid-b", "complex", &nodeID, false, 1, "", "", "", "", false, "WIDGET")
	if err != nil {
		t.Fatalf("create order B: %v", err)
	}

	if _, err := d.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	cID := claimID
	if err := d.SetProcessNodeRuntime(nodeID, &cID, 0); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	if err := d.UpdateProcessNodeRuntimeOrders(nodeID, &aID, &bID); err != nil {
		t.Fatalf("update runtime orders: %v", err)
	}
	rt, err := d.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}

	return d, gotClaim, rt, aID, bID
}

func TestComputeSwapReady_BothStaged(t *testing.T) {
	db, claim, runtime, aID, bID := seedSwapReadyFixture(t)
	if err := db.UpdateOrderStatus(aID, "staged"); err != nil {
		t.Fatalf("mark A staged: %v", err)
	}
	if err := db.UpdateOrderStatus(bID, "staged"); err != nil {
		t.Fatalf("mark B staged: %v", err)
	}
	if !ComputeSwapReady(db, claim, runtime) {
		t.Error("expected SwapReady=true when both orders staged")
	}
}

// TestComputeSwapReady_OnlyOneStaged covers the post-2026-04-27 contract:
// SwapReady tracks ONLY the StagedOrderID (Robot B, the lineside removal
// robot). Order A's status is irrelevant — the consolidated release fans
// out to both legs regardless. So when B is staged and A is in_transit (or
// in any non-terminal status), SwapReady is true. Conversely, when A is
// staged but B is in_transit (the inverse of the old contract's symmetric
// "at least one staged" rule), SwapReady is false because B isn't parked
// at the line yet.
func TestComputeSwapReady_OnlyOneStaged(t *testing.T) {
	db, claim, runtime, aID, bID := seedSwapReadyFixture(t)

	// B (StagedOrderID, lineside robot) staged; A (ActiveOrderID) in_transit.
	// New contract: SwapReady=true — the gating leg is parked.
	if err := db.UpdateOrderStatus(aID, "in_transit"); err != nil {
		t.Fatalf("mark A in_transit: %v", err)
	}
	if err := db.UpdateOrderStatus(bID, "staged"); err != nil {
		t.Fatalf("mark B staged: %v", err)
	}
	if !ComputeSwapReady(db, claim, runtime) {
		t.Error("expected SwapReady=true when StagedOrderID (B, lineside robot) is at staged — the new gating signal")
	}

	// Inverse: A staged, B in_transit. Under the new contract this returns
	// false because B isn't the parked one. The operator should wait for B.
	if err := db.UpdateOrderStatus(aID, "staged"); err != nil {
		t.Fatalf("mark A staged: %v", err)
	}
	if err := db.UpdateOrderStatus(bID, "in_transit"); err != nil {
		t.Fatalf("mark B in_transit: %v", err)
	}
	if ComputeSwapReady(db, claim, runtime) {
		t.Error("expected SwapReady=false when only ActiveOrderID (A) is staged and B has not yet arrived — B is the gate, not A")
	}
}

// TestComputeSwapReady_OneStagedOneTerminal ensures the relaxation does NOT
// fire when the non-staged leg has gone terminal (confirmed/failed/cancelled).
// The cycle is over at that point and the consolidated path shouldn't appear.
func TestComputeSwapReady_OneStagedOneTerminal(t *testing.T) {
	for _, terminalStatus := range []string{"confirmed", "failed", "cancelled"} {
		t.Run(terminalStatus, func(t *testing.T) {
			db, claim, runtime, aID, bID := seedSwapReadyFixture(t)
			if err := db.UpdateOrderStatus(aID, "staged"); err != nil {
				t.Fatalf("mark A staged: %v", err)
			}
			if err := db.UpdateOrderStatus(bID, terminalStatus); err != nil {
				t.Fatalf("mark B %s: %v", terminalStatus, err)
			}
			if ComputeSwapReady(db, claim, runtime) {
				t.Errorf("expected SwapReady=false when sibling is terminal (%s) — cycle is over", terminalStatus)
			}
		})
	}
}

func TestComputeSwapReady_NonTwoRobotClaim(t *testing.T) {
	db, claim, runtime, aID, bID := seedSwapReadyFixture(t)
	if err := db.UpdateOrderStatus(aID, "staged"); err != nil {
		t.Fatalf("mark A staged: %v", err)
	}
	if err := db.UpdateOrderStatus(bID, "staged"); err != nil {
		t.Fatalf("mark B staged: %v", err)
	}
	// Flip the claim mode — SwapReady should only fire for two_robot swaps.
	claim.SwapMode = "single_robot"
	if ComputeSwapReady(db, claim, runtime) {
		t.Error("expected SwapReady=false for single_robot claim")
	}
}

func TestComputeSwapReady_NilClaim(t *testing.T) {
	db, _, runtime, _, _ := seedSwapReadyFixture(t)
	if ComputeSwapReady(db, nil, runtime) {
		t.Error("expected SwapReady=false when claim is nil")
	}
}

func TestComputeSwapReady_MissingRuntimeOrders(t *testing.T) {
	db, claim, _, _, _ := seedSwapReadyFixture(t)
	// Runtime with no tracked orders.
	empty := &processes.RuntimeState{}
	if ComputeSwapReady(db, claim, empty) {
		t.Error("expected SwapReady=false when runtime has no tracked orders")
	}
}
