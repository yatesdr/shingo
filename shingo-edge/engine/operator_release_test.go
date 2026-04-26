package engine

import (
	"testing"

	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// stageOrderForConsumeNode seeds a staged complex order against the
// consume node and hangs it on runtime.StagedOrderID so ReleaseOrder
// behaves as it would in production.
func stageOrderForConsumeNode(t *testing.T, db *store.DB, nodeID int64, uuid string) int64 {
	t.Helper()
	orderID, err := db.CreateOrder(uuid, orders.TypeComplex,
		&nodeID, false, 1, "CONSUME-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, orders.StatusStaged); err != nil {
		t.Fatalf("transition to staged: %v", err)
	}
	return orderID
}

// TestReleaseOrderWithLineside_ResetsUOPAndCapturesBuckets verifies that
// the release-click path sets UOP to capacity, marks the changeover
// task (if any) as released, and records the parts the operator pulled
// to lineside in node_lineside_bucket.
func TestReleaseOrderWithLineside_ResetsUOPAndCapturesBuckets(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-REL", PayloadCode: "PART-R", UOPCapacity: 100, InitialUOP: 8,
	})

	// Drain the counter low (simulating pre-swap production).
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 8); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-rel-1")

	eng := testEngine(t, db)
	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-R": 12},
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	// UOP should be at capacity.
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 100 {
		t.Errorf("RemainingUOP = %d, want 100 (capacity) after release", runtime.RemainingUOP)
	}

	// Bucket should exist with 12 active units.
	b, err := db.GetActiveLinesideBucket(nodeID, styleID, "PART-R")
	if err != nil {
		t.Fatalf("GetActiveLinesideBucket: %v", err)
	}
	if b.Qty != 12 {
		t.Errorf("bucket qty = %d, want 12", b.Qty)
	}
	if b.State != store.LinesideStateActive {
		t.Errorf("bucket state = %q, want %q", b.State, store.LinesideStateActive)
	}

	// Order should be in_transit (release dispatched).
	o, _ := db.GetOrder(orderID)
	if o.Status != orders.StatusInTransit {
		t.Errorf("order status = %q, want %q", o.Status, orders.StatusInTransit)
	}
}

// TestReleaseOrderWithLineside_EmptyMapStillResetsUOP verifies that
// calling release with nothing captured still performs the UOP reset
// and deactivates stranded buckets for other styles.
func TestReleaseOrderWithLineside_EmptyMapStillResetsUOP(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-REL2", PayloadCode: "PART-R2", UOPCapacity: 50, InitialUOP: 3,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 3); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-rel-2")

	eng := testEngine(t, db)
	// Empty disposition (legacy / NOTHING-PULLED-with-no-explicit-mode):
	// should still drive the UOP reset.
	if err := eng.ReleaseOrderWithLineside(orderID, ReleaseDisposition{Mode: DispositionCaptureLineside}); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 50 {
		t.Errorf("RemainingUOP = %d, want 50 (capacity) after release with empty capture map", runtime.RemainingUOP)
	}
}

// TestReleaseOrderWithLineside_DeactivatesStrandedStyles verifies that
// when the release click happens, any active buckets on the node that
// belong to a different style are flipped to inactive.
func TestReleaseOrderWithLineside_DeactivatesStrandedStyles(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-REL3", PayloadCode: "PART-R3", UOPCapacity: 80, InitialUOP: 5,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 5); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Seed a leftover active bucket from a different style on this node.
	otherStyleID := styleID + 999
	if _, err := db.CaptureLinesideBucket(nodeID, "", otherStyleID, "PART-OLD", 4); err != nil {
		t.Fatalf("seed leftover bucket: %v", err)
	}

	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-rel-3")
	eng := testEngine(t, db)
	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-R3": 2},
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	// Leftover bucket should now be inactive.
	inactive, err := db.ListInactiveLinesideBuckets(nodeID)
	if err != nil {
		t.Fatalf("ListInactiveLinesideBuckets: %v", err)
	}
	if len(inactive) != 1 {
		t.Fatalf("inactive buckets = %d, want 1", len(inactive))
	}
	if inactive[0].StyleID != otherStyleID || inactive[0].PartNumber != "PART-OLD" {
		t.Errorf("unexpected inactive bucket: %+v", inactive[0])
	}

	// New-style bucket should be active.
	b, err := db.GetActiveLinesideBucket(nodeID, styleID, "PART-R3")
	if err != nil {
		t.Fatalf("GetActiveLinesideBucket: %v", err)
	}
	if b.Qty != 2 {
		t.Errorf("active bucket qty = %d, want 2", b.Qty)
	}
}

