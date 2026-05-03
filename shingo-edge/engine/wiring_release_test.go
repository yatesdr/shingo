package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// TestRegression_CaptureDeltaUsesActualBinPayload pins the D3/Item 5.5
// fix at operator_release.go: the BinUOPDelta(capture_reduction) emitted
// on release-with-capture must carry the bin's actual payload code (taken
// from the order, set at order create-time) — NOT the active claim's
// payload code (the target-style template). During changeover or
// reassignment scenarios the two diverge: the bin physically holds the
// old style's parts while the node has already swapped its active claim
// to the new style. Sending the claim's payload tripped Core's
// payload-mismatch validation in inventory_delta_service.ApplyBinUOPDelta
// and the delta was silently rejected — the bin's authoritative
// uop_remaining never moved on Core, leaving the count stale.
//
// Core-side rejection is already pinned by
// shingo-core/service/inventory_delta_service_test.go
// TestInventoryDelta_BinUOPDelta_RejectsMismatchedPayload — the doc's
// proposed TestRegression_CaptureDeltaRejectedOnTrueMismatch would
// duplicate that coverage, so this Edge-side pin is sufficient.
func TestRegression_CaptureDeltaUsesActualBinPayload(t *testing.T) {
	db := testEngineDB(t)
	// Active claim is the TARGET style — "PART-NEW".
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "REL-PAYLOAD-DRIFT",
		PayloadCode: "PART-NEW",
		UOPCapacity: 100,
		InitialUOP:  100,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 100); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	// Order was created when the bin held the OLD payload. The order
	// row's payload_code captures the bin's true payload at create-time;
	// the active claim has since rolled to PART-NEW.
	orderID, err := db.CreateOrder("uuid-payload-drift", orders.TypeComplex,
		&nodeID, false, 1, "REL-PAYLOAD-DRIFT-NODE", "", "", "", false,
		"PART-OLD")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("transition to staged: %v", err)
	}
	const binID int64 = 7777
	bid := binID
	if err := db.UpdateOrderBinID(orderID, &bid); err != nil {
		t.Fatalf("set bin id: %v", err)
	}
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID); err != nil {
		t.Fatalf("set runtime orders: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &flushTrackingSink{}
	eng.SetInventoryDeltaSink(sink)

	// Capture target-style parts to lineside — bucket fills attribute to
	// the active claim's part. The bin reduction is the SAME total but
	// must carry the bin's payload, not the active claim's.
	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-NEW": 30},
		CalledBy:        "test-op",
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("release: %v", err)
	}

	if len(sink.binCalls) != 1 {
		t.Fatalf("bin calls = %d, want 1: %+v", len(sink.binCalls), sink.binCalls)
	}
	binCall := sink.binCalls[0]
	if binCall.PayloadCode != "PART-OLD" {
		t.Errorf("bin call payload = %q, want %q (must be order.PayloadCode — the bin's actual payload — not the active claim's target-style template)",
			binCall.PayloadCode, "PART-OLD")
	}
	if binCall.BinID != binID || binCall.Delta != -30 || binCall.Reason != protocol.ReasonCaptureReduction {
		t.Errorf("bin call mismatch: %+v (want bin=%d delta=-30 reason=capture_reduction)",
			binCall, binID)
	}
}

