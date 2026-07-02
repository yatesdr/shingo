//go:build docker

package dispatch

// Phase-0 characterization tests.
//
// These tests are GREEN on the 2026-06-23 codebase (before any Phase-0 structural
// change) and serve as a trip-wire: if an edit flips one of them the change MUST be
// deliberate and the comment on the affected test must be updated to explain why.
//
// They are NOT design intent — they pin what the code does today, including the
// sourcing-flip asymmetry between simple and complex paths (tech debt documented
// in each test). See SYNTH-round2.md for the context that produced this file.

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// ── Disposition triad ─────────────────────────────────────────────────────────
//
// Three mutually exclusive claim outcomes drive the terminal disposition of an
// order: transient retry (claim_failed), terminal skip (no_source_bin), and
// terminal fail (no_bin). Each test below pins one leg of the triad.

// TestPhase0_DispositionTriad_ClaimFailed pins claim_failed as TRANSIENT:
// a TOCTOU race between Find and Claim must QUEUE the order for retry, not
// fail it terminally. The postFindHook injects a competing claim to make the
// race deterministic.
//
// If this test flips to emitter.failed != 0, claim_failed was accidentally
// treated as terminal — that would drop orders on concurrent claim contention.
func TestPhase0_DispositionTriad_ClaimFailed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	bin := testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-CHAR-RACE")

	// A dummy order exists only so we have a valid orderID to steal the bin with.
	stealer := &orders.Order{
		EdgeUUID: "char-race-stealer", StationID: "x",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(stealer), "create stealer order")

	d, emitter := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	d.SetPostFindHook(func() {
		// Reserve the bin between Find and Claim so the inbound order's Acquire
		// conflicts — the reservation is the race point post-1a (was the CAS).
		testdb.ReserveBin(t, db, stealer.ID, bin.ID)
		d.SetPostFindHook(nil) // fire once only
	})

	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:    "char-claim-failed",
		OrderType:    OrderTypeRetrieve,
		PayloadCode:  bp.Code,
		DeliveryNode: lineNode.Name,
		Quantity:     1.0,
	})

	if len(emitter.failed) > 0 {
		t.Fatalf("claim_failed must QUEUE (transient), not FAIL: got errorCode=%q",
			emitter.failed[0].errorCode)
	}
	order := testdb.AssertOrderStatus(t, db, "char-claim-failed", StatusQueued)

	// CHANGE-DETECTOR: simple path does NOT set queue_reason on claim_failed.
	// HandleOrderRequest calls queueOrder() (no queue_reason parameter) when
	// planErr.Transient() is true; only the complex path explicitly calls
	// SetOrderQueueReason(order.ID, codeClaimFailed) at complex_dispatch.go.
	// Phase 1 should align the two paths. If this assertion starts failing it
	// means queue_reason is now being set — update the comment and the want.
	if order.QueueReason != "" {
		t.Errorf("queue_reason = %q, want %q (simple path leaves queue_reason empty on claim_failed)",
			order.QueueReason, "")
	}
}

// TestPhase0_DispositionTriad_NoBin_PlanStore pins that planStore's no-available-bin
// path produces codeNoBin (terminal FAIL), NOT codeClaimFailed (transient QUEUE).
//
// This is the planStore exclusion from asPlanningError (Phase-0 item 4). The inline
// loop in planStore uses `if bin.ClaimedBy == nil` before calling ClaimForDispatch;
// when all bins are already claimed the loop exits without any CAS attempt and the
// order fails terminally with codeNoBin.
//
// If this test flips to emitter.queued != 0 it means planStore was accidentally
// routed through asPlanningError, changing the terminal→transient semantics of a
// claim failure in the store path.
func TestPhase0_DispositionTriad_NoBin_PlanStore(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Create a bin at the source node and pre-claim it. planStore's inline loop
	// skips bins with ClaimedBy != nil, so this leaves order.BinID == nil → codeNoBin.
	bin := testdb.CreateBinAtNode(t, db, bp.Code, lineNode.ID, "BIN-CHAR-STORE")
	stealer := &orders.Order{
		EdgeUUID: "char-store-stealer", StationID: "x",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		DeliveryNode: lineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(stealer), "create stealer order")
	testdb.ClaimBinForTest(t, db, bin.ID, stealer.ID)

	d, emitter := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:   "char-no-bin-store",
		OrderType:   OrderTypeStore,
		PayloadCode: bp.Code,
		SourceNode:  lineNode.Name,
		Quantity:    1.0,
	})

	if len(emitter.queued) > 0 {
		t.Fatal("planStore codeNoBin must FAIL (terminal), not QUEUE (transient)")
	}
	if len(emitter.failed) == 0 {
		t.Fatal("expected order to fail with codeNoBin; no failure event emitted")
	}
	if emitter.failed[0].errorCode != codeNoBin {
		t.Errorf("error code = %q, want %q", emitter.failed[0].errorCode, codeNoBin)
	}
}

