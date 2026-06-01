// wiring_concurrent_tick_test.go — Item 17 regression coverage for the
// "state machine works while ticks are running" shape.
//
// Today the swap/changeover/release tests assume the line is paused; the
// tick tests are single-bin. The product — multi-bin choreography
// executing while the cell is still cycling — has zero coverage. This is
// the regression-prone shape that drove most of the bugs in early
// changeover testing.
//
// Each test follows: set up multi-bin scenario, fire swap/release/flip
// event, fire EventCounterDelta events at strategic timing points,
// assert per-tick attribution to the correct bucket/bin plus final
// runtime state.
package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// TestRegression_MultiBinAtPairedNodes_TicksAttributeCorrectly is the
// baseline for paired-node tick attribution: each side of an A/B pair
// has its own bin, the active side decrements its bin via consume_tick,
// and the inactive side's bin stays untouched. This is the precondition
// the more complex during-flip / during-swap tests build on; if this
// fails, the others can't be trusted.
func TestRegression_MultiBinAtPairedNodes_TicksAttributeCorrectly(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, _, _ := seedABPair(t, db)

	// Seed each side's bin via its own order. A is active (per
	// seedABPair), so its bin should receive the tick.
	const binA, binB int64 = 1001, 1002
	orderA := stageABOrder(t, db, nodeAID, "uuid-multibin-A", "PART-AB", binA)
	orderB := stageABOrder(t, db, nodeBID, "uuid-multibin-B", "PART-AB", binB)
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeAID, &orderA, nil), "set A active order")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeBID, &orderB, nil), "set B active order")
	bidA, bidB := binA, binB
	_ = db.SetProcessNodeActiveBinID(nodeAID, &bidA)
	_ = db.SetProcessNodeActiveBinID(nodeBID, &bidB)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	eng.Events.Emit(Event{
		Type: EventCounterDelta,
		Payload: CounterDeltaEvent{
			ProcessID: processID,
			StyleID:   styleID,
			Delta:     7,
		},
	})

	if len(sink.binCalls) != 1 {
		t.Fatalf("bin calls = %d, want 1 (only active side decrements): %+v",
			len(sink.binCalls), sink.binCalls)
	}
	bc := sink.binCalls[0]
	if bc.BinID != binA {
		t.Errorf("bin call BinID = %d, want %d (active=A)", bc.BinID, binA)
	}
	if bc.Delta != -7 || bc.Reason != protocol.ReasonConsumeTick {
		t.Errorf("bin call mismatch: %+v (want delta=-7 reason=consume_tick)", bc)
	}
}

// TestRegression_TickDuringABFlip pins the A/B-flip flush trigger
// under ticks: a tick that fires *during* the active-pull state flip
// must not get mis-attributed. The flip is sequential — Flush, then
// SetActivePull(new=true), then SetActivePull(old=false). A tick
// arriving between Flush and the active-pull writes should still find
// the correct active side. We exercise the boundary by firing ticks
// before, during (interleaved by re-issue), and after the flip.
func TestRegression_TickDuringABFlip(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeAID, nodeBID, styleID, _, _ := seedABPair(t, db)

	const binA, binB int64 = 2001, 2002
	orderA := stageABOrder(t, db, nodeAID, "uuid-abflip-A", "PART-AB", binA)
	orderB := stageABOrder(t, db, nodeBID, "uuid-abflip-B", "PART-AB", binB)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeAID, &orderA, nil)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeBID, &orderB, nil)
	bidA, bidB := binA, binB
	_ = db.SetProcessNodeActiveBinID(nodeAID, &bidA)
	_ = db.SetProcessNodeActiveBinID(nodeBID, &bidB)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &flushTrackingSink{fakeDeltaSink: fakeDeltaSink{db: db}}
	eng.SetInventoryDeltaSink(sink)

	// Pre-flip tick → A's bin.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 3,
	}})

	// Flip A → B. FlipABNode flushes before swapping active-pull; the
	// flush ensures any pre-flip deltas are sealed against the old
	// active context.
	testutil.MustNoErr(t, eng.FlipABNode(nodeBID), "FlipABNode")

	// Post-flip tick → B's bin.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 4,
	}})

	if len(sink.binCalls) != 2 {
		t.Fatalf("bin calls = %d, want 2 (one per side): %+v",
			len(sink.binCalls), sink.binCalls)
	}
	if sink.binCalls[0].BinID != binA || sink.binCalls[0].Delta != -3 {
		t.Errorf("pre-flip call mismatch: %+v (want bin=%d delta=-3)",
			sink.binCalls[0], binA)
	}
	if sink.binCalls[1].BinID != binB || sink.binCalls[1].Delta != -4 {
		t.Errorf("post-flip call mismatch: %+v (want bin=%d delta=-4)",
			sink.binCalls[1], binB)
	}
	if sink.flushes == 0 {
		t.Errorf("Flush() not called during FlipABNode (B5 flush trigger)")
	}
}

