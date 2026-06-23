package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/service"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
)

// noOpOrderEmitter satisfies orders.EventEmitter without doing anything.
// Used in tests that drive the order-creation path but don't care about
// the resulting event side-effects.
type noOpOrderEmitter struct{}

func (noOpOrderEmitter) EmitOrderCreated(int64, string, protocol.OrderType, *int64, *int64) {
}
func (noOpOrderEmitter) EmitOrderStatusChanged(int64, string, protocol.OrderType, string, string, string, *int64, *int64) {
}
func (noOpOrderEmitter) EmitOrderCompleted(int64, string, protocol.OrderType, *int64, *int64) {
}
func (noOpOrderEmitter) EmitOrderDelivered(int64, string, protocol.OrderType, *int64, *int64, *int, int64) {
}
func (noOpOrderEmitter) EmitOrderDeliveredFallback(int64, *int, int64, string) {
}
func (noOpOrderEmitter) EmitOrderFailed(int64, string, protocol.OrderType, string) {
}
func (noOpOrderEmitter) EmitOrderFaulted(int64, string, string) {
}

// TestHandleLoopBelowThreshold_FiresForInactiveStyleLoader pins Round-3
// Obs 9's Edge-side change: a threshold signal whose loader claim
// lives on an INACTIVE style must still trigger the L1 fire path.
// Pre-fix HandleLoopBelowThreshold used FindLoaderForPayload, which
// walks only proc.ActiveStyleID, so the signal silently dropped at
// Edge despite Core having emitted it.
//
// Fixture mirrors TestFindAnyLoaderClaimForPayload_InactiveStyle:
// loader claim for WIDGET-X lives on style NEW, but style OLD is
// active. The pre-fix code would log "no loader for payload=WIDGET-X
// — dropping signal"; the post-fix code logs "loop_threshold: signal
// received loader=CAL-LOADER" and proceeds into the UOP-space math.
//
// We capture log output to assert the right path was taken without
// having to wire up Kafka/dispatch — the "firing N L1" log line is
// enough to prove HandleLoopBelowThreshold didn't short-circuit at
// the loader lookup.
func TestHandleLoopBelowThreshold_FiresForInactiveStyleLoader(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	processID, err := db.CreateProcess("OBS9-PROC", "obs9 test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "CAL-LOADER",
		Code:         "CL1",
		Name:         "Cal Loader",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	_ = nodeID

	oldStyleID, err := db.CreateStyle("OLD", "old style", processID)
	if err != nil {
		t.Fatalf("create old style: %v", err)
	}
	newStyleID, err := db.CreateStyle("NEW", "new style", processID)
	if err != nil {
		t.Fatalf("create new style: %v", err)
	}
	// OLD is active. The loader claim for WIDGET-X is on NEW (inactive).
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &oldStyleID), "set active")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             newStyleID,
		CoreNodeName:        "CAL-LOADER",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeManualSwap,
		PayloadCode:         "WIDGET-X",
		AllowedPayloadCodes: []string{"WIDGET-X"},
		UOPCapacity:         200,
		ReorderPoint:        2,
		InboundSource:       "EMPTY-SUPER",
		OutboundDestination: "FILLED-STORAGE",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	// payload_catalog is the per-bin capacity source for the UOP-threshold
	// path — claim.UOPCapacity is per-claim (zero on supermarket loaders);
	// per-payload capacity lives in the catalog.
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "Widget X", Code: "WIDGET-X", UOPCapacity: 200,
	}), "seed catalog")

	var (
		mu   sync.Mutex
		logs []string
	)
	logFn := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, sprintf(format, args...))
	}
	debugFn := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, sprintf(format, args...))
	}

	eng := &Engine{
		db:             db,
		logFn:          logFn,
		debugFn:        debugFn,
		Events:         NewEventBus(),
		orderMgr:       orders.NewManager(db, noOpOrderEmitter{}, "test-station"),
		catalogService: service.NewCatalogService(db),
	}
	// Warm the loader cache so the threshold signal resolves (else it parks as "not synced").
	seedCoreLoader(t, eng, sharedLoaderInfo("CAL-LOADER", "produce", "threshold", "WIDGET-X", 2, 100))

	eng.HandleLoopBelowThreshold(&protocol.LoopBelowThresholdSignal{
		PayloadCode:  "WIDGET-X",
		CurrentUOP:   0,
		Threshold:    100,
		CoreNodeName: "CAL-LOADER",
		Reason:       "below_threshold",
	})

	mu.Lock()
	defer mu.Unlock()
	hasReceived := false
	hasFired := false
	hasDropped := false
	for _, line := range logs {
		if strings.Contains(line, "loop_threshold: signal received loader=loader:CAL-LOADER") {
			hasReceived = true
		}
		if strings.Contains(line, "loop_threshold: loader=loader:CAL-LOADER payload=WIDGET-X firing 1 L1") {
			hasFired = true
		}
		if strings.Contains(line, "no loader for payload=WIDGET-X") {
			hasDropped = true
		}
	}
	if !hasReceived {
		t.Errorf("expected 'loop_threshold: signal received loader=CAL-LOADER' log line; got: %v", logs)
	}
	if !hasFired {
		t.Errorf("expected 'firing 1 L1' log line (threshold 100 / capacity 200 → ceil=1); got: %v", logs)
	}
	if hasDropped {
		t.Errorf("threshold signal must NOT be dropped on the no-loader check for an inactive-style loader; got drop log; full logs: %v", logs)
	}
}