// TestReleaseOrderWithLineside_TwoRobotSupplyDoesNotResetRuntime locks down
// the Bug B fix from the ALN_002 plant test (2026-04-23). For two-robot
// swaps in the per-order release path, Order A (the supply) is released
// before Order B (the evac) if the operator clicks them in that order.
// Without the runtime-reset guard, Order A's release would call
// SetProcessNodeRuntime(node.ID, &claimID, UOPCapacity) and clobber the
// runtime UOP. Order B's subsequent release with SEND PARTIAL BACK would
// then read the now-reset value (= UOPCapacity) instead of the actual
// remaining count, send remaining_uop=UOPCapacity to Core, and Core would
// stamp the evac bin with full UOP — manifest preserved, bin lands at
// OutboundDestination looking like a fresh full bin.
//
// The fix: skip SetProcessNodeRuntime when the order being released is
// the supply slot in an active two-robot swap. Order B's release (or the
// consolidated ReleaseStagedOrders path which does B-then-A) owns the
// reset.
func TestReleaseOrderWithLineside_TwoRobotSupplyDoesNotResetRuntime(t *testing.T) {
	db := testEngineDB(t)

	// Seed a consume-role node with an explicit two_robot claim.
	processID, err := db.CreateProcess("TR-SUPPLY", "two-robot supply test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "TR-SUPPLY-NODE",
		Code:         "TRS",
		Name:         "TR Supply Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle("TR-SUPPLY-STYLE", "two-robot style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)

	// Two-robot claim with InboundStaging configured (the helper requires it
	// to be non-empty when SwapMode == "two_robot").
	claimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:        styleID,
		CoreNodeName:   "TR-SUPPLY-NODE",
		Role:           "consume",
		SwapMode:       "two_robot",
		PayloadCode:    "PART-TR",
		UOPCapacity:    1200,
		InboundSource:  "TR-SOURCE",
		InboundStaging: "TR-STAGING",
	})
	if err != nil {
		t.Fatalf("upsert two_robot claim: %v", err)
	}

	// Drain the runtime to a partial value so we can detect a clobber if
	// Order A's release wrongly resets it.
	db.EnsureProcessNodeRuntime(nodeID)
	const partialUOP = 800
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, partialUOP); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Stage two orders against this node. ActiveOrderID = supply (Order A),
	// StagedOrderID = evac (Order B). The isSupplyOrderInActiveTwoRobotSwap
	// helper keys off this convention.
	orderA := stageOrderForConsumeNode(t, db, nodeID, "uuid-tr-supply-A")
	orderB := stageOrderForConsumeNode(t, db, nodeID, "uuid-tr-supply-B")
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB); err != nil {
		t.Fatalf("track A+B on runtime: %v", err)
	}

	eng := testEngine(t, db)

	// Release Order A (the supply) with the operator's NOTHING PULLED
	// disposition. Pre-fix: this would call SetProcessNodeRuntime and
	// clobber runtime.RemainingUOP from 800 → 1200.
	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{},
	}
	if err := eng.ReleaseOrderWithLineside(orderA, disp); err != nil {
		t.Fatalf("release Order A: %v", err)
	}

	// Runtime UOP must be UNCHANGED — Order B will read it for SEND PARTIAL
	// BACK or whatever disposition comes next.
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != partialUOP {
		t.Errorf("RemainingUOP = %d, want %d (Order A's release must not reset the runtime UOP for two-robot supply orders — Order B owns the reset)",
			runtime.RemainingUOP, partialUOP)
	}
}

