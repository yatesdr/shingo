// runtime_uop_binding_test.go — regression coverage for the runtime
// UOP cache binding model (hold-and-replay). Pins the contract:
//
//   - Cache writes happen at operator action (release-click finalizes the
//     OLD bin per disposition; FinalizeProduceNode) and at delivered (the
//     OrderDelivered envelope seeds active_bin_id + count + epoch). NEVER
//     at confirm.
//   - Release does NOT pre-load the incoming bin or stamp a second pointer;
//     the new bin's count+epoch arrive on its OrderDelivered envelope.
//   - PLC ticks attribute to the bin physically at the slot (active_bin_id).
//     When no bin is bound (the pickup→delivery gap) the bin portion of each
//     tick is HELD in pending_uop_delta (durable) and replayed onto the next
//     bin when it binds; lineside still drains every tick.
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

	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// mockBinUOPServer answers GET /api/bins/uop?id=N with {found,
// uop_remaining}. Missing keys return found=false (Core's "confirmed
// empty" branch).
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

// ─────────────────────────────────────────────────────────────────────────
// Release-click contract (hold-and-replay model)
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_ReleaseDoesNotPreloadIncomingBin pins the model: a
// release-click finalizes the OLD bin's count per the disposition (here
// RELEASE EMPTY → 0) and does NOT pre-load the incoming supply bin's count
// or stamp it as the active/cached pointer. The incoming bin's count+epoch
// arrive later on its OrderDelivered envelope.
func TestRuntimeBinding_ReleaseDoesNotPreloadIncomingBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "RC-NOPRE", PayloadCode: "PART-NP", UOPCapacity: 1200, InitialUOP: 800,
	})
	// Old bin physically at the slot; cache tracks it.
	oldBin := int64(8000)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &oldBin, 800)

	// Incoming supply bin whose UOP, in the OLD model, would have been
	// pre-loaded into the cache at click. Stand up a Core stub returning
	// that value to prove the new release path does NOT consult or apply it.
	const incomingBin int64 = 9001
	srv := mockBinUOPServer(t, map[int64]int{incomingBin: 1200})
	eng := testEngine(t, db)
	eng.coreClient = NewCoreClient(srv.URL)

	orderID, err := db.CreateOrder("uuid-rc-nopre", orders.TypeRetrieve,
		&nodeID, false, 1, "RC-NOPRE-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := incomingBin
	db.UpdateOrderBinID(orderID, &bid)
	db.UpdateProcessNodeRuntimeOrders(nodeID, &orderID, nil)

	disp := ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: map[string]int{}}
	testutil.MustNoErr(t, eng.ReleaseOrderWithLineside(orderID, disp), "release")

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 0 {
		t.Errorf("cache = %d, want 0 (RELEASE EMPTY finalizes the OLD bin; the incoming 1200 must NOT be pre-loaded)", rt.RemainingUOPCached)
	}
	if rt.CachedBinID != nil && *rt.CachedBinID == incomingBin {
		t.Errorf("CachedBinID = %d — release must not pre-load/stamp the incoming bin", *rt.CachedBinID)
	}
	_ = claimID
}