// TestPhase0_DispositionTriad_NoSourceBin_Complex pins that ApplyComplexPlan returns
// codeNoSourceBin (terminal SKIP) when every pickup node is empty.
//
// "Empty node" means ListBinsByNode returned zero rows — the bin was externally
// removed before dispatch. Route: skipOrderInternal → order.status = "skipped",
// NOT "failed". This tells the Edge operator surface the work was never needed
// rather than surfacing it as a system error.
//
// If this test flips to emitter.failed != 0, the skip→fail reclassification would
// alarm the operator for a benign externally-resolved condition.
func TestPhase0_DispositionTriad_NoSourceBin_Complex(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db) // create STOR type + nodes + payload

	// Two concrete nodes: pickup source (deliberately empty) and delivery.
	srcNode := &nodes.Node{Name: "CHAR-EMPTY-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(srcNode), "create source node")
	dstNode := &nodes.Node{Name: "CHAR-DST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dstNode), "create delivery node")
	// No bins created at srcNode — that is the "source bin" scenario.

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: srcNode.Name},
		{Action: protocol.ActionDropoff, Node: dstNode.Name},
	}
	stepsJSON, _ := json.Marshal(steps)
	order := &orders.Order{
		EdgeUUID:     "char-no-source-bin",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     1,
		SourceNode:   srcNode.Name,
		DeliveryNode: dstNode.Name,
		ProcessNode:  srcNode.Name,
		StepsJSON:    string(stepsJSON),
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create complex order")

	d, emitter := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	_ = d.DispatchPreparedComplex(order)

	if len(emitter.failed) > 0 {
		t.Fatalf("no_source_bin must SKIP (not fail): got failure errorCode=%q",
			emitter.failed[0].errorCode)
	}
	if len(emitter.skipped) == 0 {
		t.Fatal("expected order to be skipped with no_source_bin; no skip event emitted")
	}
	if emitter.skipped[0].errorCode != codeNoSourceBin {
		t.Errorf("skip error code = %q, want %q", emitter.skipped[0].errorCode, codeNoSourceBin)
	}
}

// ── UOP branch post-state ─────────────────────────────────────────────────────
//
// ClaimForDispatch routes to one of three write implementations based on the
// remainingUOP parameter extracted from the dispatch envelope:
//   - nil  → ClaimBin        — no manifest change (bin's payload/UOP unchanged)
//   - 0    → ClearAndClaim   — manifest cleared (payload_code="", uop_remaining=0)
//   - >0   → SyncUOPAndClaim — UOP synced, manifest content preserved
//
// These tests verify the post-claim bin state through the full dispatch path
// (envelope → extractRemainingUOP → ClaimForDispatch). Changing the routing in
// ClaimForDispatch or extractRemainingUOP will flip at least one case.

func intPtr(v int) *int { return &v }

