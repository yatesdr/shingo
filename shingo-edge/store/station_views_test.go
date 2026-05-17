package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol/testutil"
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

	processID, err := d.CreateProcess("SWAP-PROC", "swap test", "active_production", "", "", false, false)
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
	testutil.MustNoErr(t, d.SetActiveStyle(processID, &styleID), "set active style")
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
	testutil.MustNoErr(t, d.SetProcessNodeRuntime(nodeID, &cID, 0), "set runtime")
	testutil.MustNoErr(t, d.UpdateProcessNodeRuntimeOrders(nodeID, &aID, &bID), "update runtime orders")
	// Sibling-link the pair — mirrors what every site that creates a
	// two-robot pair does in production (operator_stations.go:134,
	// operator_bin_ops.go, operator_produce.go, changeover_applier.go,
	// wiring_status_changed.go). ComputeSwapReady's order-graph predicate
	// requires this pointer; tests that exercise the happy path need it
	// set the same way real flows would.
	testutil.MustNoErr(t, d.LinkOrderSiblings(aID, bID), "link siblings")
	rt, err := d.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("get runtime: %v", err)
	}

	return d, gotClaim, rt, aID, bID
}

func TestComputeSwapReady_BothStaged(t *testing.T) {
	t.Parallel()
	db, claim, runtime, aID, bID := seedSwapReadyFixture(t)
	testutil.MustNoErr(t, db.UpdateOrderStatus(aID, "staged"), "mark A staged")
	testutil.MustNoErr(t, db.UpdateOrderStatus(bID, "staged"), "mark B staged")
	if !ComputeSwapReady(db, claim, runtime, nil) {
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
	t.Parallel()
	db, claim, runtime, aID, bID := seedSwapReadyFixture(t)

	// B (StagedOrderID, lineside robot) staged; A (ActiveOrderID) in_transit.
	// New contract: SwapReady=true — the gating leg is parked.
	testutil.MustNoErr(t, db.UpdateOrderStatus(aID, "in_transit"), "mark A in_transit")
	testutil.MustNoErr(t, db.UpdateOrderStatus(bID, "staged"), "mark B staged")
	if !ComputeSwapReady(db, claim, runtime, nil) {
		t.Error("expected SwapReady=true when StagedOrderID (B, lineside robot) is at staged — the new gating signal")
	}

	// Inverse: A staged, B in_transit. Under the new contract this returns
	// false because B isn't the parked one. The operator should wait for B.
	testutil.MustNoErr(t, db.UpdateOrderStatus(aID, "staged"), "mark A staged")
	testutil.MustNoErr(t, db.UpdateOrderStatus(bID, "in_transit"), "mark B in_transit")
	if ComputeSwapReady(db, claim, runtime, nil) {
		t.Error("expected SwapReady=false when only ActiveOrderID (A) is staged and B has not yet arrived — B is the gate, not A")
	}
}

// TestComputeSwapReady_OneStagedOneTerminal ensures the relaxation does NOT
// fire when the non-staged leg has gone terminal (confirmed/failed/cancelled).
// The cycle is over at that point and the consolidated path shouldn't appear.
func TestComputeSwapReady_OneStagedOneTerminal(t *testing.T) {
	t.Parallel()
	for _, terminalStatus := range []string{"confirmed", "failed", "cancelled"} {
		t.Run(terminalStatus, func(t *testing.T) {
			db, claim, runtime, aID, bID := seedSwapReadyFixture(t)
			testutil.MustNoErr(t, db.UpdateOrderStatus(aID, "staged"), "mark A staged")
			if err := db.UpdateOrderStatus(bID, terminalStatus); err != nil {
				t.Fatalf("mark B %s: %v", terminalStatus, err)
			}
			if ComputeSwapReady(db, claim, runtime, nil) {
				t.Errorf("expected SwapReady=false when sibling is terminal (%s) — cycle is over", terminalStatus)
			}
		})
	}
}

func TestComputeSwapReady_NonTwoRobotClaim(t *testing.T) {
	t.Parallel()
	db, claim, runtime, aID, bID := seedSwapReadyFixture(t)
	testutil.MustNoErr(t, db.UpdateOrderStatus(aID, "staged"), "mark A staged")
	testutil.MustNoErr(t, db.UpdateOrderStatus(bID, "staged"), "mark B staged")
	// Flip the claim mode — SwapReady should only fire for two_robot swaps.
	claim.SwapMode = "single_robot"
	if ComputeSwapReady(db, claim, runtime, nil) {
		t.Error("expected SwapReady=false for single_robot claim")
	}
}

func TestComputeSwapReady_NilClaim(t *testing.T) {
	t.Parallel()
	db, _, runtime, _, _ := seedSwapReadyFixture(t)
	if ComputeSwapReady(db, nil, runtime, nil) {
		t.Error("expected SwapReady=false when claim is nil")
	}
}

func TestComputeSwapReady_MissingRuntimeOrders(t *testing.T) {
	t.Parallel()
	db, claim, _, _, _ := seedSwapReadyFixture(t)
	// Runtime with no tracked orders.
	empty := &processes.RuntimeState{}
	if ComputeSwapReady(db, claim, empty, nil) {
		t.Error("expected SwapReady=false when runtime has no tracked orders")
	}
}

// Plant 2026-05-11 (SNF2 ALN_001): both runtime pointers were nil at release
// time but the changeover node task still pointed at the evac order, which
// was at staged in the DB. ComputeSwapReady must fall back to the task's
// durable OldMaterialReleaseOrderID so the operator gets RELEASE instead of
// being parked on WAITING FOR OTHER ROBOT with no escape.
func TestComputeSwapReady_TaskFallbackWhenRuntimePointersNil(t *testing.T) {
	t.Parallel()
	db, claim, _, _, bID := seedSwapReadyFixture(t)
	testutil.MustNoErr(t, db.UpdateOrderStatus(bID, "staged"), "mark B staged")
	// Runtime with no tracked orders — simulates handler_bin_picked_up or
	// other clears that nulled both ActiveOrderID and StagedOrderID.
	empty := &processes.RuntimeState{}
	// Node task with OldMaterialReleaseOrderID pointing at the evac (B).
	// The planner stamps this at order-creation time; it's the durable
	// pointer that survives runtime mutations.
	task := &processes.NodeTask{OldMaterialReleaseOrderID: &bID}
	if !ComputeSwapReady(db, claim, empty, task) {
		t.Error("expected SwapReady=true via task.OldMaterialReleaseOrderID fallback when both runtime pointers are nil")
	}
}

// Symmetric guard: task fallback must still require B at staged. If the
// task points at the evac but the evac isn't actually parked yet, no release.
func TestComputeSwapReady_TaskFallbackRequiresStaged(t *testing.T) {
	t.Parallel()
	db, claim, _, _, bID := seedSwapReadyFixture(t)
	testutil.MustNoErr(t, db.UpdateOrderStatus(bID, "in_transit"), "mark B in_transit")
	empty := &processes.RuntimeState{}
	task := &processes.NodeTask{OldMaterialReleaseOrderID: &bID}
	if ComputeSwapReady(db, claim, empty, task) {
		t.Error("expected SwapReady=false when task-fallback evac is not yet at staged")
	}
}

// Drops are single-leg by construction: changeover_applier.go's
// applyNodeAction skips LinkOrderSiblings when supplyID is nil, and
// drops only create the evac order (changeover_planner.go SituationDrop).
// So the drop's evac order has no sibling pointer, and ComputeSwapReady's
// order-graph predicate returns false naturally — no per-situation
// guard needed.
//
// Plant 2026-05-11 (SNF2 ALN_002): drop on a node whose from-claim
// inherited SwapMode=two_robot. Pre-refactor ComputeSwapReady would
// resolve the staged drop order via the task-fallback (or runtime
// pointers depending on cycle state) and report swap_ready=true,
// steering the modal to /release-staged which rejected with "no
// tracked orders to release". Post-refactor: evac.SiblingOrderID==nil
// short-circuits before the staged check.
func TestComputeSwapReady_DropHasNoSibling_ViaTaskPointer(t *testing.T) {
	t.Parallel()
	db, claim, _, _, bID := seedSwapReadyFixture(t)
	testutil.MustNoErr(t, db.UpdateOrderStatus(bID, "staged"), "mark B staged")
	// Simulate a drop: clear the sibling pointer that the fixture set
	// up for the standard two-robot pair. A real drop never gets one
	// because the applier skips LinkOrderSiblings when only the evac
	// order is created.
	testutil.MustNoErr(t, db.ClearOrderSibling(bID), "clear sibling")
	empty := &processes.RuntimeState{}
	dropTask := &processes.NodeTask{
		OldMaterialReleaseOrderID: &bID,
		Situation:                 "drop",
	}
	if ComputeSwapReady(db, claim, empty, dropTask) {
		t.Error("expected SwapReady=false for drop via task-pointer fallback when evac has no sibling")
	}
}

// Same shape via the runtime-pointer path. Stale runtime state from a
// prior cycle (or a tool-change staging that touched this node) can
// leave runtime.StagedOrderID populated when the current task is a
// drop. The order-graph predicate handles this without needing a
// Situation check.
func TestComputeSwapReady_DropHasNoSibling_ViaRuntimePointer(t *testing.T) {
	t.Parallel()
	db, claim, runtime, _, bID := seedSwapReadyFixture(t)
	testutil.MustNoErr(t, db.UpdateOrderStatus(bID, "staged"), "mark B staged")
	testutil.MustNoErr(t, db.ClearOrderSibling(bID), "clear sibling")
	dropTask := &processes.NodeTask{Situation: "drop"}
	if ComputeSwapReady(db, claim, runtime, dropTask) {
		t.Error("expected SwapReady=false when runtime points at an order without a sibling (drop on a two-robot-mode node)")
	}
}

// Pin the structural failure mode the refactor introduces: a pair that
// SHOULD have been sibling-linked but wasn't (e.g., LinkOrderSiblings
// silently failed) reads as not-coordinated. This is the cost of
// removing the Situation guards — silent linkage failures become
// operator-visible as WAITING FOR OTHER ROBOT. The three operator-
// initiated sites that create pairs now return-error on linkage
// failure to prevent reaching this state. The two event-loop sites
// stay log-and-continue with a residual risk tracked in SHINGO_TODO.md.
func TestComputeSwapReady_PairWithoutSiblingPointer(t *testing.T) {
	t.Parallel()
	db, claim, runtime, _, bID := seedSwapReadyFixture(t)
	testutil.MustNoErr(t, db.UpdateOrderStatus(bID, "staged"), "mark B staged")
	// Simulate a silent LinkOrderSiblings failure: clear the pointer
	// the fixture set.
	testutil.MustNoErr(t, db.ClearOrderSibling(bID), "clear sibling")
	if ComputeSwapReady(db, claim, runtime, nil) {
		t.Error("expected SwapReady=false when the pair is missing sibling linkage (silent LinkOrderSiblings failure)")
	}
}

// Both runtime pointers populated → walk no siblings, return as-is.
func TestResolveSwapPair_RuntimeBothPresent(t *testing.T) {
	t.Parallel()
	db, _, runtime, aID, bID := seedSwapReadyFixture(t)
	evac, supply, err := ResolveSwapPair(db, runtime, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if evac == nil || *evac != bID {
		t.Errorf("expected evac=%d, got %v", bID, evac)
	}
	if supply == nil || *supply != aID {
		t.Errorf("expected supply=%d, got %v", aID, supply)
	}
}

// StagedOrderID nil but ActiveOrderID's sibling resolves the evac.
// Mirrors handler_bin_picked_up clearing StagedOrderID while ActiveOrderID
// survives.
func TestResolveSwapPair_RuntimeStagedNilSiblingWalk(t *testing.T) {
	t.Parallel()
	db, _, _, aID, bID := seedSwapReadyFixture(t)
	rt := &processes.RuntimeState{ActiveOrderID: &aID}
	evac, supply, err := ResolveSwapPair(db, rt, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if evac == nil || *evac != bID {
		t.Errorf("expected evac=%d via sibling walk, got %v", bID, evac)
	}
	if supply == nil || *supply != aID {
		t.Errorf("expected supply=%d, got %v", aID, supply)
	}
}

// Plant 2026-05-11 (SNF2 ALN_001): both runtime pointers nil. Pre-fix
// resolveSwapPair in engine/operator_stations.go rejected with "no
// tracked orders to release" while ComputeSwapReady on the HMI happily
// fell back to task.OldMaterialReleaseOrderID and rendered RELEASE.
// ResolveSwapPair now uses the same task fallback so both sides agree.
func TestResolveSwapPair_TaskFallbackWhenRuntimePointersNil(t *testing.T) {
	t.Parallel()
	db, _, _, aID, bID := seedSwapReadyFixture(t)
	empty := &processes.RuntimeState{}
	task := &processes.NodeTask{OldMaterialReleaseOrderID: &bID}
	evac, supply, err := ResolveSwapPair(db, empty, task)
	if err != nil {
		t.Fatalf("resolve via task fallback: %v", err)
	}
	if evac == nil || *evac != bID {
		t.Errorf("expected evac=%d via task fallback, got %v", bID, evac)
	}
	// Supply walked from evac's sibling pointer.
	if supply == nil || *supply != aID {
		t.Errorf("expected supply=%d via sibling walk from task-fallback evac, got %v", aID, supply)
	}
}

// No runtime, no task → "no tracked orders to release". The render-side
// equivalent: ComputeSwapReady returns false without erroring.
func TestResolveSwapPair_EmptyRuntimeAndTask(t *testing.T) {
	t.Parallel()
	db, _, _, _, _ := seedSwapReadyFixture(t)
	_, _, err := ResolveSwapPair(db, &processes.RuntimeState{}, nil)
	if err == nil {
		t.Error("expected error when both runtime and task are empty")
	}
}

// Task fallback hits a drop (single-leg, no sibling). Reject rather
// than partial-release. The HMI's ComputeSwapReady would have already
// short-circuited via the sibling check, so this is defense-in-depth.
func TestResolveSwapPair_TaskFallbackSingleLegRejected(t *testing.T) {
	t.Parallel()
	db, _, _, _, bID := seedSwapReadyFixture(t)
	testutil.MustNoErr(t, db.ClearOrderSibling(bID), "clear sibling")
	task := &processes.NodeTask{OldMaterialReleaseOrderID: &bID, Situation: "drop"}
	_, _, err := ResolveSwapPair(db, &processes.RuntimeState{}, task)
	if err == nil {
		t.Error("expected single-leg rejection when task-fallback evac has no sibling")
	}
}