// TestRegression_TickDuringPullPartsLinesideWindow pins the consume
// path's bucket-first attribution after an operator captures parts to
// lineside. The operator clicks PULL PARTS LINESIDE → bucket fills →
// the bin is "released" (waiting for the bot to physically pick it up)
// while the cell keeps cycling. Each tick during that window must
// drain the bucket via consume_drain before any consume_tick lands on
// a bin. Without bucket-first ordering the post-release ticks would
// show as bin drains against whatever is at the slot, corrupting
// attribution.
func TestRegression_TickDuringPullPartsLinesideWindow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "TICK-PPL",
		PayloadCode: "PART-PPL",
		UOPCapacity: 100,
		InitialUOP:  100,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 100), "seed runtime")

	const binID int64 = 3001
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-ppl")
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &flushTrackingSink{fakeDeltaSink: fakeDeltaSink{db: db}}
	eng.SetInventoryDeltaSink(sink)

	// Operator pulls 20 parts to lineside.
	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-PPL": 20},
		CalledBy:        "test-op",
	}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "release")

	// Reset sink so we focus assertions on post-release ticks.
	sink.bucketCalls = nil
	sink.binCalls = nil

	// Cell keeps cycling: 8 ticks of 5 each = 40 total during the
	// pickup window. First 20 should drain the bucket; the rest
	// (clamped to 0 minimum) hit the bin.
	for i := 0; i < 8; i++ {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: styleID, Delta: 5,
		}})
	}

	bucketDrained := 0
	for _, b := range sink.bucketCalls {
		if b.Reason == protocol.ReasonConsumeDrain {
			bucketDrained += -b.Delta
		}
	}
	if bucketDrained != 20 {
		t.Errorf("bucket drained total = %d, want 20 (bucket size); calls = %+v",
			bucketDrained, sink.bucketCalls)
	}

	// First four ticks each hit the bucket (5 each = 20). The fifth
	// tick is the boundary (bucket is empty mid-tick); the rest go to
	// the bin. Attribution at every tick must be one or the other,
	// never silent.
	for i, b := range sink.bucketCalls {
		if b.Delta >= 0 {
			t.Errorf("bucket call[%d] non-negative delta %d (drains must be negative): %+v", i, b.Delta, b)
		}
	}
}

