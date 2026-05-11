package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// stageOrderForConsumeNode seeds a staged complex order against the
// consume node and hangs it on runtime.StagedOrderID so ReleaseOrder
// behaves as it would in production. delivery_node is set to the
// node's actual CoreNodeName so the order is recognizable as a supply
// leg (delivers AT the slot) by the durable supply guard.
func stageOrderForConsumeNode(t *testing.T, db *store.DB, nodeID int64, uuid string) int64 {
	t.Helper()
	node, err := db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("get process node %d: %v", nodeID, err)
	}
	orderID, err := db.CreateOrder(uuid, orders.TypeComplex,
		&nodeID, false, 1, node.CoreNodeName, "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("transition to staged: %v", err)
	}
	return orderID
}

// TestReleaseOrderWithLineside_ZeroesUOPAndCapturesBuckets verifies the
// release-click contract (Stephen 2026-05-04 SME correction): the slot's
// runtime UOP zeroes immediately at release click — the bin is leaving,
// the slot has no count attributed until the new bin lands. The cycle's
// other firing point (new bin dropped) flips the count back to capacity
// via SetProcessNodeRuntimeWithBin at delivery completion.
func TestReleaseOrderWithLineside_ZeroesUOPAndCapturesBuckets(t *testing.T) {
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

	// UOP must zero at release — bin is leaving, no count attributed
	// to the slot until the new bin drops (which flips it to capacity).
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 (release zeroes the slot; new-bin-drop flips to capacity)", runtime.RemainingUOPCached)
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

// TestReleaseOrderWithLineside_EmptyMapZeroesUOP verifies that release
// with no captures (RELEASE EMPTY semantics) zeroes the slot at release
// click and deactivates stranded buckets for other styles.
func TestReleaseOrderWithLineside_EmptyMapZeroesUOP(t *testing.T) {
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
	// must not touch runtime UOP.
	if err := eng.ReleaseOrderWithLineside(orderID, ReleaseDisposition{Mode: DispositionCaptureLineside}); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 (release zeroes the slot; new-bin-drop flips to capacity)", runtime.RemainingUOPCached)
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


// TestComputeReleaseRemainingUOP exercises the disposition → *int routing in
// isolation so the late-binding contract (empty Mode → nil, capture → &0,
// partial → &runtime.RemainingUOPCached, partial-with-non-positive-runtime → &0)
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
			rt := &processes.RuntimeState{RemainingUOPCached: tc.runtimeUOP}
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
// runtime UOP is preserved (delivery completion will reset, not release),
// and stranded other-style buckets are still deactivated.
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

	// Runtime UOP zeroes at release — the partial bin is leaving the
	// slot. The wire-shape RemainingUOP=&runtime preserves the partial
	// count for Core's manifest, but the slot's local cache zeros.
	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOP = %d, want 0 (release zeroes the slot)",
			runtime.RemainingUOPCached)
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

// TestHandleComplexOrderBCompletion_ResetsOnDelivery locks down the new
// contract: the runtime UOP turnover happens on delivery completion, not at
// release click. Even if the operator drained the counter between release
// and arrival, completion resets to capacity because that's when the new
// bin is physically present.
func TestHandleComplexOrderBCompletion_ResetsOnDelivery(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-IDEMP", PayloadCode: "PART-IDEMP", UOPCapacity: 100, InitialUOP: 100,
	})

	// Simulate counter drained to 87 (any value < capacity) before delivery.
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 87); err != nil {
		t.Fatalf("seed drained runtime: %v", err)
	}

	// DeliveryNode must equal the seeded node's CoreNodeName for
	// binArrivingAt to fire. BinID set so resolveReplenishUOP returns
	// claim capacity.
	orderID, err := db.CreateOrder("uuid-idemp", orders.TypeComplex,
		&nodeID, false, 1, "LSD-IDEMP-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed))
	deliveredBin := int64(404)
	db.UpdateOrderBinID(orderID, &deliveredBin)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	emitOrderCompleted(eng, orderID, "uuid-idemp", orders.TypeComplex, &nodeID)

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOPCached != 100 {
		t.Errorf("RemainingUOP = %d, want 100 (delivered handler fallback to claim capacity)",
			runtime.RemainingUOPCached)
	}
}