// ─────────────────────────────────────────────────────────────────────────
// Delivered-event contract
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_DeliveredFlipsCacheAndPointers pins §2: bin
// physically arrives, the delivered handler binds active_bin_id and sets
// cache + epoch to the values carried on the OrderDelivered envelope.
func TestRuntimeBinding_DeliveredFlipsCacheAndPointers(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "DEL-FLIP", PayloadCode: "PART-DF", UOPCapacity: 1200, InitialUOP: 0,
	})

	const deliveredBinID int64 = 9301
	const deliveredBinUOP = 1180 // partial; verifies bin's actual count, not capacity
	const deliveredEpoch int64 = 52
	eng := testEngine(t, db)
	eng.wireEventHandlers()

	orderID, err := db.CreateOrder("uuid-del-flip", orders.TypeRetrieve,
		&nodeID, false, 1, "DEL-FLIP-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	bid := deliveredBinID
	db.UpdateOrderBinID(orderID, &bid)

	// Seed rides the OrderDelivered envelope (Core's snapshot at arrival):
	// bin uop + load-lifecycle epoch. No HTTP pull.
	uop := deliveredBinUOP
	eng.Events.Emit(Event{
		Type: EventOrderDelivered,
		Payload: OrderDeliveredEvent{
			OrderID: orderID, OrderUUID: "uuid-del-flip", OrderType: orders.TypeRetrieve,
			ProcessNodeID: &nodeID, BinID: &bid, BinUOP: &uop, BinEpoch: deliveredEpoch,
		},
	})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != deliveredBinUOP {
		t.Errorf("cache = %d, want %d (bin uop from delivery envelope)", rt.RemainingUOPCached, deliveredBinUOP)
	}
	// The epoch must land on the runtime so binAtNode stamps tick deltas
	// with the right generation (the R68 / always-0 fix).
	if rt.ActiveBinEpoch != deliveredEpoch {
		t.Errorf("ActiveBinEpoch = %d, want %d (epoch from delivery envelope)", rt.ActiveBinEpoch, deliveredEpoch)
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != deliveredBinID {
		t.Errorf("ActiveBinID = %v, want %d", rt.ActiveBinID, deliveredBinID)
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

	// Seed a bin physically at the slot with a known count.
	supplyBin := int64(9401)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &supplyBin, seededUOP)

	eng := testEngine(t, db)
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
	if rt.ActiveBinID == nil || *rt.ActiveBinID != supplyBin {
		t.Errorf("ActiveBinID = %v, want %d (removal delivery must not overwrite the at-slot bin)", rt.ActiveBinID, supplyBin)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// PLC tick attribution: hold-and-replay
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_PLCTicksHoldWhenNoBinThenReplayOntoNextBin is the
// load-bearing test for the model. While no bin is bound (pickup→delivery
// gap) the bin portion of each tick is HELD in pending_uop_delta (durably
// on the runtime row), not lost or charged to a departed bin. When the
// next bin's OrderDelivered binds it, the first tick replays the held
// total onto it with the bin's epoch.
func TestRuntimeBinding_PLCTicksHoldWhenNoBinThenReplayOntoNextBin(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "HOLD-REPLAY", PayloadCode: "PART-HR", UOPCapacity: 1200, InitialUOP: 0,
	})
	// Gap: no bin bound (active nil) — old bin picked up, new not delivered.
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, nil, 0)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	// Three ticks during the gap — no bin to attribute to. They must be
	// held (durable), not dropped, and must not touch the cache.
	for i := 0; i < 3; i++ {
		eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
			ProcessID: processID, StyleID: styleID, Delta: 1,
		}})
	}
	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.PendingUOPDelta != 3 {
		t.Fatalf("PendingUOPDelta = %d, want 3 (gap ticks held, durable on the runtime row)", rt.PendingUOPDelta)
	}
	if rt.RemainingUOPCached != 0 {
		t.Errorf("cache = %d, want 0 (held ticks must not touch the cache while unbound)", rt.RemainingUOPCached)
	}

	// New bin arrives: OrderDelivered seeds active_bin_id + count + epoch.
	const newBin int64 = 9502
	const newBinUOP = 1000
	uop := newBinUOP
	orderID, _ := db.CreateOrder("uuid-hold-replay", orders.TypeRetrieve,
		&nodeID, false, 1, "HOLD-REPLAY-NODE", "", "", "", false, "")
	bid := newBin
	db.UpdateOrderBinID(orderID, &bid)
	eng.Events.Emit(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{
		OrderID: orderID, OrderUUID: "uuid-hold-replay", OrderType: orders.TypeRetrieve,
		ProcessNodeID: &nodeID, BinID: &bid, BinUOP: &uop, BinEpoch: 7,
	}})

	// First tick after binding replays the 3 held + this 1 = 4 onto the new bin.
	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 1,
	}})
	rt, _ = db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != newBinUOP-4 {
		t.Errorf("cache = %d, want %d (1000 - (3 held + 1) replayed onto the new bin)", rt.RemainingUOPCached, newBinUOP-4)
	}
	if rt.PendingUOPDelta != 0 {
		t.Errorf("PendingUOPDelta = %d, want 0 (held delta consumed on replay)", rt.PendingUOPDelta)
	}
}

// TestRuntimeBinding_PLCTicksAfterPickupOnlyDrainLineside pins the
// post-pickup state where the tick is fully covered by lineside: active
// is nil, a lineside bucket has enough to absorb the whole delta, so the
// bin portion is 0 (nothing held) and the cache is untouched.
func TestRuntimeBinding_PLCTicksAfterPickupOnlyDrainLineside(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "GAP-PICKUP", PayloadCode: "PART-GP", UOPCapacity: 1200, InitialUOP: 1200,
	})

	// Post-pickup gap: active is nil.
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, nil, 1200)

	// Seed lineside bucket large enough to absorb the whole delta.
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
	if rt.PendingUOPDelta != 0 {
		t.Errorf("PendingUOPDelta = %d, want 0 (delta fully drained from lineside; no bin portion to hold)", rt.PendingUOPDelta)
	}
}

// TestRuntimeBinding_PLCTicksResumeCacheDecrementAfterDelivery pins
// that once a bin is bound, ticks decrement the cache normally.
func TestRuntimeBinding_PLCTicksResumeCacheDecrementAfterDelivery(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, nodeID, styleID, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "GAP-RESUME", PayloadCode: "PART-GR", UOPCapacity: 1200, InitialUOP: 1200,
	})

	bin := int64(9701)
	// Bin bound at the slot.
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 1200)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	eng.Events.Emit(Event{Type: EventCounterDelta, Payload: CounterDeltaEvent{
		ProcessID: processID, StyleID: styleID, Delta: 3,
	}})

	rt, _ := db.GetProcessNodeRuntime(nodeID)
	if rt.RemainingUOPCached != 1197 {
		t.Errorf("cache = %d, want 1197 (1200-3, bound bin should decrement)", rt.RemainingUOPCached)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Confirm contract: zero cache writes
// ─────────────────────────────────────────────────────────────────────────

// TestRuntimeBinding_ConfirmDoesNotTouchCache pins the user's
// requirement that StatusConfirmed is a pure operator-semantic event
// with no cache side-effects. EventOrderCompleted alone (without a
// preceding EventOrderDelivered) must not flip cache or active_bin_id.
func TestRuntimeBinding_ConfirmDoesNotTouchCache(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "CONF-NOOP", PayloadCode: "PART-CN", UOPCapacity: 1200, InitialUOP: 600,
	})
	const seededUOP = 600
	seededBin := int64(9801)
	db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &seededBin, seededUOP)

	eng := testEngine(t, db)
	eng.wireEventHandlers()

	orderID, _ := db.CreateOrder("uuid-conf-noop", orders.TypeRetrieve,
		&nodeID, false, 1, "CONF-NOOP-NODE", "", "", "", false, "")
	differentBin := int64(9802)
	db.UpdateOrderBinID(orderID, &differentBin)

	// Fire EventOrderCompleted directly (no preceding delivery event).
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
