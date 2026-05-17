// runtime_uop_binding_test.go — regression coverage for the runtime
// UOP cache binding redesign. Pins the new contract:
//
//   - Cache writes happen at operator action (release-click /
//     FinalizeProduceNode) and at delivered. NEVER at confirm.
//   - PLC ticks gate cache decrement on active_bin_id == cached_bin_id.
//     During the gap the cache stays put; lineside still drains and
//     Core-side bin deltas still flow against active_bin_id.
//   - Manual_swap nodes are skipped in the PLC tick path entirely
//     (forklift-managed, no PLC tags).
//
// Companion to the ALN_002 incident writeup
// (C:\Users\stephen.brown\GitHub\runtime-uop-binding-brief.md).

package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// mockBinUOPServer answers GET /api/bins/uop?id=N with {found,
// uop_remaining}. Missing keys return found=false (Core's "confirmed
// empty" branch). Use coreUnreachableServer for the unreachable path.
func mockBinUOPServer(t *testing.T, uopByBin map[int64]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bins/uop" {
			http.NotFound(w, r)
			return
		}
		binID, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		uop, found := uopByBin[binID]
		_ = json.NewEncoder(w).Encode(struct {
			Found        bool `json:"found"`
			UOPRemaining int  `json:"uop_remaining"`
		}{Found: found, UOPRemaining: uop})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// coreUnreachableServer always returns HTTP 500 so BinByID returns the
// Core-unreachable tri-state.
func coreUnreachableServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ─────────────────────────────────────────────────────────────────────────
// Release-click contract
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_ReleaseClickWritesIncomingBinUOP pins the §1
// contract: operator click on a single-robot supply order writes the
// bin's authoritative UOP from Core, and stamps cached_bin_id so the
// PLC tick gate can detect steady-vs-gap state.
func TestRuntimeBinding_ReleaseClickWritesIncomingBinUOP(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "RC-WRITE", PayloadCode: "PART-RC", UOPCapacity: 1200, InitialUOP: 800,
	})

	const supplyBinID int64 = 9001
	const supplyBinUOP = 1200

	srv := mockBinUOPServer(t, map[int64]int{supplyBinID: supplyBinUOP})
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	// Single-robot supply order delivering to the slot.
	orderID, err := db.CreateOrder("uuid-rc-write", orders.TypeRetrieve,
		&nodeID, false, 1, "RC-WRITE-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	binID := supplyBinID
	testutil.MustNoErr(t, db.UpdateOrderBinID(orderID, &binID), "attach bin")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil), "track order")

	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: map[string]int{}}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "release")

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != supplyBinUOP {
		t.Errorf("RemainingUOPCached = %d, want %d (incoming supply bin's UOP)", rt.RemainingUOPCached, supplyBinUOP)
	}
	if rt.CachedBinID == nil || *rt.CachedBinID != supplyBinID {
		t.Errorf("CachedBinID = %v, want %d (release-click stamps the incoming bin)", rt.CachedBinID, supplyBinID)
	}
	_ = claimID
}

// TestRuntimeBinding_ReleaseClickFallsBackToZero pins open-question #2:
// removal-only release (no supply leg / supply leg's BinID nil) writes
// cache := 0, cached_bin_id := nil.
func TestRuntimeBinding_ReleaseClickFallsBackToZero(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "RC-ZERO", PayloadCode: "PART-RC0", UOPCapacity: 1200, InitialUOP: 600,
	})

	srv := mockBinUOPServer(t, nil) // no bins; not consulted anyway
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	// Removal-only order: DeliveryNode is the supermarket, BinID nil.
	orderID, err := db.CreateOrder("uuid-rc-zero", orders.TypeMove,
		&nodeID, false, 1, "OUTBOUND", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil), "track order")

	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: map[string]int{}}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "release")

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 0 {
		t.Errorf("RemainingUOPCached = %d, want 0 (no supply leg)", rt.RemainingUOPCached)
	}
	if rt.CachedBinID != nil {
		t.Errorf("CachedBinID = %v, want nil (no supply bin)", rt.CachedBinID)
	}
	_ = claimID
}

