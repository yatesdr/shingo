// Package scenarios contains end-to-end integration tests that exercise
// the Edge↔Core wire round-trip through the harness Bus. Each scenario
// runs both processes against real test databases (SQLite for Edge,
// Postgres-via-testcontainer for Core) and asserts on bin records,
// order rows, and runtime state — the things that survive a process
// restart and that operators actually see.
//
// The build tag `docker` matches Core's existing pattern: these tests
// require Docker for the Postgres container. Skipped cleanly otherwise.
//
//go:build docker

package scenarios

import (
	"testing"

	"shingo/protocol"
	"shingo/integration/harness"

	coreharness "shingocore/testharness"
	"shingocore/dispatch"
	coremessaging "shingocore/messaging"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	coreorders "shingocore/store/orders"

	edgeharness "shingoedge/testharness"
)

// TestScenario_PartialReleaseLandsBinWithCorrectManifest is the
// closed-loop bin-centric integration test for fixes #11 and #15.
//
// Round 1 of the partial-return loop:
//
//  1. Setup: bin at line claimed by a staged complex order. Bin's
//     uop_remaining starts at full capacity (a fresh bin from the
//     prior cycle's swap). Core has the authoritative bin record.
//
//  2. Edge enqueues an OrderRelease envelope with RemainingUOP=300
//     (the SEND PARTIAL BACK disposition, simulating the operator's
//     declaration that 300 of capacity remain in the bin).
//
//  3. Bus pumps Edge → Core. The envelope round-trips through real
//     JSON marshaling and Core's protocol.Ingestor → CoreHandler →
//     Dispatcher.HandleOrderRelease → BinManifestService.SyncOrClearForReleased.
//
//  4. Assert: bin record at Core reflects the partial state:
//     - uop_remaining = 300 (#15 sync)
//     - manifest reconstructed as {"items":[{"catid":..,"qty":300}]} (#15)
//     - payload_code preserved
//     - claimed_by preserved (release does not unclaim — that comes at
//       arrival via ApplyArrival)
//
// What this catches that unit tests miss:
//   - JSON marshal/unmarshal of OrderRelease envelope (a type change
//     in protocol.OrderRelease that breaks decoding would surface here)
//   - The full ingestor → handler → service → DB chain (a misrouted
//     envelope type would never reach SyncOrClearForReleased)
//   - The atomic JSONB rebuild SQL running against real Postgres (a
//     pgx-version subtlety in jsonb_build_object would surface here)
//
// Round 2 of the loop (Core → Edge: bin returns as a partial supply
// for the next cycle, BinUOPRemaining snapshot survives the wire) is
// covered in shingo-edge/engine/uop_regression_test.go's
// TestRegression_11_DeliveryOfPartialBinResetsToBinUOP, which validates
// the Edge-side handling of the snapshot. Wiring that into the harness
// scenario is the next layer of integration testing — see SHINGO_TODO.md
// for the open work.
func TestScenario_PartialReleaseLandsBinWithCorrectManifest(t *testing.T) {
	// ── Core setup ─────────────────────────────────────────────────
	coreDB := coreharness.OpenDB(t)
	sd := coreharness.SetupStandardData(t, coreDB)

	// Bin at the line, full from a previous cycle. Confirmed manifest
	// is the pre-condition for any release path — an unconfirmed bin
	// wouldn't be eligible.
	bin := &corebins.Bin{
		BinTypeID: sd.BinType.ID,
		Label:     "BIN-SCN-PART",
		NodeID:    &sd.LineNode.ID,
		Status:    "staged",
	}
	if err := coreDB.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	const fullManifest = `{"items":[{"catid":"PART-A","qty":1000}]}`
	if err := coreDB.SetBinManifest(bin.ID, fullManifest, sd.Payload.Code, 1000); err != nil {
		t.Fatalf("set manifest: %v", err)
	}
	if err := coreDB.ConfirmBinManifest(bin.ID, ""); err != nil {
		t.Fatalf("confirm manifest: %v", err)
	}

	// Staged complex order claiming the bin. The release path requires
	// Status=Staged AND claimed_by=order.ID.
	order := &coreorders.Order{
		EdgeUUID:     "scenario-partial-1",
		StationID:    "edge.test",
		OrderType:    dispatch.OrderTypeComplex,
		Status:       dispatch.StatusStaged,
		Quantity:     1,
		SourceNode:   sd.LineNode.Name,
		DeliveryNode: "OUTBOUND-DEST",
		PayloadCode:  sd.Payload.Code,
		StepsJSON: `[{"action":"wait","node":"` + sd.LineNode.Name + `"},` +
			`{"action":"pickup","node":"` + sd.LineNode.Name + `"},` +
			`{"action":"dropoff","node":"OUTBOUND-DEST"}]`,
	}
	if err := coreDB.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// CreateOrder may set initial status; force staged + claimed_by so
	// the release path's WHERE guard matches.
	if err := coreDB.UpdateOrderStatus(order.ID, string(dispatch.StatusStaged), "scenario setup"); err != nil {
		t.Fatalf("force staged: %v", err)
	}
	if err := coreDB.ClaimBin(bin.ID, order.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}

	// Need the OUTBOUND-DEST node for step resolution downstream
	// (HandleOrderRelease reads steps to figure out post-wait segments).
	outNode := &corenodes.Node{Name: "OUTBOUND-DEST", Enabled: true}
	if err := coreDB.CreateNode(outNode); err != nil {
		t.Fatalf("create outbound node: %v", err)
	}

	// Build the Core dispatcher + handler stack. The trackingBackend
	// captures fleet calls so we can assert them if needed (not used
	// by this scenario but sets up the dispatcher correctly).
	backend := coreharness.NewTrackingBackend()
	emitter := &noopEmitter{}
	dispatcher := dispatch.NewDispatcher(coreDB, backend, emitter, "core", "shingo.dispatch", nil)
	coreHandler := coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", dispatcher)
	coreIngestor := protocol.NewIngestor(coreHandler, nil)

	// ── Edge setup ─────────────────────────────────────────────────
	// Edge side is minimal: we just need an outbox to enqueue from
	// and an ingestor that won't panic on inbound messages we don't
	// care about for this scenario.
	edgeDB := edgeharness.OpenDB(t)
	edgeIngestor := protocol.NewIngestor(&protocol.NoOpHandler{}, nil)

	// ── Wire the bus ───────────────────────────────────────────────
	bus := harness.NewBus(t,
		harness.EdgeSide{
			EdgeStore:    edgeDB,
			EdgeIngestor: edgeIngestor,
		},
		harness.CoreSide{
			CoreStore:    coreDB,
			CoreIngestor: coreIngestor,
		},
	)

	// ── Cycle 1: operator releases bin with PARTIAL 300 ────────────
	const partialUOP = 300
	v := partialUOP
	releasePayload := &protocol.OrderRelease{
		OrderUUID:    "scenario-partial-1",
		RemainingUOP: &v,
		CalledBy:     "operator-test",
	}
	env, err := protocol.NewEnvelope(
		protocol.TypeOrderRelease,
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.test"},
		protocol.Address{Role: protocol.RoleCore},
		releasePayload,
	)
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	encoded, err := env.Encode()
	if err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	if _, err := edgeDB.EnqueueOutbox(encoded, protocol.TypeOrderRelease); err != nil {
		t.Fatalf("enqueue release on Edge outbox: %v", err)
	}

	// ── Pump: Edge → Wire → Core ───────────────────────────────────
	delivered := bus.PumpEdgeOutbox()
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}

	// ── Assert bin record post-release ─────────────────────────────
	got, err := coreDB.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}

	// #15: uop_remaining synced to operator-declared value
	if got.UOPRemaining != partialUOP {
		t.Errorf("UOPRemaining = %d, want %d (#15: SyncOrClearForReleased should sync)",
			got.UOPRemaining, partialUOP)
	}

	// #15: payload_code preserved on partial release (only RELEASE
	// EMPTY clears it)
	if got.PayloadCode != sd.Payload.Code {
		t.Errorf("PayloadCode = %q, want %q (preserved on partial)",
			got.PayloadCode, sd.Payload.Code)
	}

	// #15: manifest rewritten to single-payload form. Pre-fix this
	// would still carry qty=1000 (the pre-release value); post-fix
	// it's reconstructed to qty=300.
	if got.Manifest == nil {
		t.Fatal("Manifest = nil; want reconstructed single-payload manifest")
	}
	parsed, err := got.ParseManifest()
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("manifest items = %d, want 1 (single-payload normalization)",
			len(parsed.Items))
	}
	item := parsed.Items[0]
	if item.CatID != sd.Payload.Code {
		t.Errorf("manifest CatID = %q, want %q (= payload_code per single-payload normalization)",
			item.CatID, sd.Payload.Code)
	}
	if item.Quantity != int64(partialUOP) {
		t.Errorf("manifest Quantity = %d, want %d (= operator-declared remaining)",
			item.Quantity, partialUOP)
	}

	// claimed_by must still point at this order (release does not
	// unclaim — that's ApplyArrival's job, which runs at the bin's
	// next physical arrival).
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d (preserved through release)",
			got.ClaimedBy, order.ID)
	}

	// Edge outbox should be drained.
	pending, _ := edgeDB.ListPendingOutbox(10)
	if len(pending) != 0 {
		t.Errorf("Edge outbox after pump = %d pending, want 0", len(pending))
	}
}

// noopEmitter satisfies the dispatch.Emitter interface for tests that
// don't care about emitted events. Mirrors the test pattern in Core's
// own dispatcher_test.go (mockEmitter) without re-exporting that test
// type across packages.
type noopEmitter struct{}

func (noopEmitter) EmitOrderReceived(_ int64, _, _ string, _ protocol.OrderType, _, _ string) {
}
func (noopEmitter) EmitOrderDispatched(_ int64, _, _, _ string)   {}
func (noopEmitter) EmitOrderFailed(_ int64, _, _, _, _ string)    {}
func (noopEmitter) EmitOrderCancelled(_ int64, _, _, _, _ string) {}
func (noopEmitter) EmitOrderCompleted(_ int64, _, _ string)       {}
func (noopEmitter) EmitOrderQueued(_ int64, _, _, _ string)       {}