// TestRegression_CaptureReleaseFlushBoundary pins the release-click
// flush trigger: when the operator submits PULL PARTS LINESIDE, any
// per-part captures emit LinesideBucketDelta(capture_fill), the
// summed bin reduction emits BinUOPDelta(capture_reduction), and
// Flush is called after the OrderRelease envelope is queued. Without
// the explicit flush, an Edge restart between release and the next
// 5s tick could lose the just-recorded deltas.
func TestRegression_CaptureReleaseFlushBoundary(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "REL-CAP-FLUSH",
		PayloadCode: "PART-CAP",
		UOPCapacity: 100,
		InitialUOP:  100,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 100); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	const binID int64 = 12345
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-cap-flush")
	bid := binID
	if err := db.UpdateOrderBinID(orderID, &bid); err != nil {
		t.Fatalf("set bin id: %v", err)
	}
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID); err != nil {
		t.Fatalf("set runtime orders: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &flushTrackingSink{}
	eng.SetInventoryDeltaSink(sink)

	// Operator pulls 30 of PART-CAP to lineside.
	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-CAP": 30},
		CalledBy:        "test-op",
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Bucket fill: one capture_fill record for 30.
	if len(sink.bucketCalls) != 1 {
		t.Fatalf("bucket calls = %d, want 1: %+v", len(sink.bucketCalls), sink.bucketCalls)
	}
	bc := sink.bucketCalls[0]
	if bc.Delta != 30 || bc.Reason != protocol.ReasonCaptureFill || bc.PartNumber != "PART-CAP" {
		t.Errorf("bucket call mismatch: %+v", bc)
	}

	// Bin reduction: one capture_reduction record for -30 against the bin.
	if len(sink.binCalls) != 1 {
		t.Fatalf("bin calls = %d, want 1: %+v", len(sink.binCalls), sink.binCalls)
	}
	binCall := sink.binCalls[0]
	if binCall.BinID != binID || binCall.Delta != -30 || binCall.Reason != protocol.ReasonCaptureReduction {
		t.Errorf("bin call mismatch: %+v (want bin=%d delta=-30 reason=capture_reduction)",
			binCall, binID)
	}

	// Flush MUST have been called at least once after the release.
	if sink.flushes == 0 {
		t.Errorf("Flush() not called — release-click is a flush trigger and must fire after the OrderRelease envelope queues")
	}
}

// TestRegression_ReleasePartialEmitsNoBinDelta pins the inverse case:
// when the operator picks SEND PARTIAL BACK (no captures), no bucket
// delta or capture_reduction bin delta fires. The legacy RemainingUOP
// pointer still ships via the OrderRelease envelope; only the
// Phase 1 delta emission stays quiet.
func TestRegression_ReleasePartialEmitsNoBinDelta(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "REL-PARTIAL-NOEMIT",
		PayloadCode: "PART-PNE",
		UOPCapacity: 100,
		InitialUOP:  47,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 47); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-partial-noemit")
	bid := int64(99)
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &flushTrackingSink{}
	eng.SetInventoryDeltaSink(sink)

	disp := ReleaseDisposition{
		Mode:     DispositionSendPartialBack,
		CalledBy: "test-op",
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("release: %v", err)
	}

	if len(sink.bucketCalls) != 0 {
		t.Errorf("bucket calls = %d, want 0 (partial back doesn't capture): %+v",
			len(sink.bucketCalls), sink.bucketCalls)
	}
	if len(sink.binCalls) != 0 {
		t.Errorf("bin calls = %d, want 0 (no captures means no capture_reduction): %+v",
			len(sink.binCalls), sink.binCalls)
	}
}

// TestRegression_ReleaseSupplyOrderSuppressesBinDelta pins the
// supply-bin guard at the delta layer. For Order A in a two-robot
// swap, the manifestUOP suppression also suppresses the
// capture_reduction bin delta — applying it would corrupt the shadow
// count for a fresh supply bin. Bucket fills still fire (the parts
// physically went to lineside regardless of which order triggered
// the release).
func TestRegression_ReleaseSupplyOrderSuppressesBinDelta(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "REL-SUPPLY-SUPP",
		PayloadCode: "PART-SUP",
		UOPCapacity: 100,
		InitialUOP:  100,
	})
	// Promote the claim to two_robot.
	claim, _ := db.GetStyleNodeClaimByNode(activeStyleForNode(t, db, nodeID), "REL-SUPPLY-SUPP-NODE")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:        claim.StyleID,
		CoreNodeName:   claim.CoreNodeName,
		Role:           claim.Role,
		SwapMode:       "two_robot",
		PayloadCode:    claim.PayloadCode,
		UOPCapacity:    claim.UOPCapacity,
		InboundSource:  "TR-SOURCE",
		InboundStaging: "TR-STAGING",
	}); err != nil {
		t.Fatalf("promote claim: %v", err)
	}

	orderA := stageOrderForConsumeNode(t, db, nodeID, "uuid-supply-A")
	orderB := stageOrderForConsumeNode(t, db, nodeID, "uuid-supply-B")
	bidA := int64(2001)
	bidB := int64(2002)
	_ = db.UpdateOrderBinID(orderA, &bidA)
	_ = db.UpdateOrderBinID(orderB, &bidB)
	if err := db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB); err != nil {
		t.Fatalf("set A+B: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &flushTrackingSink{}
	eng.SetInventoryDeltaSink(sink)

	// Operator picks capture_lineside on Order A — the supply leg.
	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-SUP": 5},
		CalledBy:        "test-op",
	}
	if err := eng.ReleaseOrderWithLineside(orderA, disp); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Bucket fill ships — the parts physically left wherever they
	// came from and went to lineside.
	if len(sink.bucketCalls) != 1 {
		t.Errorf("bucket calls = %d, want 1 (capture_fill rides regardless of supply guard): %+v",
			len(sink.bucketCalls), sink.bucketCalls)
	}
	// Bin reduction does NOT — supply bin must not be reduced.
	if len(sink.binCalls) != 0 {
		t.Errorf("bin calls = %d, want 0 (supply bin must not get capture_reduction): %+v",
			len(sink.binCalls), sink.binCalls)
	}
}