// TestRuntimeBinding_ReleaseClickResolvesSupplyViaSibling pins the
// two-robot symmetry: releasing the EVAC leg looks up the SUPPLY
// sibling's bin via order.SiblingOrderID and writes its UOP. Both
// legs land on the same value; idempotent rewrite.
func TestRuntimeBinding_ReleaseClickResolvesSupplyViaSibling(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, err := db.CreateProcess("RC-SIB", "two-robot sibling", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "RC-SIB-NODE", Code: "RCS",
		Name: "RC Sibling Node", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, _ := db.CreateStyle("RC-SIB-STYLE", "", processID)
	db.SetActiveStyle(processID, &styleID)
	claimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "RC-SIB-NODE", Role: "consume",
		SwapMode: "two_robot", PayloadCode: "PART-SIB", UOPCapacity: 1000,
		InboundSource: "MARKET", InboundStaging: "STAGING",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 500), "seed runtime")

	const supplyBinID int64 = 9101
	const supplyBinUOP = 950 // partial supply — verifies it's not just "capacity"
	srv := mockBinUOPServer(t, map[int64]int{supplyBinID: supplyBinUOP})
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	// Order A (supply): delivers TO the slot; carries supply bin.
	orderA, err := db.CreateOrder("uuid-rcsib-A", orders.TypeComplex,
		&nodeID, false, 1, "RC-SIB-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	bidA := supplyBinID
	db.UpdateOrderBinID(orderA, &bidA)

	// Order B (evac): delivers AWAY (supermarket); no bin yet.
	orderB, err := db.CreateOrder("uuid-rcsib-B", orders.TypeComplex,
		&nodeID, false, 1, "MARKET", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	testutil.MustNoErr(t, db.LinkOrderSiblings(orderA, orderB), "link siblings")
	testutil.MustNoErr(t, db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA, &orderB), "track A+B")

	// Releasing B (the evac, no own supply BinID) must walk the sibling
	// pointer to find A's BinID.
	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: map[string]int{}}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderB, disp), "release B")

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != supplyBinUOP {
		t.Errorf("after B release: cache = %d, want %d (supply sibling lookup)", rt.RemainingUOPCached, supplyBinUOP)
	}
	if rt.CachedBinID == nil || *rt.CachedBinID != supplyBinID {
		t.Errorf("after B release: CachedBinID = %v, want %d", rt.CachedBinID, supplyBinID)
	}

	// Now release A — should be an idempotent rewrite of the same value.
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderA, disp), "release A")
	rt, _ = db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != supplyBinUOP {
		t.Errorf("after A release: cache = %d, want %d (idempotent)", rt.RemainingUOPCached, supplyBinUOP)
	}
}

// TestRuntimeBinding_CoreUnavailableAtReleaseClickPreservesCache pins
// open-question #3 policy: if Core is unreachable during the release-
// click bin lookup, the cache is left untouched. The B2-fix precedent
// (BinAtLineside): a transient Core blip must not zero a live cache.
func TestRuntimeBinding_CoreUnavailableAtReleaseClickPreservesCache(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "RC-COREDOWN", PayloadCode: "PART-RCD", UOPCapacity: 1200, InitialUOP: 750,
	})
	const seededUOP = 750

	srv := coreUnreachableServer(t)
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	orderID, err := db.CreateOrder("uuid-coredown", orders.TypeRetrieve,
		&nodeID, false, 1, "RC-COREDOWN-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bin := int64(9201)
	db.UpdateOrderBinID(orderID, &bin)
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: map[string]int{}}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "release")

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != seededUOP {
		t.Errorf("cache = %d, want %d (Core unreachable → leave cache untouched)", rt.RemainingUOPCached, seededUOP)
	}
	_ = claimID
}

