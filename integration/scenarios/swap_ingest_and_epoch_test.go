// End-to-end scenarios for two inventory behaviors that the per-module unit
// tests can only cover from one side:
//
//   - A swap-mode produce finalize stamps Core's bin manifest via a
//     fire-and-forget ingest and creates NO order row. The Edge unit test can
//     see "no local order, manifest envelope queued"; only this scenario, with
//     a real Core handler on the other end of the Bus, can assert Core applied
//     the stamp and minted nothing.
//
//   - A delta carrying a delta_epoch from before a bin was reset is dropped and
//     recorded as a discrepancy, while a current-epoch delta still applies.
//     This drives the real load → consume → release → late-delta lifecycle
//     against Postgres so the epoch advances through the production reset paths.
//
//go:build docker

package scenarios

import (
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	"shingocore/dispatch"
	coremessaging "shingocore/messaging"
	"shingocore/service"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	coreharness "shingocore/testharness"
	"shingocore/uop"

	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
)

// TestScenario_SwapFinalizeStampsCoreBinWithoutOrderRow drives a swap-mode
// produce finalize on Edge, pumps the resulting envelopes to a real Core
// handler, and asserts Core stamped the target bin's manifest and remaining
// count from the queued ingest while creating no order row. This closes the
// Core-side gap the Edge-only finalize test cannot reach: that the manifest-
// only ingest is a pure stamp with nothing for a later abort to cancel.
func TestScenario_SwapFinalizeStampsCoreBinWithoutOrderRow(t *testing.T) {
	const stationID = "edge.test"
	const produceNodeName = "PRODUCE-NODE"
	const producedCount = 50

	// ── Core: real handler stack + a bin parked at the produce node ──
	coreDB := coreharness.OpenDB(t)
	sd := coreharness.SetupStandardData(t, coreDB) // provides payload PART-A + a BinType

	produceNode := &corenodes.Node{Name: produceNodeName, Enabled: true}
	if err := coreDB.CreateNode(produceNode); err != nil {
		t.Fatalf("create core produce node: %v", err)
	}
	// A bare bin sits at the produce node — the freshly-filled cart whose
	// manifest the ingest will stamp. No payload yet (Core learns it here).
	coreBin := &corebins.Bin{
		BinTypeID: sd.BinType.ID,
		Label:     "CORE-PRODUCE-BIN",
		NodeID:    &produceNode.ID,
		Status:    "available",
	}
	if err := coreDB.CreateBin(coreBin); err != nil {
		t.Fatalf("create core bin: %v", err)
	}

	backend := coreharness.NewTrackingBackend()
	dispatcher := dispatch.NewDispatcher(coreDB, backend, &noopEmitter{}, "core", "shingo.dispatch", nil)
	coreHandler := coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", dispatcher)
	coreIngestor := protocol.NewIngestor(nil)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeOrderIngest, coreHandler.HandleOrderIngest)
	coreIngestor.Dispatch = func(env *protocol.Envelope) {
		coreRouter.Dispatch(env, env.Type)
	}

	// ── Edge: full engine + a swap-mode produce node matching Core's node ──
	edge := edgeharness.NewEdge(t, stationID)
	processID, err := edge.DB.CreateProcess("SCN-PROC", "swap ingest scenario", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: produceNodeName,
		Code:         "SCN1",
		Name:         "Scenario Produce Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create process node: %v", err)
	}
	styleID, err := edge.DB.CreateStyle("SCN-STYLE", "scenario style", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := edge.DB.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	claimID, err := edge.DB.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        produceNodeName,
		Role:                "produce",
		SwapMode:            "sequential", // any swap mode → manifest-only ingest path
		PayloadCode:         sd.Payload.Code,
		UOPCapacity:         100,
		InboundSource:       "EMPTY-STORAGE",
		InboundStaging:      "PRODUCE-IN-STAGING",
		OutboundStaging:     "PRODUCE-OUT-STAGING",
		OutboundDestination: "FILLED-STORAGE",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if _, err := edge.DB.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	if err := edge.DB.SetProcessNodeRuntime(nodeID, &claimID, producedCount); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	bus := harness.NewBus(t,
		harness.EdgeSide{EdgeStore: edge.DB, EdgeIngestor: edge.Ingestor},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIngestor},
	)

	// Drain any startup envelopes so we pump only what finalize emits.
	drainOutbox(t, edge)

	// ── Drive the swap-mode produce finalize ──
	if _, err := edge.Engine.FinalizeProduceNode(nodeID); err != nil {
		t.Fatalf("FinalizeProduceNode: %v", err)
	}

	// Edge minted no local ingest order (the phantom is gone).
	edgeOrders, err := edge.DB.ListOrders()
	if err != nil {
		t.Fatalf("list edge orders: %v", err)
	}
	for _, o := range edgeOrders {
		if o.OrderType == "ingest" {
			t.Errorf("edge created a local ingest order #%d (phantom should be gone)", o.ID)
		}
	}

	// ── Pump Edge → Core. The complex-order envelope is unrouted on Core
	// (logged and dropped); only the manifest-only ingest is handled. ──
	bus.PumpEdgeOutbox()

	// Core stamped the bin from the queued manifest.
	got, err := coreDB.GetBin(coreBin.ID)
	if err != nil {
		t.Fatalf("get core bin: %v", err)
	}
	if got.PayloadCode != sd.Payload.Code {
		t.Errorf("core bin PayloadCode = %q, want %q (ingest should stamp it)", got.PayloadCode, sd.Payload.Code)
	}
	if got.UOPRemaining != producedCount {
		t.Errorf("core bin UOPRemaining = %d, want %d (the finalized count)", got.UOPRemaining, producedCount)
	}
	if got.Manifest == nil {
		t.Error("core bin Manifest = nil, want the stamped manifest")
	}

	// Core created NO order row — the manifest-only ingest is a pure stamp,
	// so a later abort has nothing to cancel (the source of the old
	// "order not found").
	var orderCount int
	if err := coreDB.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&orderCount); err != nil {
		t.Fatalf("count core orders: %v", err)
	}
	if orderCount != 0 {
		t.Errorf("core order rows = %d, want 0 (manifest-only ingest must not create an order)", orderCount)
	}
}