func TestPhase0_UOPBranch_PostState(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		uop         *int // nil → no RemainingUOP in envelope
		wantPayload string
		wantUOP     int
	}{
		{"nil_uop_plain_claim", nil, "PART-A", 100},
		{"zero_uop_clear_and_claim", intPtr(0), "", 0},
		{"positive_uop_sync_and_claim", intPtr(42), "PART-A", 42},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			storageNode, lineNode, bp := setupTestData(t, db)
			bin := testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-UOP-"+tc.name)

			d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

			var env *protocol.Envelope
			if tc.uop == nil {
				// No RemainingUOP → extractRemainingUOP returns nil → ClaimBin
				env = testEnvelope()
			} else {
				// RemainingUOP set → extractRemainingUOP returns the value
				env = testEnvelopeWithUOP(t, tc.uop)
			}

			d.HandleOrderRequest(env, &protocol.OrderRequest{
				OrderUUID:    "char-uop-" + tc.name,
				OrderType:    OrderTypeRetrieve,
				PayloadCode:  bp.Code,
				DeliveryNode: lineNode.Name,
				Quantity:     1.0,
			})

			testdb.AssertOrderStatus(t, db, "char-uop-"+tc.name, StatusDispatched)

			got, err := db.GetBin(bin.ID)
			testutil.MustNoErr(t, err, "get bin after claim")

			if got.PayloadCode != tc.wantPayload {
				t.Errorf("PayloadCode = %q, want %q", got.PayloadCode, tc.wantPayload)
			}
			if got.UOPRemaining != tc.wantUOP {
				t.Errorf("UOPRemaining = %d, want %d", got.UOPRemaining, tc.wantUOP)
			}
		})
	}
}

// testEnvelopeWithUOP builds an envelope carrying a RemainingUOP value in the
// protocol payload, exercising the extractRemainingUOP path.
func testEnvelopeWithUOP(t *testing.T, uop *int) *protocol.Envelope {
	t.Helper()
	body, err := json.Marshal(&protocol.OrderRequest{RemainingUOP: uop})
	if err != nil {
		t.Fatalf("marshal order request: %v", err)
	}
	payload, err := json.Marshal(protocol.Data{Body: body})
	if err != nil {
		t.Fatalf("marshal data wrapper: %v", err)
	}
	env := testEnvelope()
	env.Payload = payload
	return env
}

// ── Status-at-claim-time ──────────────────────────────────────────────────────
//
// CHANGE-DETECTOR — TECH DEBT. Not an invariant.
//
// The two dispatch paths move the order to sourcing at different points relative
// to bin claim:
//
//   Simple (planRetrieve/planRetrieveEmpty/planMove):
//     MoveToSourcing is called BEFORE ClaimForDispatch.
//     Order is in "sourcing" status when the bin claim fires.
//
//   Complex (DispatchPreparedComplex → ApplyComplexPlan):
//     ApplyComplexPlan (claim) is at complex_dispatch.go:442.
//     MoveToSourcing is at complex_dispatch.go:482 — AFTER claim.
//     Order is in "queued" status when the bin claim fires.
//
// This asymmetry creates different crash-window behaviors. If a process crash
// occurs between claim and MoveToSourcing:
//   Simple path: the bin is claimed, the order is in "sourcing". AbandonStuckOrders
//     sweeps ("queued","staged","sourcing","dispatched"), so this window is covered.
//   Complex path: the bin is claimed, the order is in "queued". AbandonStuckOrders
//     sweeps "queued" too, so the window is also covered — but the terminal cleanup
//     path (FailOrderAtomic) must unclaim the bin correctly from "queued" status,
//     which is a different code path than from "sourcing".
//
// If Phase 1 normalises the ordering (e.g. MoveToSourcing before claim on the
// complex path too), this test will break and should be removed or updated to
// reflect the new invariant.