// TestHandleLoopBelowThreshold_CeilsToWholeBins covers the UOP-space
// math: when threshold > capacity, one bin is insufficient and the
// handler computes ceil(gap / capacity) desired bins. The SNF2 plant
// incident on 2026-05-21 was the opposite case (threshold 100 /
// capacity 345 → ceil=1) where the legacy bin-count refill over-fired
// to minStock=2 — this test pins the math so future refactors can't
// quietly reintroduce a unit mismatch.
//
// Post-PR-0 the per-node capacity cap bounds the ACTUAL fire to the
// window's physical slot count (1 here): the math still computes 3
// (asserted via the desired_bins log) but tryCreateL1 dispatches one,
// and the remaining bins follow on subsequent signals as the window
// frees. See bin-loader-refactor-reviews/impl-questions.md Q1.
func TestHandleLoopBelowThreshold_CeilsToWholeBins(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	processID, err := db.CreateProcess("CEIL-PROC", "ceil test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "MULTIBIN-LOADER",
		Code:         "ML1",
		Name:         "Multibin Loader",
		Sequence:     1,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle("ONLY", "only style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        "MULTIBIN-LOADER",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeManualSwap,
		PayloadCode:         "TINY-PART",
		AllowedPayloadCodes: []string{"TINY-PART"},
		UOPCapacity:         0, // supermarket loader — capacity is in the catalog, not the claim
		InboundSource:       "EMPTY-SUPER",
		OutboundDestination: "FILLED-STORAGE",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	// threshold 900 UOP / capacity 300 UOP per bin → ceil(900/300) = 3 bins.
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "Tiny Part", Code: "TINY-PART", UOPCapacity: 300,
	}), "seed catalog")

	var (
		mu   sync.Mutex
		logs []string
	)
	capture := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, sprintf(format, args...))
	}
	eng := &Engine{
		db:             db,
		logFn:          capture,
		debugFn:        capture,
		Events:         NewEventBus(),
		orderMgr:       orders.NewManager(db, noOpOrderEmitter{}, "test-station"),
		catalogService: service.NewCatalogService(db),
	}
	// Warm the loader cache so the threshold signal resolves (else it parks as "not synced").
	seedCoreLoader(t, eng, sharedLoaderInfo("MULTIBIN-LOADER", "produce", "threshold", "TINY-PART", 0, 100))

	eng.HandleLoopBelowThreshold(&protocol.LoopBelowThresholdSignal{
		PayloadCode:  "TINY-PART",
		CurrentUOP:   0,
		Threshold:    900,
		CoreNodeName: "MULTIBIN-LOADER",
		Reason:       "below_threshold",
	})

	mu.Lock()
	defer mu.Unlock()
	mathComputed3 := false // ceil(900/300) = 3 desired bins (UOP-space math)
	cappedToOne := false   // per-node window cap (1 physical slot) bounds the fire
	for _, line := range logs {
		if strings.Contains(line, "desired_bins=3") {
			mathComputed3 = true
		}
		if strings.Contains(line, "loop_threshold: loader=loader:MULTIBIN-LOADER payload=TINY-PART firing 1 L1") {
			cappedToOne = true
		}
	}
	if !mathComputed3 {
		t.Errorf("expected ceil-to-whole-bins math desired_bins=3 (900/300); got: %v", logs)
	}
	if !cappedToOne {
		t.Errorf("expected the fire capped to 1 L1 by the per-node window cap; got: %v", logs)
	}
}