// seedManualSwapClaim creates a separate process holding a manual_swap claim
// (loader or unloader) keyed off the supplied payload code. Used by the
// side-cycle trigger tests to stand up a downstream loader/unloader that the
// LINE's release should fan out to.
func seedManualSwapClaim(t *testing.T, db *store.DB, prefix string, role protocol.ClaimRole, payloadCode, outbound string) (nodeID, claimID int64) {
	t.Helper()
	processID, err := db.CreateProcess(prefix+"-PROC", prefix+" mswap", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create mswap process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: prefix + "-MSWAP-NODE",
		Code:         prefix[:3],
		Name:         prefix + " mswap",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create mswap node: %v", err)
	}
	styleID, err := db.CreateStyle(prefix+"-MSWAP-STYLE", prefix+" mswap", processID)
	if err != nil {
		t.Fatalf("create mswap style: %v", err)
	}
	db.SetActiveStyle(processID, &styleID)

	claimID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        prefix + "-MSWAP-NODE",
		Role:                role,
		SwapMode:            "manual_swap",
		PayloadCode:         payloadCode,
		UOPCapacity:         100,
		OutboundDestination: outbound,
	})
	if err != nil {
		t.Fatalf("upsert mswap claim: %v", err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	return nodeID, claimID
}

// TestReleaseOrderWithLineside_ConsumeReleaseDoesNotFireL1 pins the
// post-AMR-trial architecture: the consume-side release path no longer
// creates an L1 retrieve_empty at the loader. L1 is owned by Core's
// wiring_kanban DemandSignal pipeline (Core observes the bin movement
// at storage, emits to Edge, Edge fires L1 with current supply count).
//
// Both DispositionCaptureLineside and DispositionSendPartialBack are
// covered: neither releases of the consume side should fire L1, since
// the trigger is the system's filled-bin count, not the operator's
// release event.
func TestReleaseOrderWithLineside_ConsumeReleaseDoesNotFireL1(t *testing.T) {
	cases := []struct {
		name string
		mode ReleaseDispositionMode
	}{
		{"capture_lineside", DispositionCaptureLineside},
		{"send_partial_back", DispositionSendPartialBack},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testEngineDB(t)
			_, lineNodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
				Prefix: "L1-NOFIRE-" + tc.name, PayloadCode: "PART-" + tc.name, UOPCapacity: 100, InitialUOP: 8,
			})
			db.SetProcessNodeRuntime(lineNodeID, &claimID, 8)
			loaderNodeID, _ := seedManualSwapClaim(t, db, "LDR-"+tc.name, "produce", "PART-"+tc.name, "STORAGE-NODE")

			orderID := stageOrderForConsumeNode(t, db, lineNodeID, "uuid-"+tc.name)
			eng := testEngine(t, db)
			if err := eng.ReleaseOrderWithLineside(orderID, ReleaseDisposition{Mode: tc.mode}); err != nil {
				t.Fatalf("ReleaseOrderWithLineside: %v", err)
			}

			loaderOrders, err := db.ListActiveOrdersByProcessNode(loaderNodeID)
			if err != nil {
				t.Fatalf("ListActiveOrdersByProcessNode: %v", err)
			}
			for _, o := range loaderOrders {
				if o.RetrieveEmpty && o.PayloadCode == "PART-"+tc.name {
					t.Errorf("consume-side release (%s) must not fire L1 — owned by DemandSignal now; found %+v", tc.name, o)
				}
			}
		})
	}
}