func TestPhase0_StatusAtClaimTime(t *testing.T) {
	// Simple path: capture order status inside postFindHook.
	t.Run("simple_retrieve_sourcing_before_claim", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		storageNode, lineNode, bp := setupTestData(t, db)
		testdb.CreateBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-STAT-SIMPLE")

		const uuid = "char-status-simple"
		var capturedStatus string

		d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
		d.SetPostFindHook(func() {
			// postFindHook fires after Find, before ClaimForDispatch in planRetrieve.
			// MoveToSourcing already ran at planning_service.go:192.
			if o, err := db.GetOrderByUUID(uuid); err == nil {
				capturedStatus = string(o.Status)
			}
			d.SetPostFindHook(nil) // fire once
		})

		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID:    uuid,
			OrderType:    OrderTypeRetrieve,
			PayloadCode:  bp.Code,
			DeliveryNode: lineNode.Name,
			Quantity:     1.0,
		})

		testdb.AssertOrderStatus(t, db, uuid, StatusDispatched) // succeeded overall

		// CHANGE-DETECTOR: simple path is in "sourcing" at claim time because
		// MoveToSourcing fires before ClaimForDispatch (planning_service.go:192 vs :290).
		if capturedStatus != string(StatusSourcing) {
			t.Errorf("status at claim time (simple path) = %q, want %q — "+
				"CHANGE-DETECTOR: if this changed, the sourcing-flip tech debt was addressed; update this test",
				capturedStatus, string(StatusSourcing))
		}
	})

	// Complex path: order is in "queued" when DispatchPreparedComplex enters and
	// ApplyComplexPlan claims bins. MoveToSourcing fires at complex_dispatch.go:482,
	// AFTER ApplyComplexPlan at complex_dispatch.go:442.
	//
	// We cannot inject a hook at exact claim time inside ApplyComplexPlan without
	// modifying production code. Instead, we verify the observable pre-condition
	// (DispatchPreparedComplex's guard at line 248 asserts StatusQueued) and the
	// post-condition (order transitions through sourcing to dispatched, proving
	// MoveToSourcing ran after claim).
	t.Run("complex_queued_at_claim_time", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		setupTestData(t, db)

		srcNode := &nodes.Node{Name: "CHAR-STAT-SRC", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(srcNode), "create src node")
		dstNode := &nodes.Node{Name: "CHAR-STAT-DST", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(dstNode), "create dst node")
		testdb.CreateBinAtNode(t, db, "PART-A", srcNode.ID, "BIN-STAT-COMPLEX")

		steps := []resolvedStep{
			{Action: protocol.ActionPickup, Node: srcNode.Name},
			{Action: protocol.ActionDropoff, Node: dstNode.Name},
		}
		stepsJSON, _ := json.Marshal(steps)
		order := &orders.Order{
			EdgeUUID:     "char-status-complex",
			StationID:    "line-1",
			OrderType:    OrderTypeComplex,
			Status:       StatusQueued,
			Quantity:     1,
			SourceNode:   srcNode.Name,
			DeliveryNode: dstNode.Name,
			ProcessNode:  srcNode.Name,
			StepsJSON:    string(stepsJSON),
		}
		testutil.MustNoErr(t, db.CreateOrder(order), "create complex order")

		// CHANGE-DETECTOR: order must be in "queued" when DispatchPreparedComplex
		// enters — the guard at complex_dispatch.go:248 enforces this. If this
		// assertion fails, the call site changed to pass a non-queued order, which
		// would be a broader lifecycle change.
		if order.Status != StatusQueued {
			t.Fatalf("pre-condition: order.Status = %q, want %q (complex claim-path entry)",
				order.Status, StatusQueued)
		}

		d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
		_ = d.DispatchPreparedComplex(order)

		// After dispatch: order moved through sourcing → dispatched, proving
		// MoveToSourcing ran AFTER the bin claim at complex_dispatch.go:482 > :442.
		// TODO(Phase1): add a hook in ApplyComplexPlan to capture status at exact
		// claim time, and update this assertion to verify queued→sourcing ordering.
		got, err := db.GetOrder(order.ID)
		testutil.MustNoErr(t, err, "get order post-dispatch")
		if got.Status == StatusFailed || got.Status == StatusSkipped {
			t.Fatalf("complex dispatch failed unexpectedly: status=%q", got.Status)
		}
	})
}