// TestHandleLoopBelowThreshold_SkipsWhenProjectedUOPCoversThreshold
// pins the inFlight-projects-UOP semantics: each in-flight L1 will
// contribute one bin's capacity once filled and returned, so when
// currentUOP + inFlight*capacity is already at or above threshold, no
// additional L1 is needed. Equivalent to the dedup gate in the
// legacy bin-count path but expressed in UOP space.
func TestHandleLoopBelowThreshold_SkipsWhenProjectedUOPCoversThreshold(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	processID, err := db.CreateProcess("SKIP-PROC", "skip test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "SKIP-LOADER",
		Code:         "SL1",
		Name:         "Skip Loader",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle("ONLY", "only style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        "SKIP-LOADER",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeManualSwap,
		PayloadCode:         "BIG-PART",
		AllowedPayloadCodes: []string{"BIG-PART"},
		InboundSource:       "EMPTY-SUPER",
		OutboundDestination: "FILLED-STORAGE",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "Big Part", Code: "BIG-PART", UOPCapacity: 500,
	}), "seed catalog")

	// Seed one in-flight retrieve_empty at the loader for BIG-PART. With
	// capacity 500 and threshold 400, one in-flight bin already covers the
	// threshold (desiredBins = ceil(400/500) = 1, inFlight = 1), so
	// tryCreateL1's in-flight dedup must fire nothing.
	om := orders.NewManager(db, noOpOrderEmitter{}, "test-station")
	if _, err := om.CreateRetrieveOrder(
		&nodeID, true, 1, "SKIP-LOADER", "EMPTY-SUPER", "",
		"standard", "BIG-PART", false, true,
	); err != nil {
		t.Fatalf("seed in-flight L1: %v", err)
	}

	var (
		mu   sync.Mutex
		logs []string
	)
	capture := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, sprintf(format, args...))
	}
	eng := &Engine{
		db:             db,
		logFn:          capture,
		debugFn:        capture,
		Events:         NewEventBus(),
		orderMgr:       om,
		catalogService: service.NewCatalogService(db),
	}
	// Warm the loader cache so the threshold signal resolves (else it parks as "not synced").
	seedCoreLoader(t, eng, sharedLoaderInfo("SKIP-LOADER", "produce", "threshold", "BIG-PART", 0, 100))

	eng.HandleLoopBelowThreshold(&protocol.LoopBelowThresholdSignal{
		PayloadCode:  "BIG-PART",
		CurrentUOP:   0,
		Threshold:    400,
		CoreNodeName: "SKIP-LOADER",
		Reason:       "below_threshold",
	})

	mu.Lock()
	defer mu.Unlock()
	skipped := false
	fired := false
	for _, line := range logs {
		// The reservation seam emits one structured decision record; a skip is
		// to_fire=0 — it counted the in-flight empty that already covers the
		// threshold and fired nothing (replaces the old per-payload "skipping"
		// debug line).
		if strings.Contains(line, "loader_reserve") && strings.Contains(line, "to_fire=0") {
			skipped = true
		}
		if strings.Contains(line, "firing") && strings.Contains(line, "L1") {
			fired = true
		}
	}
	if !skipped {
		t.Errorf("expected reservation-seam skip record (to_fire=0; 1 in-flight covers threshold); got: %v", logs)
	}
	if fired {
		t.Errorf("must NOT fire an L1 when in-flight already covers threshold; got: %v", logs)
	}
}