// TestRegression_ABInactivePairFlush pins the A/B-flip flush trigger:
// FlipABNode triggers a reporter flush before swapping active-pull,
// so any deltas the inactive accumulator collected ship before the
// new bin starts driving counts. Without this trigger the inactive
// node's residual deltas would either get lost (on Edge restart) or
// attribute to the wrong active-bin context.
func TestRegression_ABInactivePairFlush(t *testing.T) {
	db := testEngineDB(t)
	_, nodeAID, styleID, claimAID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "AB-FLIP-A",
		PayloadCode: "PART-AB",
		UOPCapacity: 100,
		InitialUOP:  100,
	})
	// Pair claim A → B.
	claimA, _ := db.GetStyleNodeClaimByNode(styleID, "AB-FLIP-A-NODE")
	nodeBID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    claimA.StyleID, // any process; same one
		CoreNodeName: "AB-FLIP-B-NODE",
		Code:         "ABB",
		Name:         "AB Flip B",
		Sequence:     2,
		Enabled:      true,
	})
	if err != nil {
		// The signature is best-effort — if it errs (style/process
		// mismatch), the test setup is fine without a real paired
		// node row; the FlipABNode call only checks PairedCoreNode
		// on the claim, not that the paired node exists for the
		// flush-trigger assertion. Skip to the assertion.
		t.Logf("paired node create err (non-fatal for flush trigger test): %v", err)
		nodeBID = 0
	}

	// Pair on the claim itself — that's what FlipABNode checks.
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:        claimA.StyleID,
		CoreNodeName:   claimA.CoreNodeName,
		Role:           claimA.Role,
		SwapMode:       claimA.SwapMode,
		PayloadCode:    claimA.PayloadCode,
		UOPCapacity:    claimA.UOPCapacity,
		PairedCoreNode: "AB-FLIP-B-NODE",
	}); err != nil {
		t.Fatalf("pair claim A: %v", err)
	}

	if err := db.SetProcessNodeRuntime(nodeAID, &claimAID, 100); err != nil {
		t.Fatalf("seed runtime A: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &flushTrackingSink{}
	eng.SetInventoryDeltaSink(sink)

	// Pre-condition: zero flushes so far.
	if sink.flushes != 0 {
		t.Fatalf("pre-flip flushes = %d, want 0", sink.flushes)
	}

	if err := eng.FlipABNode(nodeAID); err != nil {
		t.Fatalf("FlipABNode: %v", err)
	}

	if sink.flushes == 0 {
		t.Errorf("Flush() not called during FlipABNode — A/B active-pull state flip is a flush trigger")
	}
	_ = nodeBID
	_ = orders.TypeRetrieve // anchor import
}

// flushTrackingSink extends fakeDeltaSink with a Flush counter so
// release-path tests can assert the explicit flush triggers fire.
type flushTrackingSink struct {
	fakeDeltaSink
	flushes int
}

func (s *flushTrackingSink) Flush() {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
}

// TestRegression_ReleaseRejectsWhenDeltaPending pins the Item 12
// release-click pending guard. After the release-time Flush, if the
// reporter still reports the bin as pending (a transient outbox
// enqueue failure), ReleaseOrderWithLineside must return
// ErrCountChangePending and decline to ship the OrderRelease envelope.
// Without the guard, the release would race with the next periodic
// flush at Core: the manifest sync could land before (or after) the
// bin delta in unpredictable order, corrupting Core's bin count.
func TestRegression_ReleaseRejectsWhenDeltaPending(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "REL-PEND",
		PayloadCode: "PART-PEND",
		UOPCapacity: 100,
		InitialUOP:  100,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 100); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	const binID int64 = 8001
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-pend")
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	// Sink seeded with the bin already pending — survives Flush
	// because this fake sink doesn't actually drain on Flush, mimicking
	// the production case where EnqueueOutbox failed transiently and
	// the entry stays in pendingBinIDs.
	sink := &flushTrackingSink{
		fakeDeltaSink: fakeDeltaSink{
			pendingBins: map[int64]struct{}{binID: {}},
		},
	}
	eng.SetInventoryDeltaSink(sink)

	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-PEND": 5},
		CalledBy:        "test-op",
	}
	err := eng.ReleaseOrderWithLineside(orderID, disp)
	if err != ErrCountChangePending {
		t.Fatalf("ReleaseOrderWithLineside err = %v, want ErrCountChangePending (pending bin must abort release)", err)
	}

	// The order must NOT have transitioned to in_transit — the abort
	// happens before orderMgr.ReleaseOrderWithDisposition runs.
	o, _ := db.GetOrder(orderID)
	if o.Status == orders.StatusInTransit {
		t.Errorf("order status = %q, want NOT in_transit (release must not ship when pending)", o.Status)
	}
}