// TestScenario_StaleEpochDeltaDroppedAndRecordedAfterRelease exercises the real
// load → consume → release → late-delta lifecycle against Postgres. After a
// release advances the bin's delta_epoch, a consume delta still carrying the
// pre-release epoch must be dropped (count unchanged) and recorded as a single
// discrepancy row, while a delta on the current epoch still applies.
func TestScenario_StaleEpochDeltaDroppedAndRecordedAfterRelease(t *testing.T) {
	coreDB := coreharness.OpenDB(t)
	sd := coreharness.SetupStandardData(t, coreDB)

	manifest := service.NewBinManifestService(coreDB)
	inv := uop.NewInventoryDeltaService(coreDB, manifest)

	bin := &corebins.Bin{
		BinTypeID: sd.BinType.ID,
		Label:     "SCN-EPOCH-BIN",
		NodeID:    &sd.LineNode.ID,
		Status:    "available",
	}
	if err := coreDB.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}

	consume := func(epoch int64, delta int, seq int64) error {
		now := time.Now().UTC()
		return inv.ApplyBinUOPDelta(&protocol.BinUOPDelta{
			Station:     "ALN_001",
			BinID:       bin.ID,
			PayloadCode: sd.Payload.Code,
			Delta:       delta,
			Reason:      protocol.ReasonConsumeTick,
			SequenceID:  seq,
			Epoch:       epoch,
			WindowStart: now.Add(-5 * time.Second),
			WindowEnd:   now,
		})
	}
	uopOf := func() int {
		t.Helper()
		var v int
		if err := coreDB.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, bin.ID).Scan(&v); err != nil {
			t.Fatalf("read uop: %v", err)
		}
		return v
	}

	// ── Load: stamp the manifest. delta_epoch advances to the load epoch. ──
	loadEpoch, err := manifest.SetForProduction(bin.ID, `{"items":[{"catid":"`+sd.Payload.Code+`","qty":100}]}`, sd.Payload.Code, 100)
	if err != nil {
		t.Fatalf("load (SetForProduction): %v", err)
	}

	// ── Consume on the load epoch: applies. ──
	if err := consume(loadEpoch, -10, 1); err != nil {
		t.Fatalf("current-epoch consume should apply: %v", err)
	}
	if got := uopOf(); got != 90 {
		t.Fatalf("after consume uop = %d, want 90", got)
	}

	// ── Release empty: count resets and delta_epoch advances again. ──
	const releaseOrderID int64 = 778899
	if err := coreDB.ClaimBin(bin.ID, releaseOrderID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}
	zero := 0
	if err := manifest.SyncOrClearForReleased(bin.ID, releaseOrderID, &zero, protocol.DispositionReleaseEmpty, "operator"); err != nil {
		t.Fatalf("release empty: %v", err)
	}
	if got := uopOf(); got != 0 {
		t.Fatalf("after release uop = %d, want 0", got)
	}

	// ── Late delta carrying the pre-release (load) epoch: dropped. ──
	if err := consume(loadEpoch, -5, 2); err != uop.ErrInventoryDeltaSkipped {
		t.Fatalf("stale-epoch consume err = %v, want ErrInventoryDeltaSkipped", err)
	}
	if got := uopOf(); got != 0 {
		t.Errorf("after stale delta uop = %d, want 0 unchanged", got)
	}
	var dropped int
	if err := coreDB.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit WHERE bin_id=$1 AND op='stale_epoch_dropped'`, bin.ID).Scan(&dropped); err != nil {
		t.Fatalf("count discrepancy rows: %v", err)
	}
	if dropped != 1 {
		t.Errorf("stale_epoch_dropped rows = %d, want exactly 1", dropped)
	}

	// ── Reload and consume on the new epoch: applies again. ──
	reloadEpoch, err := manifest.SetForProduction(bin.ID, `{"items":[{"catid":"`+sd.Payload.Code+`","qty":100}]}`, sd.Payload.Code, 100)
	if err != nil {
		t.Fatalf("reload (SetForProduction): %v", err)
	}
	if reloadEpoch <= loadEpoch {
		t.Fatalf("reload epoch = %d, want greater than load epoch %d", reloadEpoch, loadEpoch)
	}
	if err := consume(reloadEpoch, -3, 3); err != nil {
		t.Fatalf("post-reset current-epoch consume should apply: %v", err)
	}
	if got := uopOf(); got != 97 {
		t.Errorf("after reload+consume uop = %d, want 97", got)
	}
}