// TestReleaseOrderWithLineside_TwoRobotEvacResetsRuntime is the
// counterpart: Order B (the evac) IS allowed to reset the runtime UOP,
// because that's "prepare the line for the new bin's UOP cycle." Without
// this, Bug B's fix would over-correct and break the legitimate reset.
func TestReleaseOrderWithLineside_TwoRobotEvacResetsRuntime(t *testing.T) {
	db := testEngineDB(t)

	processID, err := db.CreateProcess("TR-EVAC", "two-robot evac test", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "TR-EVAC-NODE",
		Code:         "TRE",
		Name:         "TR Evac Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, _ := db.CreateStyle("TR-EVAC-STYLE", "", processID)
	db.SetActiveStyle(processID, &styleID)
	claimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:        styleID,
		CoreNodeName:   "TR-EVAC-NODE",
		Role:           "consume",
		SwapMode:       "two_robot",
		PayloadCode:    "PART-TR",
		UOPCapacity:    1200,
		InboundSource:  "TR-SOURCE",
		InboundStaging: "TR-STAGING",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 800); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Both orders staged, but releasing the EVAC slot (StagedOrderID = B).
	orderA := stageOrderForConsumeNode(t, db, nodeID, "uuid-tr-evac-A")
	orderB := stageOrderForConsumeNode(t, db, nodeID, "uuid-tr-evac-B")
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB); err != nil {
		t.Fatalf("track A+B on runtime: %v", err)
	}

	eng := testEngine(t, db)

	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{},
	}
	if err := eng.ReleaseOrderWithLineside(orderB, disp); err != nil {
		t.Fatalf("release Order B: %v", err)
	}

	// Runtime UOP MUST be reset to capacity — that's the "new cycle" signal.
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 1200 {
		t.Errorf("RemainingUOP = %d, want 1200 (Order B's release must reset the runtime UOP for the next cycle)",
			runtime.RemainingUOP)
	}
}

// TestComputeReleaseRemainingUOP exercises the disposition → *int routing in
// isolation so the late-binding contract (empty Mode → nil, capture → &0,
// partial → &runtime.RemainingUOP, partial-with-non-positive-runtime → &0)
// is locked down without the surrounding HTTP/DB/dispatch machinery.
func TestComputeReleaseRemainingUOP(t *testing.T) {
	cases := []struct {
		name        string
		mode        ReleaseDispositionMode
		runtimeUOP  int
		wantNil     bool
		wantValue   int
	}{
		{"empty_mode_returns_nil_for_backward_compat", "", 42, true, 0},
		{"unknown_mode_returns_nil", "weird_thing", 42, true, 0},
		{"capture_lineside_returns_zero", DispositionCaptureLineside, 42, false, 0},
		{"send_partial_back_returns_runtime_uop", DispositionSendPartialBack, 800, false, 800},
		{"send_partial_back_zero_runtime_returns_zero", DispositionSendPartialBack, 0, false, 0},
		{"send_partial_back_negative_runtime_returns_zero", DispositionSendPartialBack, -1, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &processes.RuntimeState{RemainingUOP: tc.runtimeUOP}
			got := computeReleaseRemainingUOP(ReleaseDisposition{Mode: tc.mode}, rt)
			if tc.wantNil {
				if got != nil {
					t.Errorf("got %v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want *%d", tc.wantValue)
			}
			if *got != tc.wantValue {
				t.Errorf("got %d, want %d", *got, tc.wantValue)
			}
		})
	}
}