// TestRegression_TickDuringPartialBackPickupWindow pins the Item 11
// scenario: operator releases a partial bin, cell keeps cycling,
// ticks fire before the robot physically picks the bin up. Until the
// BinPickedUp envelope arrives, ticks must keep attributing to the
// released bin. After BinPickedUp clears the runtime's ActiveOrderID,
// subsequent ticks no longer attribute to the released bin.
//
// Companion to TestRegression_PartialBackTicksAttributeToReleasedBin
// in handler_bin_picked_up_test.go — same invariant, exercised here
// via the public Engine surface so a future dispatch-layer refactor
// can't silently break during-pickup-window attribution.
func TestRegression_TickDuringPartialBackPickupWindow(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "PB-PICKUP",
		PayloadCode: "PART-PB-PU",
		UOPCapacity: 100,
		InitialUOP:  40,
	})
	const binID int64 = 11500
	const orderUUID = "uuid-pb-pu"
	orderID, err := db.CreateOrder(orderUUID, orders.TypeRetrieve,
		&nodeID, false, 1, "PB-PICKUP-NODE", "", "", "", false, "PART-PB-PU")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bid, 40), "seed runtime")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	// During-window: 3 ticks, all attribute to the released bin.
	for i := 0; i < 3; i++ {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: styleID, Delta: 2,
		}})
	}
	preCount := len(sink.binCalls)
	for _, c := range sink.binCalls {
		if c.BinID != binID {
			t.Errorf("during-window tick attributed to bin=%d, want %d", c.BinID, binID)
		}
	}
	if preCount != 3 {
		t.Fatalf("during-window bin calls = %d, want 3", preCount)
	}

	// BinPickedUp fires.
	eng.HandleBinPickedUp(orderUUID, binID, "PB-PICKUP-NODE")

	// Post-pickup tick must not attribute to the released bin.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 2,
	}})
	for _, c := range sink.binCalls[preCount:] {
		if c.BinID == binID {
			t.Errorf("post-pickup tick attributed to released bin=%d (must not — pickup cleared ActiveOrderID)",
				c.BinID)
		}
	}
}

// TestRegression_TickDuringChangeoverRunout pins the runout window:
// while a changeover task is in "preparing" / "ready" state but before
// the operator clicks RELEASE, the line is still consuming the
// from-style bin. Ticks during this window must attribute to the
// from-style's claim/bin, not jump prematurely to the to-style. The
// resolveReleaseClaim → toClaim transition only fires on release; until
// then the active claim drives attribution.
func TestRegression_TickDuringChangeoverRunout(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, fromStyleID, toStyleID := seedRunoutScenario(t, db, "RUNOUT")

	// from-style claim is the runtime's active claim; ticks fire under
	// from-style's StyleID until cutover.
	fromClaim, _ := db.GetStyleNodeClaimByNode(fromStyleID, "RUNOUT-NODE")
	if fromClaim == nil {
		t.Fatal("from claim missing — seed contract changed")
	}

	const binID int64 = 4001
	orderID, err := db.CreateOrder("uuid-runout", orders.TypeRetrieve,
		&nodeID, false, 1, "RUNOUT-NODE", "", "", "", false, "PART-OLD")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &fromClaim.ID, &bid, 80), "seed runtime")

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	// Fire ticks under from-style — these are pre-release runout ticks.
	for i := 0; i < 3; i++ {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: fromStyleID, Delta: 5,
		}})
	}

	// to-style ticks before cutover should be ignored — the runtime's
	// active claim is still fromStyle. handleCounterDelta only matches
	// claim.StyleID == delta.StyleID, so to-style ticks find no claim.
	for i := 0; i < 2; i++ {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: toStyleID, Delta: 5,
		}})
	}

	// Only the 3 from-style ticks should have produced bin deltas.
	binTotal := 0
	for _, c := range sink.binCalls {
		if c.BinID != binID {
			t.Errorf("bin call attributed to bin=%d, want %d (must stay on from-style bin until cutover): %+v",
				c.BinID, binID, c)
		}
		binTotal += -c.Delta
	}
	if binTotal != 15 {
		t.Errorf("bin call total = %d, want 15 (3 ticks × 5 from-style only): %+v",
			binTotal, sink.binCalls)
	}
}