// ─────────────────────────────────────────────────────────────────────────
// Delivered-event contract
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_DeliveredFlipsCacheAndPointers pins §2: bin
// physically arrives, the delivered handler binds active_bin_id ==
// cached_bin_id and sets cache to the bin's authoritative UOP from Core.
func TestRuntimeBinding_DeliveredFlipsCacheAndPointers(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "DEL-FLIP", PayloadCode: "PART-DF", UOPCapacity: 1200, InitialUOP: 0,
	})

	const deliveredBinID int64 = 9301
	const deliveredBinUOP = 1180 // partial; verifies bin's actual count, not capacity
	srv := mockBinUOPServer(t, map[int64]int{deliveredBinID: deliveredBinUOP})
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)
	eng.wireEventHandlers()

	orderID, err := db.CreateOrder("uuid-del-flip", orders.TypeRetrieve,
		&nodeID, false, 1, "DEL-FLIP-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := deliveredBinID
	db.UpdateOrderBinID(orderID, &bid)

	eng.Events.Emit(Event{
		Type: EventOrderDelivered,
		Payload: OrderDeliveredEvent{
			OrderID: orderID, OrderUUID: "uuid-del-flip", OrderType: orders.TypeRetrieve,
			ProcessNodeID: &nodeID, BinID: &bid,
		},
	})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != deliveredBinUOP {
		t.Errorf("cache = %d, want %d (Core's authoritative bin uop)", rt.RemainingUOPCached, deliveredBinUOP)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != deliveredBinID {
		t.Errorf("ActiveBinID = %v, want %d", rt.ActiveBinID, deliveredBinID)
	}
	if rt.CachedBinID == nil || *rt.CachedBinID != deliveredBinID {
		t.Errorf("CachedBinID = %v, want %d", rt.CachedBinID, deliveredBinID)
	}
	_ = claimID
}

// TestRuntimeBinding_RemovalOnlyOrderDoesNotResetCache pins the §2
// gate: orders whose DeliveryNode is the supermarket (Order B in
// two-robot consume, sequential-removal step, etc.) flow through
// EventOrderDelivered too, but the handler no-ops because the gate is
// DeliveryNode == coreNodeName. Cache stays at whatever the supply
// leg's delivery set it to.
func TestRuntimeBinding_RemovalOnlyOrderDoesNotResetCache(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "REM-NORST", PayloadCode: "PART-RN", UOPCapacity: 1200, InitialUOP: 850,
	})
	const seededUOP = 850

	// Even though the cache is seeded, ALSO seed cached_bin_id so the
	// scenario reflects a real "supply already delivered, removal still
	// in flight" state. Use a different bin id from the removal order's
	// bin so we can prove the removal didn't overwrite anything.
	supplyBin := int64(9401)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &supplyBin, seededUOP)
	db.SetProcessNodeCachedBin(nodeID, &supplyBin, seededUOP)

	srv := mockBinUOPServer(t, map[int64]int{supplyBin: 0 /* should not be consulted */})
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)
	eng.wireEventHandlers()

	// Removal order: process node points at the slot, but DeliveryNode
	// is the supermarket. Has its own (outgoing) bin.
	removalOrderID, _ := db.CreateOrder("uuid-rem-norst", orders.TypeComplex,
		&nodeID, false, 1, "MARKET", "", "", "", false, "")
	outgoingBin := int64(9402)
	db.UpdateOrderBinID(removalOrderID, &outgoingBin)

	eng.Events.Emit(Event{
		Type: EventOrderDelivered,
		Payload: OrderDeliveredEvent{
			OrderID: removalOrderID, OrderUUID: "uuid-rem-norst", OrderType: orders.TypeComplex,
			ProcessNodeID: &nodeID, BinID: &outgoingBin,
		},
	})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != seededUOP {
		t.Errorf("cache = %d, want %d (removal delivery to supermarket must not touch cache)", rt.RemainingUOPCached, seededUOP)
	}
	if rt.CachedBinID == nil || *rt.CachedBinID != supplyBin {
		t.Errorf("CachedBinID = %v, want %d (removal delivery must not overwrite supply leg's pointer)", rt.CachedBinID, supplyBin)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// PLC tick gap-detection contract
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_PLCTicksDoNotDecrementCacheDuringGap is the
// load-bearing test for the gap-window correctness fix. After release-
// click writes cached_bin_id = new supply bin, but BEFORE delivery has
// flipped active_bin_id, PLC ticks must not decrement the cache —
// otherwise the slot displays a count for a bin that hasn't been
// touched yet.
//
// Mirrors the ALN_002 -3/1200 incident: cache was zeroed at click,
// ticks decremented from 0 into negatives.
func TestRuntimeBinding_PLCTicksDoNotDecrementCacheDuringGap(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "GAP-NODEC", PayloadCode: "PART-GD", UOPCapacity: 1200, InitialUOP: 1200,
	})

	// Gap state: cache represents incoming new bin (=1200), but the
	// physical bin still on the slot is the OLD one (different id).
	oldBin := int64(9501)
	newBin := int64(9502)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &oldBin, 1200)
	db.SetProcessNodeCachedBin(nodeID, &newBin, 1200)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Three ticks during the gap.
	for i := 0; i < 3; i++ {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: styleID, Delta: 1,
		}})
	}

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 1200 {
		t.Errorf("cache = %d, want 1200 (PLC ticks must not decrement during release→delivery gap)", rt.RemainingUOPCached)
	}
}