// TestHandleUnloaderFullInCompletion_FiresU2 verifies the U2 mirror of
// handleLoaderEmptyInCompletion: when a U1 retrieve order (full bin, role
// consume, manual_swap) confirms at the unloader, U2 fires as a move from
// the unloader to claim.OutboundDestination.
func TestHandleUnloaderFullInCompletion_FiresU2(t *testing.T) {
	db := testEngineDB(t)
	unloaderNodeID, _ := seedManualSwapClaim(t, db, "U2-FIRE", "consume", "PART-U2", "STORAGE-NODE")

	// U1 = retrieve order (NOT retrieve_empty) at the unloader for the payload.
	orderID, err := db.CreateOrder("uuid-u1-fire", orders.TypeRetrieve,
		&unloaderNodeID, false, 1, "U2-FIRE-MSWAP-NODE", "", "", "PART-U2", false, "")
	if err != nil {
		t.Fatalf("create U1 order: %v", err)
	}
	db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed))

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	emitOrderCompleted(eng, orderID, "uuid-u1-fire", orders.TypeRetrieve, &unloaderNodeID)

	all, err := db.ListActiveOrdersByProcessNode(unloaderNodeID)
	if err != nil {
		t.Fatalf("ListActiveOrdersByProcessNode: %v", err)
	}
	var u2Found bool
	for _, o := range all {
		if o.OrderType == orders.TypeMove && o.DeliveryNode == "STORAGE-NODE" {
			u2Found = true
		}
	}
	if !u2Found {
		t.Errorf("expected U2 move from unloader to STORAGE-NODE; got %+v", all)
	}
}

// TestHandleNormalReplenishment_RetrieveStillResets verifies that simple
// retrieve orders that DELIVER to the process node continue to reset
// UOP at completion. Post-#11 the predicate also requires DeliveryNode
// to match the process node's CoreNodeName — see TestRegression_11_*
// for the negative path (removal-shaped orders).
func TestHandleNormalReplenishment_RetrieveStillResets(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "LSD-RETR", PayloadCode: "PART-RETR", UOPCapacity: 100, InitialUOP: 10,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 10); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// DeliveryNode must equal the seeded node's CoreNodeName for
	// binArrivingAt to fire. Prefix "LSD-RETR" → "LSD-RETR-NODE".
	// BinID must be set so binArrivingAt returns a non-nil pointer
	// (otherwise resolveReplenishUOP correctly returns 0 — empty slot).
	orderID, err := db.CreateOrder("uuid-retr", orders.TypeRetrieve,
		&nodeID, false, 1, "LSD-RETR-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed))
	deliveredBin := int64(101)
	db.UpdateOrderBinID(orderID, &deliveredBin)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	emitOrderCompleted(eng, orderID, "uuid-retr", orders.TypeRetrieve, &nodeID)

	runtime, _ := db.GetProcessNodeRuntime(nodeID)
	if runtime.RemainingUOPCached != 100 {
		t.Errorf("RemainingUOP = %d, want 100 (delivered handler fallback to claim capacity)",
			runtime.RemainingUOPCached)
	}
}