// TestRegression_TickDuringTwoRobotSwap pins the active-claim shape
// during a two-robot evac/deliver swap. R1 evacuates the old bin (the
// "supply" leg in two-robot terminology — actually the outgoing bin
// for consume nodes), R2 delivers the fresh bin. Ticks fire during the
// choreography. Until the new bin's delivery completes, attribution
// stays on the old bin. After delivery, the runtime resets to the new
// bin's UOP (snapshot value or capacity) and ticks attribute to the new
// bin via the now-current order.
func TestRegression_TickDuringTwoRobotSwap(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "TR-SWAP",
		PayloadCode: "PART-TR",
		UOPCapacity: 100,
		InitialUOP:  60,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 60), "seed runtime")
	// Promote claim to two_robot so the supply-bin guard is exercised.
	claim, _ := db.GetStyleNodeClaimByNode(styleID, "TR-SWAP-NODE")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: claim.StyleID, CoreNodeName: claim.CoreNodeName,
		Role: claim.Role, SwapMode: "two_robot",
		PayloadCode: claim.PayloadCode, UOPCapacity: claim.UOPCapacity,
		InboundSource: "TR-SOURCE", InboundStaging: "TR-STAGING",
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// Order A (supply / new bin), Order B (evac / outgoing bin). On the
	// runtime row, ActiveOrderID = B (the bin currently at the line),
	// StagedOrderID = A (the supply on its way in). Ticks must
	// attribute to B's bin until the swap completes — pin the bin
	// pointer to the evac bin since that's what's physically at the slot.
	const binEvac, binSupply int64 = 5001, 5002
	orderEvac := stageOrderForConsumeNode(t, db, nodeID, "uuid-tr-evac")
	orderSupply := stageOrderForConsumeNode(t, db, nodeID, "uuid-tr-supply")
	bidE, bidS := binEvac, binSupply
	_ = db.UpdateOrderBinID(orderEvac, &bidE)
	_ = db.UpdateOrderBinID(orderSupply, &bidS)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderEvac, &orderSupply)
	_ = db.SetProcessNodeActiveBinID(nodeID, &bidE)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	// Ticks during the swap (R1 enroute, hasn't picked up yet) attribute
	// to evac bin.
	for i := 0; i < 4; i++ {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: styleID, Delta: 3,
		}})
	}

	for _, c := range sink.binCalls {
		if c.BinID != binEvac {
			t.Errorf("during-swap tick attributed to bin=%d, want %d (evac bin until delivery completes): %+v",
				c.BinID, binEvac, c)
		}
	}

	totalDuringSwap := 0
	for _, c := range sink.binCalls {
		totalDuringSwap += -c.Delta
	}
	if totalDuringSwap != 12 {
		t.Errorf("during-swap bin total = %d, want 12 (4 ticks × 3): %+v",
			totalDuringSwap, sink.binCalls)
	}
}