// TestRegression_ReleaseAcceptsAfterFlush is the positive companion:
// when the bin is not pending after Flush, the release proceeds
// normally. Pin: the guard is a transient gate, not a permanent
// block; the periodic-flush recovery path can't deadlock the operator.
func TestRegression_ReleaseAcceptsAfterFlush(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "REL-ACCEPT",
		PayloadCode: "PART-ACCEPT",
		UOPCapacity: 100,
		InitialUOP:  100,
	})
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, 100); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	const binID int64 = 8002
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-accept")
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	// Sink with no pending entries — release must succeed.
	sink := &flushTrackingSink{}
	eng.SetInventoryDeltaSink(sink)

	disp := ReleaseDisposition{
		Mode:            DispositionCaptureLineside,
		LinesideCapture: map[string]int{"PART-ACCEPT": 5},
		CalledBy:        "test-op",
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("ReleaseOrderWithLineside err = %v, want nil (no pending → release proceeds)", err)
	}

	o, _ := db.GetOrder(orderID)
	if o.Status != orders.StatusInTransit {
		t.Errorf("order status = %q, want %q (release must dispatch when no pending)", o.Status, orders.StatusInTransit)
	}
	if sink.flushes == 0 {
		t.Errorf("Flush() not called — release must flush before checking pending")
	}
}

// TestRegression_ReleaseUnderpack_WireShape pins the Edge half of the
// underpack release flow: the operator declares the bin physically
// empty before the tracked count reaches zero (bin labeled 1200
// actually held 1190; cell starves at runtime=10). Wire shape:
//   - OrderRelease.RemainingUOP = &0 (same as RELEASE EMPTY)
//   - OrderRelease.Disposition.Kind = DispositionReleaseUnderpack
//   - OrderRelease.Disposition.CountSuggested = the runtime cache at click
//     time (so Core can record the missing-delta in the audit row)
//
// The disposition kind is what distinguishes the underpack signal
// from RELEASE EMPTY at the audit layer. Without the wire-shape pin
// here, an Edge-side regression that lost the disposition (or mapped
// it to release_empty) would be invisible until forensics realized
// the underpack pattern stopped showing up.
func TestRegression_ReleaseUnderpack_WireShape(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix:      "REL-UNDERPACK",
		PayloadCode: "PART-UP",
		UOPCapacity: 1200,
		InitialUOP:  10,
	})
	const runtimeAtClick = 10
	if err := db.SetProcessNodeRuntime(nodeID, &claimID, runtimeAtClick); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	const binID int64 = 12001
	orderID := stageOrderForConsumeNode(t, db, nodeID, "uuid-underpack")
	bid := binID
	_ = db.UpdateOrderBinID(orderID, &bid)
	_ = db.UpdateProcessNodeRuntimeOrders(nodeID, nil, &orderID)

	eng := testEngine(t, db)
	eng.wireEventHandlers()
	sink := &flushTrackingSink{}
	eng.SetInventoryDeltaSink(sink)

	// Drain pre-existing outbox.
	pending, _ := db.ListPendingOutbox(100)
	for _, m := range pending {
		_ = db.AckOutbox(m.ID)
	}

	disp := ReleaseDisposition{
		Mode:     DispositionReleaseUnderpack,
		CalledBy: "test-op",
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	releases := findOutboxByType(t, db, protocol.TypeOrderRelease)
	if len(releases) != 1 {
		t.Fatalf("OrderRelease envelopes = %d, want 1", len(releases))
	}
	rel := decodeOrderRelease(t, releases[0])

	// Wire shape: RemainingUOP = &0 (manifest clear).
	if rel.RemainingUOP == nil {
		t.Fatalf("wire RemainingUOP = nil, want &0 (underpack clears manifest like RELEASE EMPTY)")
	}
	if *rel.RemainingUOP != 0 {
		t.Errorf("wire RemainingUOP = %d, want 0", *rel.RemainingUOP)
	}

	// Disposition: kind = release_underpack so Core's audit op is
	// OpReleasedUnderpack instead of OpReleasedEmpty.
	if rel.Disposition == nil {
		t.Fatalf("wire Disposition = nil, want non-nil with Kind=release_underpack")
	}
	if rel.Disposition.Kind != protocol.DispositionReleaseUnderpack {
		t.Errorf("Disposition.Kind = %q, want %q",
			rel.Disposition.Kind, protocol.DispositionReleaseUnderpack)
	}

	// CountSuggested carries the runtime at click time so Core can
	// record the missing-inventory delta. Without this, forensics
	// couldn't tell the magnitude of the underpack from the audit
	// row alone.
	if rel.Disposition.CountSuggested == nil {
		t.Errorf("Disposition.CountSuggested = nil, want &%d (runtime at click time)", runtimeAtClick)
	} else if *rel.Disposition.CountSuggested != runtimeAtClick {
		t.Errorf("Disposition.CountSuggested = %d, want %d",
			*rel.Disposition.CountSuggested, runtimeAtClick)
	}
}