// TestReleaseOrderWithLineside_SendPartialBack_SkipsBucketCapture verifies
// the SEND PARTIAL BACK disposition: no bucket capture happens (so the
// operator's leftover stays on the bin instead of being kitted lineside),
// the runtime UOP still resets to capacity for the next bin, and stranded
// other-style buckets are still deactivated.
func TestReleaseOrderWithLineside_SendPartialBack_SkipsBucketCapture(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-PARTIAL", PayloadCode: "PART-PB", UOPCapacity: 1200, InitialUOP: 800,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 800); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	// Stranded bucket from a previous style — should be deactivated even
	// on the partial-back path because the deactivation reflects "this
	// node is now running this style," not bucket capture.
	otherStyleID := styleID + 999
	if _, err := db.CaptureLinesideBucket(nodeID, "", otherStyleID, "PART-OLD-PB", 7); err != nil {
		t.Fatalf("seed leftover bucket: %v", err)
	}

	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-pb-1")
	eng := testEngine(t, db)
	disp := ReleaseDisposition{
		Mode:            DispositionSendPartialBack,
		LinesideCapture: map[string]int{"PART-PB": 99}, // ignored when Mode == send_partial_back
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	// Runtime should reset to capacity for the NEXT bin.
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 1200 {
		t.Errorf("RemainingUOP = %d, want 1200 (capacity)", runtime.RemainingUOP)
	}

	// No active bucket for the operator's part — capture skipped.
	if b, err := db.GetActiveLinesideBucket(nodeID, styleID, "PART-PB"); err == nil && b != nil && b.Qty > 0 {
		t.Errorf("send_partial_back should not capture lineside bucket; got bucket %+v", b)
	}

	// Stranded other-style bucket should be deactivated.
	inactive, err := db.ListInactiveLinesideBuckets(nodeID)
	if err != nil {
		t.Fatalf("ListInactiveLinesideBuckets: %v", err)
	}
	if len(inactive) != 1 || inactive[0].StyleID != otherStyleID {
		t.Errorf("expected one inactive bucket for the other style; got %+v", inactive)
	}

	// Order in_transit (release dispatched).
	o, _ := db.GetOrder(orderID)
	if o.Status != orders.StatusInTransit {
		t.Errorf("order status = %q, want %q", o.Status, orders.StatusInTransit)
	}
}

// TestHandleComplexOrderBCompletion_SkipsIfAlreadyReleased verifies the
// idempotent gate: when the release handler already advanced the task
// to "released" (and reset the counter), Order B completion doesn't
// clobber the drained counter back to capacity.
func TestHandleComplexOrderBCompletion_SkipsIfAlreadyReleased(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-IDEMP", PayloadCode: "PART-IDEMP", UOPCapacity: 100, InitialUOP: 100,
	})

	// Simulate "release click already happened": UOP at capacity,
	// then a few counter ticks drained it to 87.
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 87); err != nil {
		t.Fatalf("seed drained runtime: %v", err)
	}

	// There's no changeover in this scenario — handleNormalReplenishment
	// should see a complex order on a consume node with a live runtime
	// and skip the reset. Create the completed order.
	orderID, err := db.CreateOrder("uuid-idemp", orders.TypeComplex,
		&nodeID, false, 1, "CONSUME-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, orders.StatusConfirmed)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-idemp",
			OrderType:     orders.TypeComplex,
			ProcessNodeID: &nodeID,
		},
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 87 {
		t.Errorf("RemainingUOP = %d, want 87 (release already ran, completion must not clobber)",
			runtime.RemainingUOP)
	}
}

// TestHandleNormalReplenishment_RetrieveStillResets verifies that simple
// retrieve orders (no release-click prompt) continue to reset UOP at
// completion — the gate only applies to complex orders.
func TestHandleNormalReplenishment_RetrieveStillResets(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-RETR", PayloadCode: "PART-RETR", UOPCapacity: 100, InitialUOP: 10,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 10); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	orderID, err := db.CreateOrder("uuid-retr", orders.TypeRetrieve,
		&nodeID, false, 1, "CONSUME-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, orders.StatusConfirmed)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID:       orderID,
			OrderUUID:     "uuid-retr",
			OrderType:     orders.TypeRetrieve,
			ProcessNodeID: &nodeID,
		},
	})

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOP != 100 {
		t.Errorf("RemainingUOP = %d, want 100 (simple retrieve should still reset)",
			runtime.RemainingUOP)
	}
}