// TestHandleLoopBelowThreshold_SkipsOnMissingCatalogCapacity pins the
// fail-loud behavior when the per-payload UOP capacity isn't in
// payload_catalog. Without capacity the UOP-space math is undefined;
// silently falling back to the legacy bin-count floor would
// reintroduce the over-fire this path exists to avoid.
func TestHandleLoopBelowThreshold_SkipsOnMissingCatalogCapacity(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	processID, err := db.CreateProcess("MISS-PROC", "missing-capacity test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "MISS-LOADER",
		Code:         "MS1",
		Name:         "Miss Loader",
		Sequence:     1,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err := db.CreateStyle("ONLY", "only style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &styleID), "set active")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        "MISS-LOADER",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeManualSwap,
		PayloadCode:         "ORPHAN-PART",
		AllowedPayloadCodes: []string{"ORPHAN-PART"},
		InboundSource:       "EMPTY-SUPER",
		OutboundDestination: "FILLED-STORAGE",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	// Intentionally no UpsertPayloadCatalog for ORPHAN-PART.

	var (
		mu   sync.Mutex
		logs []string
	)
	capture := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, sprintf(format, args...))
	}
	eng := &Engine{
		db:             db,
		logFn:          capture,
		debugFn:        capture,
		Events:         NewEventBus(),
		orderMgr:       orders.NewManager(db, noOpOrderEmitter{}, "test-station"),
		catalogService: service.NewCatalogService(db),
	}
	// Warm the loader cache so the signal resolves; the catalog miss (not a loader
	// miss) is what this test exercises.
	seedCoreLoader(t, eng, sharedLoaderInfo("MISS-LOADER", "produce", "threshold", "ORPHAN-PART", 0, 100))

	eng.HandleLoopBelowThreshold(&protocol.LoopBelowThresholdSignal{
		PayloadCode:  "ORPHAN-PART",
		CurrentUOP:   0,
		Threshold:    100,
		CoreNodeName: "MISS-LOADER",
		Reason:       "below_threshold",
	})

	mu.Lock()
	defer mu.Unlock()
	skipped := false
	fired := false
	for _, line := range logs {
		if strings.Contains(line, "no per-bin capacity in catalog") {
			skipped = true
		}
		if strings.Contains(line, "firing") && strings.Contains(line, "L1") {
			fired = true
		}
	}
	if !skipped {
		t.Errorf("expected 'no per-bin capacity in catalog' skip log; got: %v", logs)
	}
	if fired {
		t.Errorf("must NOT fire an L1 without a catalog capacity; got: %v", logs)
	}
}