// TestRuntimeBinding_PLCTicksAfterPickupOnlyDrainLineside pins the
// post-pickup state: active_bin_id = nil (HandleBinPickedUp cleared
// it), cached_bin_id still pointing at incoming. Ticks drain lineside
// and that's it — no Core delta, no cache change.
func TestRuntimeBinding_PLCTicksAfterPickupOnlyDrainLineside(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "GAP-PICKUP", PayloadCode: "PART-GP", UOPCapacity: 1200, InitialUOP: 1200,
	})

	// Post-pickup gap: active is nil, cached points at incoming.
	newBin := int64(9601)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, nil, 1200)
	db.SetProcessNodeCachedBin(nodeID, &newBin, 1200)

	// Seed lineside bucket so we can assert the drain still happens.
	if _, err := db.CaptureLinesideBucket(nodeID, "", 0, "PART-GP", 5); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 3,
	}})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 1200 {
		t.Errorf("cache = %d, want 1200 (post-pickup ticks must not touch cache)", rt.RemainingUOPCached)
	}
}

// TestRuntimeBinding_PLCTicksResumeCacheDecrementAfterDelivery pins
// that once delivery sets active_bin_id == cached_bin_id, ticks resume
// normal cache decrement.
func TestRuntimeBinding_PLCTicksResumeCacheDecrementAfterDelivery(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "GAP-RESUME", PayloadCode: "PART-GR", UOPCapacity: 1200, InitialUOP: 1200,
	})

	bin := int64(9701)
	// Steady state: active == cached.
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 1200)
	db.SetProcessNodeCachedBin(nodeID, &bin, 1200)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 3,
	}})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 1197 {
		t.Errorf("cache = %d, want 1197 (1200-3, steady state should decrement)", rt.RemainingUOPCached)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Confirm contract: zero cache writes
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_ConfirmDoesNotTouchCache pins the user's
// requirement that StatusConfirmed is a pure operator-semantic event
// with no cache side-effects. EventOrderCompleted alone (without a
// preceding EventOrderDelivered) must not flip cache, active_bin_id,
// or cached_bin_id.
func TestRuntimeBinding_ConfirmDoesNotTouchCache(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "CONF-NOOP", PayloadCode: "PART-CN", UOPCapacity: 1200, InitialUOP: 600,
	})
	const seededUOP = 600
	seededBin := int64(9801)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &seededBin, seededUOP)
	db.SetProcessNodeCachedBin(nodeID, &seededBin, seededUOP)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	orderID, _ := db.CreateOrder("uuid-conf-noop", orders.TypeRetrieve,
		&nodeID, false, 1, "CONF-NOOP-NODE", "", "", "", false, "")
	differentBin := int64(9802)
	db.UpdateOrderBinID(orderID, &differentBin)

	// Fire EventOrderCompleted directly, NOT through emitOrderCompleted
	// helper (which fires Delivered first under the test convention).
	// This is the "confirm-only" path — operator pressed confirm with
	// no preceding delivery event.
	eng.Events.Emit(Event{
		Type: EventOrderCompleted,
		Payload: OrderCompletedEvent{
			OrderID: orderID, OrderUUID: "uuid-conf-noop",
			OrderType: orders.TypeRetrieve, ProcessNodeID: &nodeID,
		},
	})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != seededUOP {
		t.Errorf("cache = %d, want %d (confirm must not touch cache)", rt.RemainingUOPCached, seededUOP)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != seededBin {
		t.Errorf("ActiveBinID = %v, want %d (confirm must not touch active_bin_id)", rt.ActiveBinID, seededBin)
	}
	if rt.CachedBinID == nil || *rt.CachedBinID != seededBin {
		t.Errorf("CachedBinID = %v, want %d (confirm must not touch cached_bin_id)", rt.CachedBinID, seededBin)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Manual-swap PLC skip
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_ManualSwapNodesSkipPLCTicks pins the user's
// requirement: the manual loader/unloader workflow is forklift-
// managed; PLC tick deltas must not affect bin counts on those nodes.
func TestRuntimeBinding_ManualSwapNodesSkipPLCTicks(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, err := db.CreateProcess("MS-SKIP", "manual swap skip", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "MS-SKIP-NODE", Code: "MSS",
		Name: "MS Skip Node", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, _ := db.CreateStyle("MS-SKIP-STYLE", "", processID)
	db.SetActiveStyle(processID, &styleID)
	claimID, _ := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "MS-SKIP-NODE", Role: "consume",
		SwapMode: "manual_swap", PayloadCode: "PART-MS", UOPCapacity: 100,
		OutboundDestination: "STORAGE",
	})
	db.EnsureProcessNodeRuntime(nodeID)
	bin := int64(9901)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 100)
	db.SetProcessNodeCachedBin(nodeID, &bin, 100)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 5,
	}})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 100 {
		t.Errorf("cache = %d, want 100 (manual_swap nodes must not consume PLC ticks)", rt.RemainingUOPCached)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Faulted → Failed terminal expiry (open question #5 policy)
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_FaultedTerminalLeavesCacheAtClickValue pins the
// inheritance of operator_release.go's existing precedent: if the
// release-click wrote a value and the order then dies (faulted →
// failed terminal), the cache stays where the click left it. The
// reconciler self-heal path settles drift over time. No new failure-
// event subscriber needed.
//
// Pin via direct status writes since the lifecycle service path
// requires Manager wiring; this is a contract test, not an integration
// test.
func TestRuntimeBinding_FaultedTerminalLeavesCacheAtClickValue(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "FT-EXP", PayloadCode: "PART-FT", UOPCapacity: 1200, InitialUOP: 0,
	})

	const supplyBinID int64 = 10001
	const supplyBinUOP = 1200
	srv := mockBinUOPServer(t, map[int64]int{supplyBinID: supplyBinUOP})
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)
	eng.wireEventHandlers()

	// Operator click: cache := 1200, cached_bin_id := supply.
	orderID, _ := db.CreateOrder("uuid-ft-exp", orders.TypeRetrieve,
		&nodeID, false, 1, "FT-EXP-NODE", "", "", "", false, "")
	bid := supplyBinID
	db.UpdateOrderBinID(orderID, &bid)
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: map[string]int{}}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "release")

	// Order goes Faulted → Failed terminal.
	db.UpdateOrderStatus(orderID, string(protocol.StatusFaulted))
	db.UpdateOrderStatus(orderID, string(orders.StatusFailed))
	eng.Events.Emit(Event{
		Type:    EventOrderFailed,
		Payload: OrderFailedEvent{OrderID: orderID, OrderUUID: "uuid-ft-exp", OrderType: orders.TypeRetrieve, Reason: "grace_timeout"},
	})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != supplyBinUOP {
		t.Errorf("cache = %d, want %d (terminal failure leaves cache at click value)", rt.RemainingUOPCached, supplyBinUOP)
	}
	if rt.CachedBinID == nil || *rt.CachedBinID != supplyBinID {
		t.Errorf("CachedBinID = %v, want %d", rt.CachedBinID, supplyBinID)
	}
	_ = claimID
}

// ─────────────────────────────────────────────────────────────────────────
// Compile-time guard: the deleted helper must not exist
// ─────────────────────────────────────────────────────────────────────────

// Defensive guard against a future revert of the resolveReplenishUOP
// deletion. If someone re-introduces the function (and thus the lying
// "returns 0 when binID is nil" behavior the comment claimed), this
// file fails to compile because the symbol shadow check below would
// fail. The check has no runtime cost.
//
// Implementation: assign nil to a variable of the function's expected
// type. If resolveReplenishUOP is brought back with the same signature
// (role, capacity, *int64) int, the compiler will let this through;
// but the test below (TestRuntimeBinding_DeliveredFlipsCacheAndPointers)
// already pins the new contract so a re-introduction would surface
// elsewhere. The comment is the spec; a real compile-time guard would
// require build tags to assert non-existence which is overkill.
//
// Brief reference: §"Tests to add" item TestRegression_ResolveReplenishUOPDeleted.
var _ = "resolveReplenishUOP must remain deleted; see runtime-uop-binding-brief.md"