// TestRegression_ReleaseClickZeroesRuntimeUOP_AcrossSwapModes is the
// systematic guard for the SME release-timing contract (Stephen 2026-05-04):
//
//	On operator release click, runtime.RemainingUOPCached for the slot must
//	zero immediately for every swap mode where the bin being released is
//	physically leaving the slot. The slot's count flips back to capacity at
//	the new-bin-drop firing point (delivery completion via
//	SetProcessNodeRuntimeWithBin), not on a downstream confirm step.
//
// Two documented exceptions:
//
//	(a) Two-robot SUPPLY order (Order A): the supply bin is incoming; the
//	    OLD bin is still on the slot consuming until Order B (evac)
//	    completes. Supply releases must preserve the runtime count so the
//	    old bin's tally isn't wiped while it's still doing work.
//
//	(b) Produce role: produce nodes reset on ingest completion, not
//	    release. The release path early-returns before the zero so a
//	    produce release leaves runtime UOP intact.
//
// This test was added after a one-robot-swap floor observation where the
// HMI's lineside count stayed stale until a downstream confirm — the bug
// was the deferred runtime reset that this contract now disallows. If a
// future refactor moves the reset back, one of the subtests fails before
// it reaches the floor.
//
// Caveat: the supply-bin guard currently keys on `claim.SwapMode == "two_robot"`
// (operator_release.go), not "two_robot_press_index" or "sequential". Those
// modes' release behavior is covered as standalone single-order releases
// here. A future expansion of the supply guard to additional multi-bin
// modes should grow this test alongside.
func TestRegression_ReleaseClickZeroesRuntimeUOP_AcrossSwapModes(t *testing.T) {
	const (
		seededUOP = 800
		capacity  = 1200
	)

	type setup struct {
		swapMode protocol.SwapMode // claim.SwapMode
		role        string // "consume" or "produce"
		releaseSide string // "single", "supply", "evac" (which order to release)
	}
	type want struct {
		runtimeUOP int // expected runtime.RemainingUOPCached after release
	}

	cases := []struct {
		name  string
		setup setup
		disp  ReleaseDispositionMode
		want  want
	}{
		{
			name:  "simple_consume_zeroes",
			setup: setup{swapMode: "simple", role: "consume", releaseSide: "single"},
			disp:  DispositionCaptureLineside,
			want:  want{runtimeUOP: 0},
		},
		{
			name:  "single_robot_consume_zeroes",
			setup: setup{swapMode: "single_robot", role: "consume", releaseSide: "single"},
			disp:  DispositionCaptureLineside,
			want:  want{runtimeUOP: 0},
		},
		{
			name:  "two_robot_evac_zeroes",
			setup: setup{swapMode: "two_robot", role: "consume", releaseSide: "evac"},
			disp:  DispositionCaptureLineside,
			want:  want{runtimeUOP: 0},
		},
		// two_robot_supply_preserves removed: under the new contract,
		// both legs of a two-robot release write the supply bin's UOP
		// (resolved via sibling pointer). The "supply preserves"
		// behavior is gone — both legs are idempotent rewrites.
		{
			name:  "two_robot_press_index_zeroes",
			setup: setup{swapMode: "two_robot_press_index", role: "consume", releaseSide: "single"},
			disp:  DispositionCaptureLineside,
			want:  want{runtimeUOP: 0},
		},
		{
			name:  "sequential_consume_zeroes",
			setup: setup{swapMode: "sequential", role: "consume", releaseSide: "single"},
			disp:  DispositionCaptureLineside,
			want:  want{runtimeUOP: 0},
		},
		{
			name:  "send_partial_back_zeroes",
			setup: setup{swapMode: "simple", role: "consume", releaseSide: "single"},
			disp:  DispositionSendPartialBack,
			want:  want{runtimeUOP: 0},
		},
		{
			name:  "produce_role_preserves",
			setup: setup{swapMode: "simple", role: "produce", releaseSide: "single"},
			disp:  DispositionCaptureLineside,
			want:  want{runtimeUOP: seededUOP},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testEngineDB(t)

			// Process + style + node, mode-specific claim.
			processID, err := db.CreateProcess("REL-MODE-"+tc.name, "", "active_production", "", "", false, false)
			if err != nil {
				t.Fatalf("create process: %v", err)
			}
			coreNode := "REL-MODE-" + tc.name + "-NODE"
			nodeID, err := db.CreateProcessNode(processes.NodeInput{
				ProcessID:    processID,
				CoreNodeName: coreNode,
				Code:         "REL",
				Name:         "Release Mode " + tc.name,
				Sequence:     1,
				Enabled:      true,
			})
			if err != nil {
				t.Fatalf("create node: %v", err)
			}
			styleID, err := db.CreateStyle("REL-MODE-STYLE-"+tc.name, "", processID)
			if err != nil {
				t.Fatalf("create style: %v", err)
			}
			db.SetActiveStyle(processID, &styleID)
			claimInput := processes.NodeClaimInput{
				StyleID:        styleID,
				CoreNodeName:   coreNode,
				Role:           protocol.ClaimRole(tc.setup.role),
				SwapMode:       tc.setup.swapMode,
				PayloadCode:    "PART-MODE",
				UOPCapacity:    capacity,
				InboundSource:  "REL-MODE-SOURCE",
				InboundStaging: "REL-MODE-STAGING",
			}
			if tc.setup.swapMode == "two_robot_press_index" {
				claimInput.PairedCoreNode = coreNode + "-PAIR"
				claimInput.OutboundDestination = "REL-MODE-OUTBOUND"
			}
			claimID, err := db.UpsertStyleNodeClaim(claimInput)
			if err != nil {
				t.Fatalf("upsert claim: %v", err)
			}
			db.EnsureProcessNodeRuntime(nodeID)
			if err := db.SetProcessNodeRuntime(nodeID, &claimID, seededUOP); err != nil {
				t.Fatalf("seed runtime UOP: %v", err)
			}

			// Stage one or two orders depending on releaseSide. The
			// supply/evac convention matches isSupplyOrderInActiveTwoRobotSwap:
			// runtime.ActiveOrderID = supply (A), StagedOrderID = evac (B).
			var releaseOrderID int64
			switch tc.setup.releaseSide {
			case "single":
				releaseOrderID = stageOrderForConsumeNode(t, db, nodeID, "uuid-"+tc.name)
				if err := db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &releaseOrderID); err != nil {
					t.Fatalf("track staged order: %v", err)
				}
			case "supply":
				orderA := stageOrderForConsumeNode(t, db, nodeID, "uuid-"+tc.name+"-A")
				orderB := stageOrderForConsumeNode(t, db, nodeID, "uuid-"+tc.name+"-B")
				_ = db.UpdateOrderDeliveryNode(orderB, "TR-EVAC-DEST")
				if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB); err != nil {
					t.Fatalf("track A+B on runtime: %v", err)
				}
				if err := db.LinkOrderSiblings(orderA, orderB); err != nil {
					t.Fatalf("link siblings: %v", err)
				}
				releaseOrderID = orderA
			case "evac":
				orderA := stageOrderForConsumeNode(t, db, nodeID, "uuid-"+tc.name+"-A")
				orderB := stageOrderForConsumeNode(t, db, nodeID, "uuid-"+tc.name+"-B")
				_ = db.UpdateOrderDeliveryNode(orderB, "TR-EVAC-DEST")
				if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB); err != nil {
					t.Fatalf("track A+B on runtime: %v", err)
				}
				if err := db.LinkOrderSiblings(orderA, orderB); err != nil {
					t.Fatalf("link siblings: %v", err)
				}
				releaseOrderID = orderB
			default:
				t.Fatalf("unknown releaseSide %q", tc.setup.releaseSide)
			}

			eng := testEngine(t, db)
			disp := ReleaseDisposition{Mode: tc.disp, CalledBy: "regression-test"}
			if err := eng.ReleaseOrderWithLineside(releaseOrderID, disp); err != nil {
				t.Fatalf("ReleaseOrderWithLineside: %v", err)
			}

			runtime, _ := db.GetProcessNodeRuntime(nodeID)
			if runtime.RemainingUOPCached != tc.want.runtimeUOP {
				t.Errorf("runtime.RemainingUOPCached = %d, want %d (mode=%s role=%s side=%s disp=%s)",
					runtime.RemainingUOPCached, tc.want.runtimeUOP,
					tc.setup.swapMode, tc.setup.role, tc.setup.releaseSide, tc.disp)
			}
		})
	}
}