// TestHandleLoopBelowThreshold_ParksAndReplaysBeforeCacheSync pins the startup-race
// fix: a threshold signal that arrives before the loader cache's first sync (the
// signal beat the node-list response) is PARKED, not dropped, and replayed the instant
// SetCoreLoaders warms the cache — so the reorder isn't lost until the next delta.
func TestHandleLoopBelowThreshold_ParksAndReplaysBeforeCacheSync(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.loaderStore = newLoaderStore(eng) // aggregate store, COLD cache (no sync yet)
	eng.catalogService = service.NewCatalogService(db)

	// The loader's core node needs an Edge process_node (the L1 delivery target) and
	// the payload needs a catalog capacity for the UOP-space math.
	procID, err := db.CreateProcess("RACE-PROC", "", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "RACE-LOADER", Code: "R1", Name: "R1", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "Race Part", Code: "RACE-PART", UOPCapacity: 100,
	}), "seed catalog")

	countL1 := func() int {
		ords, err := db.ListActiveOrdersByProcessNode(nodeID)
		if err != nil {
			t.Fatalf("list orders: %v", err)
		}
		n := 0
		for _, o := range ords {
			if o.RetrieveEmpty && o.PayloadCode == "RACE-PART" {
				n++
			}
		}
		return n
	}

	sig := &protocol.LoopBelowThresholdSignal{
		PayloadCode: "RACE-PART", CurrentUOP: 0, Threshold: 100, CoreNodeName: "RACE-LOADER", Reason: "below_threshold",
	}

	// Cold cache: the loader can't resolve yet, so the signal must be PARKED (not
	// dropped) and fire nothing.
	eng.HandleLoopBelowThreshold(sig)
	if got := countL1(); got != 0 {
		t.Fatalf("cold cache: expected 0 L1 (signal parked), got %d", got)
	}
	if n := len(eng.pendingThreshold); n != 1 {
		t.Fatalf("expected 1 parked signal, got %d", n)
	}
	if eng.loaderCacheWarmed.Load() {
		t.Fatal("cache must not be warmed before any SetCoreLoaders")
	}

	// The node-list sync lands, carrying the loader. SetCoreLoaders warms the cache
	// and replays the parked signal, which now resolves and fires its L1.
	eng.SetCoreLoaders([]protocol.LoaderInfo{{
		Name: "RL", LoaderKey: "loader:RACE-LOADER", Role: "produce", Layout: "shared_window", Replenishment: "threshold",
		InboundSource: "EMPTY-SUPER", OutboundDest: "FILLED", ConfigGen: 1,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "RACE-LOADER", Kind: "window"}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "RACE-PART", UOPThreshold: 100}},
	}})

	if !eng.loaderCacheWarmed.Load() {
		t.Error("cache should be marked warmed after SetCoreLoaders")
	}
	if n := len(eng.pendingThreshold); n != 0 {
		t.Errorf("pending buffer should be drained after replay, got %d", n)
	}
	if got := countL1(); got != 1 {
		t.Errorf("after cache sync: expected the parked signal to replay and fire 1 L1, got %d", got)
	}
}

// sprintf is a thin alias so the log-capture closures read cleanly.
func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// TestHandleLoopBelowThreshold_RoutesToSignaledCoreNode pins the routing fix:
// when the SAME payload is loaded at two loaders, the L1 must land at the
// loader the signal names (sig.CoreNodeName), not the alphabetically-first
// one a payload-only resolution would pick. Before the fix the handler used
// FindAnyLoaderClaimForPayload(payload) and would have fired at LOADER-A.
func TestHandleLoopBelowThreshold_RoutesToSignaledCoreNode(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.catalogService = service.NewCatalogService(db)

	_, nodeA, _ := seedActiveManualSwapLoader(t, db, "AAA-PROC", "LOADER-A", "SHARED")
	_, nodeB, _ := seedActiveManualSwapLoader(t, db, "BBB-PROC", "LOADER-B", "SHARED")
	testutil.MustNoErr(t, db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "Shared", Code: "SHARED", UOPCapacity: 100,
	}), "seed catalog")
	// Both loaders serve SHARED; the signal names LOADER-B, so the L1 must land there.
	seedCoreLoader(t, eng,
		sharedLoaderInfo("LOADER-A", "produce", "threshold", "SHARED", 0, 100),
		sharedLoaderInfo("LOADER-B", "produce", "threshold", "SHARED", 0, 100))

	eng.HandleLoopBelowThreshold(&protocol.LoopBelowThresholdSignal{
		PayloadCode:  "SHARED",
		CurrentUOP:   0,
		Threshold:    100,
		CoreNodeName: "LOADER-B",
		Reason:       "below_threshold",
	})

	countL1 := func(nodeID int64) int {
		ords, err := db.ListActiveOrdersByProcessNode(nodeID)
		if err != nil {
			t.Fatalf("list orders: %v", err)
		}
		n := 0
		for _, o := range ords {
			if o.RetrieveEmpty && o.PayloadCode == "SHARED" {
				n++
			}
		}
		return n
	}
	if got := countL1(nodeB); got != 1 {
		t.Errorf("expected 1 L1 at the signaled loader LOADER-B, got %d", got)
	}
	if got := countL1(nodeA); got != 0 {
		t.Errorf("expected 0 L1 at the non-signaled loader LOADER-A, got %d", got)
	}
}