// TestRegression_ChangeoverDoesNotCarryUOPAcrossStyles pins the cross-
// style invariant via the tick path: changing from style X to Y and
// back to X must not leave Y's runtime UOP polluting X's tracking.
// Ticks are dispatched by handleCounterDelta against `claim.StyleID
// == delta.StyleID` — so during X, only X-style ticks attribute; the
// runtime decrement targets the *X bin* via the active order. After
// pivot to Y, X-style ticks find no active claim (the runtime now
// points at Y's claim), and Y-style ticks decrement Y's runtime.
// Pivot back to X with a fresh bin: ticks resume against the new X
// bin, and the runtime starts from the new bin's count, not Y's
// residue or X's prior session.
func TestRegression_ChangeoverDoesNotCarryUOPAcrossStyles(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleX, styleY, claimX, claimY := seedTwoStyleNode(t, db, "X2Y")

	// --- Phase 1: under X. Active order points at bin_X1. ---
	const binX1 int64 = 6001
	orderX1 := stageABOrder(t, db, nodeID, "uuid-x2y-x1", "PART-X", binX1)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderX1, nil)
	db.SetActiveStyle(processID, &styleX)
	bidX1 := binX1
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimX, &bidX1, 200)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &fakeDeltaSink{db: db}
	eng.SetInventoryDeltaSink(sink)

	// 30 units consumed under X.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleX, Delta: 30,
	}})
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 170 {
		t.Errorf("after X tick: runtime=%d, want 170 (200-30)", rt.RemainingUOPCached)
	}
	if len(sink.binCalls) != 1 || sink.binCalls[0].BinID != binX1 {
		t.Errorf("X tick attribution: %+v (want one call to bin %d)", sink.binCalls, binX1)
	}

	// --- Phase 2: pivot to Y with a fresh bin. ---
	const binY int64 = 6002
	orderY := stageABOrder(t, db, nodeID, "uuid-x2y-y", "PART-Y", binY)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderY, nil)
	db.SetActiveStyle(processID, &styleY)
	bidY := binY
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimY, &bidY, 150) // Y capacity

	// X-style tick: should be ignored (runtime active claim is Y).
	sink.binCalls = nil
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleX, Delta: 5,
	}})
	if len(sink.binCalls) != 0 {
		t.Errorf("post-pivot X tick attributed (must find no claim): %+v", sink.binCalls)
	}

	// Y-style tick: decrements Y's bin from 150 → 130.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleY, Delta: 20,
	}})
	rt, _ = db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 130 {
		t.Errorf("after Y tick: runtime=%d, want 130 (150-20, NOT 170-20=150 from X residue)",
			rt.RemainingUOPCached)
	}
	if len(sink.binCalls) != 1 || sink.binCalls[0].BinID != binY {
		t.Errorf("Y tick attribution: %+v (want one call to bin %d)", sink.binCalls, binY)
	}

	// --- Phase 3: pivot back to X with a fresh bin. ---
	const binX2 int64 = 6003
	orderX2 := stageABOrder(t, db, nodeID, "uuid-x2y-x2", "PART-X", binX2)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderX2, nil)
	db.SetActiveStyle(processID, &styleX)
	bidX2 := binX2
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimX, &bidX2, 200) // fresh X capacity

	sink.binCalls = nil
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleX, Delta: 10,
	}})
	rt, _ = db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 190 {
		t.Errorf("post-Y→X fresh runtime=%d, want 190 (200-10, NOT 130-10=120 from Y residue or 170-10=160 from prior X)",
			rt.RemainingUOPCached)
	}
	if len(sink.binCalls) != 1 || sink.binCalls[0].BinID != binX2 {
		t.Errorf("X-fresh tick attribution: %+v (want bin %d, NOT prior X bin %d)",
			sink.binCalls, binX2, binX1)
	}
}

// TestRegression_ChangeoverBackToStyle_ResetsToCapacityPostItem8 pins
// the post-Item-8 partial-return shape across a style cycle: original
// X bin returns (e.g. SEND PARTIAL BACK then re-deliver). Pre-Item-8
// the runtime read OrderDelivered.BinUOPRemaining and reset to the
// bin's actual partial value; post-Item-8 the snapshot is gone and
// the runtime resets to claim.UOPCapacity unconditionally. The
// reconciler heals to Core's authoritative bin value within ~60s
// (covered by TestRegression_ReconciliationSelfHeal in
// uop_reconciler_test.go).
func TestRegression_ChangeoverBackToStyle_ResetsToCapacityPostItem8(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, styleX, _, claimX, _ := seedTwoStyleNode(t, db, "BACK")

	xClaim, _ := db.GetStyleNodeClaimByNode(styleX, "BACK-NODE")
	if xClaim == nil {
		t.Fatal("X claim missing — seed contract changed")
	}

	// Set baseline runtime (pre-changeover X session at 80).
	db.SetActiveStyle(xClaim.StyleID, &styleX)
	db.SetProcessNodeRuntime(nodeID, &claimX, 80)

	// X bin returns. Order has bin_id set so binArrivingAt picks it up
	// at completion and resolveReplenishUOP returns claim capacity.
	orderID, err := db.CreateOrder("uuid-back-return", orders.TypeComplex,
		&nodeID, false, 1, "BACK-NODE", "", "", "", false, "PART-X")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "confirm order")
	returnedBin := int64(7777)
	_ = db.UpdateOrderBinID(orderID, &returnedBin)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	emitOrderCompleted(eng, orderID, "uuid-back-return", orders.TypeComplex, &nodeID)

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != xClaim.UOPCapacity {
		t.Errorf("post-arrival runtime = %d, want %d (delivered handler fallback to claim.UOPCapacity)",
			rt.RemainingUOPCached, xClaim.UOPCapacity)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// stageABOrder seeds a confirmed retrieve order against an A/B node with
// the given bin id. Used for paired-node tick-attribution scenarios
// where each side needs its own active order with a bin id.
func stageABOrder(t *testing.T, db *store.DB, nodeID int64, uuid, payload string, binID int64) int64 {
	t.Helper()
	orderID, err := db.CreateOrder(uuid, orders.TypeRetrieve,
		&nodeID, false, 1, "AB-NODE-X", "", "", "", false, payload)
	if err != nil {
		t.Fatalf("create order %s: %v", uuid, err)
	}
	bid := binID
	testutil.MustNoErr(t, db.UpdateOrderBinID(orderID, &bid), "set bin id")
	testutil.MustNoErr(t, db.UpdateOrderStatus(orderID, string(orders.StatusConfirmed)), "confirm")
	return orderID
}

// seedRunoutScenario creates a node with two styles, where from-style is
// active. Models the changeover-runout window: the line is consuming
// from from-style while preparing the to-style switch.
func seedRunoutScenario(t *testing.T, db *store.DB, prefix string) (processID, nodeID, fromStyleID, toStyleID int64) {
	t.Helper()
	processID, _ = db.CreateProcess(prefix+"-PROC", prefix+" runout", "active_production", "", "", false, false)
	nodeID, _ = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: prefix + "-NODE",
		Code: prefix[:3], Name: prefix + " Node", Sequence: 1, Enabled: true,
	})
	fromStyleID, _ = db.CreateStyle(prefix+"-FROM", prefix+" from", processID)
	toStyleID, _ = db.CreateStyle(prefix+"-TO", prefix+" to", processID)
	db.SetActiveStyle(processID, &fromStyleID)

	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: prefix + "-NODE",
		Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-OLD", UOPCapacity: 100,
	})
	db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: prefix + "-NODE",
		Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-NEW", UOPCapacity: 100,
	})
	db.EnsureProcessNodeRuntime(nodeID)
	return
}

// seedTwoStyleNode creates a node with claims for two styles X and Y on
// the same node. Returns ids needed to pivot ActiveClaimID across the
// styles for changeover scenarios.
func seedTwoStyleNode(t *testing.T, db *store.DB, prefix string) (processID, nodeID, styleX, styleY, claimX, claimY int64) {
	t.Helper()
	processID, _ = db.CreateProcess(prefix+"-PROC", prefix+" two style", "active_production", "", "", false, false)
	nodeID, _ = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: prefix + "-NODE",
		Code: prefix[:3], Name: prefix + " Node", Sequence: 1, Enabled: true,
	})
	styleX, _ = db.CreateStyle(prefix+"-X", prefix+" X", processID)
	styleY, _ = db.CreateStyle(prefix+"-Y", prefix+" Y", processID)

	claimX, _ = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleX, CoreNodeName: prefix + "-NODE",
		Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-X", UOPCapacity: 200,
	})
	claimY, _ = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleY, CoreNodeName: prefix + "-NODE",
		Role: "consume", SwapMode: "simple",
		PayloadCode: "PART-Y", UOPCapacity: 150,
	})
	db.EnsureProcessNodeRuntime(nodeID)
	return
}
