//go:build docker

package dispatch

import (
	"encoding/json"
	"sync"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestFullDepletion_ClearsManifest verifies that when a move order carries
// remaining_uop=0 (fully depleted bin), the manifest is atomically cleared
// and the bin is claimed in a single operation.
func TestFullDepletion_ClearsManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Create a line node (process node) and a filled bin there
	processNode := &store.Node{Name: "LINE1-CONSUME", Enabled: true}
	db.CreateNode(processNode)

	bin := &store.Bin{BinTypeID: 1, Label: "BIN-DEP-1", NodeID: &processNode.ID, Status: "staged"}
	db.CreateBin(bin)
	db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART-A","qty":100}]}`, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	// Verify bin starts with manifest
	before, _ := db.GetBin(bin.ID)
	if before.PayloadCode == "" {
		t.Fatal("bin should have payload_code before depletion")
	}

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// Build an envelope with remaining_uop=0 (fully depleted)
	zero := 0
	orderReq := &protocol.OrderRequest{
		OrderUUID:    "uuid-dep-1",
		OrderType:    "move",
		Quantity:     1,
		SourceNode:   processNode.Name,
		DeliveryNode: lineNode.Name,
		PayloadCode:  bp.Code,
		RemainingUOP: &zero,
	}
	body, _ := json.Marshal(orderReq)
	env := testEnvelope()
	env.Payload, _ = json.Marshal(protocol.Data{Body: body})

	d.HandleOrderRequest(env, orderReq)

	// Verify bin's manifest is cleared AND bin is claimed
	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (cleared after full depletion)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed should be false after clear")
	}
	if got.ClaimedBy == nil {
		t.Error("bin should be claimed after dispatch")
	}
}

// TestPartialConsumption_SyncsUOP verifies that when a move order carries
// remaining_uop>0 (partially consumed bin), the UOP is synced to the bin
// record while preserving the manifest and payload_code.
func TestPartialConsumption_SyncsUOP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	processNode := &store.Node{Name: "LINE1-PARTIAL", Enabled: true}
	db.CreateNode(processNode)

	bin := &store.Bin{BinTypeID: 1, Label: "BIN-PRT-1", NodeID: &processNode.ID, Status: "staged"}
	db.CreateBin(bin)
	manifest := `{"items":[{"catid":"PART-A","qty":100}]}`
	db.SetBinManifest(bin.ID, manifest, bp.Code, 100)
	db.ConfirmBinManifest(bin.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	// remaining_uop=42 — partial consumption
	partial := 42
	orderReq := &protocol.OrderRequest{
		OrderUUID:    "uuid-prt-1",
		OrderType:    "move",
		Quantity:     1,
		SourceNode:   processNode.Name,
		DeliveryNode: lineNode.Name,
		PayloadCode:  bp.Code,
		RemainingUOP: &partial,
	}
	body, _ := json.Marshal(orderReq)
	env := testEnvelope()
	env.Payload, _ = json.Marshal(protocol.Data{Body: body})

	d.HandleOrderRequest(env, orderReq)

	got, _ := db.GetBin(bin.ID)

	// UOP should be synced
	if got.UOPRemaining != 42 {
		t.Errorf("UOPRemaining = %d, want 42", got.UOPRemaining)
	}

	// Manifest should be preserved
	if got.PayloadCode != bp.Code {
		t.Errorf("PayloadCode = %q, want %q (preserved)", got.PayloadCode, bp.Code)
	}
	if got.Manifest == nil {
		t.Error("Manifest should be preserved after partial consumption")
	} else {
		// Postgres JSONB normalizes whitespace/key order, so compare decoded values
		var gotJSON, wantJSON interface{}
		json.Unmarshal([]byte(*got.Manifest), &gotJSON)
		json.Unmarshal([]byte(manifest), &wantJSON)
		gotBytes, _ := json.Marshal(gotJSON)
		wantBytes, _ := json.Marshal(wantJSON)
		if string(gotBytes) != string(wantBytes) {
			t.Errorf("Manifest = %s, want %s", *got.Manifest, manifest)
		}
	}

	// Should be claimed
	if got.ClaimedBy == nil {
		t.Error("bin should be claimed after dispatch")
	}
}

// TestConcurrentRetrieveEmpty_BothClaimed_NoOverlap verifies that when two
// retrieve_empty orders race for two available bins, each order claims a
// different bin with no double-claims. This tests concurrent claim distribution
// rather than the ghost-bin TOCTOU (which ClearAndClaim's atomic SQL prevents).
func TestConcurrentRetrieveEmpty_BothClaimed_NoOverlap(t *testing.T) {
	db := testDB(t)
	_, _, _ = setupTestData(t, db)

	// Create a payload with bin type compatibility
	bp := &store.Payload{Code: "RACE-BP"}
	db.CreatePayload(bp)
	bt, _ := db.GetBinTypeByCode("DEFAULT")
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	storageNode := &store.Node{Name: "RACE-STORAGE", Zone: "A", Enabled: true}
	db.CreateNode(storageNode)

	// Create two empty bins
	bin1 := &store.Bin{BinTypeID: bt.ID, Label: "RACE-BIN-1", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin1)
	bin2 := &store.Bin{BinTypeID: bt.ID, Label: "RACE-BIN-2", NodeID: &storageNode.ID, Status: "available"}
	db.CreateBin(bin2)

	// Create two orders that will race to claim
	order1 := &store.Order{EdgeUUID: "race-1", StationID: "test", OrderType: "retrieve", Status: "pending", Quantity: 1, DeliveryNode: "LINE1-IN", PayloadDesc: "retrieve_empty"}
	db.CreateOrder(order1)
	order2 := &store.Order{EdgeUUID: "race-2", StationID: "test", OrderType: "retrieve", Status: "pending", Quantity: 1, DeliveryNode: "LINE1-IN", PayloadDesc: "retrieve_empty"}
	db.CreateOrder(order2)

	// Race: two goroutines try to find and claim empty bins concurrently
	var wg sync.WaitGroup
	results := make([]int64, 2)
	errors := make([]error, 2)

	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			found, err := db.FindEmptyCompatibleBin(bp.Code, "")
			if err != nil {
				errors[idx] = err
				return
			}
			orderID := order1.ID
			if idx == 1 {
				orderID = order2.ID
			}
			if err := db.ClaimBin(found.ID, orderID); err != nil {
				errors[idx] = err
				return
			}
			results[idx] = found.ID
		}(i)
	}
	wg.Wait()

	// Count successful claims
	successCount := 0
	for i := 0; i < 2; i++ {
		if errors[i] == nil && results[i] > 0 {
			successCount++
		}
	}

	// Both should succeed (two empty bins available)
	if successCount != 2 {
		t.Errorf("expected 2 successful claims (2 empty bins), got %d", successCount)
	}

	// Verify each bin is claimed by exactly one order
	got1, _ := db.GetBin(bin1.ID)
	got2, _ := db.GetBin(bin2.ID)
	if got1.ClaimedBy == nil && got2.ClaimedBy == nil {
		t.Error("expected at least one bin to be claimed")
	}
	// They should have different claimants
	if got1.ClaimedBy != nil && got2.ClaimedBy != nil && *got1.ClaimedBy == *got2.ClaimedBy {
		t.Error("both bins claimed by same order — race condition")
	}
}

// TestComplexOrder_RemainingUOP_ProcessNodeOnly verifies that in a complex
// order with multiple pickups, only the pickup at the process node gets
// remainingUOP applied. Other pickups (storage, staging) get plain claims.
func TestComplexOrder_RemainingUOP_ProcessNodeOnly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)

	processNode := &store.Node{Name: "COMPLEX-LINE1", Enabled: true}
	db.CreateNode(processNode)
	stagingNode := &store.Node{Name: "COMPLEX-STAGING", Enabled: true}
	db.CreateNode(stagingNode)

	// Bin at process node (outgoing, depleted bin)
	binProcess := &store.Bin{BinTypeID: 1, Label: "BIN-CP-1", NodeID: &processNode.ID, Status: "staged"}
	db.CreateBin(binProcess)
	db.SetBinManifest(binProcess.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(binProcess.ID, "")

	// Bin at staging node (incoming, should NOT have manifest changed)
	binStaging := &store.Bin{BinTypeID: 1, Label: "BIN-CP-2", NodeID: &stagingNode.ID, Status: "staged"}
	db.CreateBin(binStaging)
	db.SetBinManifest(binStaging.ID, `{"items":[{"catid":"NEW","qty":200}]}`, bp.Code, 200)
	db.ConfirmBinManifest(binStaging.ID, "")

	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)

	zero := 0
	env := testEnvelope()
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   "uuid-complex-pn",
		PayloadCode: bp.Code,
		Quantity:     1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: processNode.Name},       // outgoing depleted bin
			{Action: "dropoff", Node: "COMPLEX-STAGING"},     // park old nearby
			{Action: "pickup", Node: stagingNode.Name},       // grab new from staging
			{Action: "dropoff", Node: processNode.Name},      // deliver new to line
		},
		RemainingUOP: &zero,
	})

	// Process node bin: should have manifest CLEARED (remainingUOP=0 applied)
	gotProcess, _ := db.GetBin(binProcess.ID)
	if gotProcess.PayloadCode != "" {
		t.Errorf("process bin PayloadCode = %q, want empty (cleared)", gotProcess.PayloadCode)
	}
	if gotProcess.ClaimedBy == nil {
		t.Error("process bin should be claimed")
	}

	// Staging node bin: should have manifest PRESERVED (plain claim, no remainingUOP)
	gotStaging, _ := db.GetBin(binStaging.ID)
	if gotStaging.PayloadCode != bp.Code {
		t.Errorf("staging bin PayloadCode = %q, want %q (preserved)", gotStaging.PayloadCode, bp.Code)
	}
	if gotStaging.UOPRemaining != 200 {
		t.Errorf("staging bin UOPRemaining = %d, want 200 (preserved)", gotStaging.UOPRemaining)
	}
	if gotStaging.ClaimedBy == nil {
		t.Error("staging bin should be claimed")
	}
}

// ──────────────────────────────────────────────────────────────────────────
// HandleOrderRelease + RemainingUOP integration tests.
//
// These tests stage a complex order with a wait step (so it lands in
// StatusStaged with the line bin claimed by claimComplexBins), then call
// HandleOrderRelease with various RemainingUOP values to assert the
// late-binding manifest sync runs correctly before the fleet release.
//
// Maps to BinManifestService.SyncOrClearForReleased's three branches plus
// the "wrong owner" failure surface (operator clicks release on an order
// whose bin has been reassigned to someone else).
// ──────────────────────────────────────────────────────────────────────────

// stageComplexOrderWithLineBin sets up a complex order whose first non-wait
// pickup is at the line node, dispatches it through HandleComplexOrderRequest
// (which claims the line bin), then forces the order into StatusStaged so
// HandleOrderRelease will accept it.
func stageComplexOrderWithLineBin(t *testing.T, db *store.DB, d *Dispatcher, lineNode *store.Node, bp *store.Payload, orderUUID, binLabel string) (*store.Order, *store.Bin) {
	t.Helper()

	// Destination node for the dropoff step (must exist for step resolution).
	destNode := &store.Node{Name: "RELEASE-DEST", Enabled: true}
	if err := db.CreateNode(destNode); err != nil {
		t.Fatalf("create dest node: %v", err)
	}

	// Filled bin at the line (outgoing partial/empty after consumption).
	bin := &store.Bin{BinTypeID: 1, Label: binLabel, NodeID: &lineNode.ID, Status: "staged"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin %s: %v", binLabel, err)
	}
	if err := db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART-A","qty":100}]}`, bp.Code, 100); err != nil {
		t.Fatalf("set manifest %s: %v", binLabel, err)
	}
	if err := db.ConfirmBinManifest(bin.ID, ""); err != nil {
		t.Fatalf("confirm manifest %s: %v", binLabel, err)
	}

	env := testEnvelope()
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID:   orderUUID,
		PayloadCode: bp.Code,
		Quantity:    1,
		Steps: []protocol.ComplexOrderStep{
			{Action: "wait", Node: lineNode.Name},
			{Action: "pickup", Node: lineNode.Name},
			{Action: "dropoff", Node: destNode.Name},
		},
		// nil at creation — release path is what we're testing.
	})

	order, err := db.GetOrderByUUID(orderUUID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.BinID == nil {
		t.Fatalf("expected order.BinID to be set by claimComplexBins; got nil")
	}
	if *order.BinID != bin.ID {
		t.Fatalf("expected order to claim bin %d, got %d", bin.ID, *order.BinID)
	}

	// Force StatusStaged so HandleOrderRelease accepts the release.
	if err := db.UpdateOrderStatus(order.ID, StatusStaged, "test: simulate robot waiting"); err != nil {
		t.Fatalf("set order staged: %v", err)
	}
	order, _ = db.GetOrderByUUID(orderUUID)
	return order, bin
}

func TestHandleOrderRelease_RemainingUOPZeroClearsManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	_, bin := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-rel-zero", "BIN-REL-ZERO")

	// NOTHING PULLED disposition → remaining_uop=0 → manifest cleared.
	zero := 0
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-rel-zero",
		RemainingUOP: &zero,
	})

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (cleared on release)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed should be false after release-clear")
	}
	// Claim must remain — release does not unclaim.
	if got.ClaimedBy == nil {
		t.Error("ClaimedBy should be preserved after release-clear")
	}
}

func TestHandleOrderRelease_RemainingUOPPositiveSyncsUOP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	_, bin := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-rel-pos", "BIN-REL-POS")

	// SEND PARTIAL BACK disposition → remaining_uop=positive → UOP synced,
	// manifest preserved.
	partial := 800
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-rel-pos",
		RemainingUOP: &partial,
	})

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 800 {
		t.Errorf("UOPRemaining = %d, want 800 (synced from release)", got.UOPRemaining)
	}
	if got.PayloadCode != bp.Code {
		t.Errorf("PayloadCode = %q, want %q (preserved)", got.PayloadCode, bp.Code)
	}
	if got.Manifest == nil {
		t.Error("Manifest should be preserved on partial-back release")
	}
}

func TestHandleOrderRelease_RemainingUOPNilLeavesManifestAlone(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	_, bin := stageComplexOrderWithLineBin(t, db, d, lineNode, bp, "uuid-rel-nil", "BIN-REL-NIL")

	before, _ := db.GetBin(bin.ID)

	// Legacy / Order-A path: nil remaining_uop → no manifest action.
	// Preserves pre-Phase-8 behavior: release dispatches without touching
	// the bin's manifest.
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID: "uuid-rel-nil",
		// RemainingUOP omitted (nil)
	})

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != before.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q (untouched on nil release)", got.PayloadCode, before.PayloadCode)
	}
	if got.UOPRemaining != before.UOPRemaining {
		t.Errorf("UOPRemaining = %d, want %d (untouched on nil release)", got.UOPRemaining, before.UOPRemaining)
	}
}

// TestHandleOrderRelease_BinIDNilFallbackClearsManifest verifies the
// source-node fallback path. Setup: an order with order.BinID=nil but a
// bin sitting at order.SourceNode (the line). This is the production
// failure mode for two-robot Order B observed on ALN_002 plant test
// 2026-04-23 — claimComplexBins didn't populate BinID, but the bin is
// physically at the line and the operator's release wants its manifest
// cleared. Without the fallback, HandleOrderRelease silently skipped the
// sync and the bin landed at OutboundDestination still tagged.
//
// With the fallback: HandleOrderRelease detects BinID==nil, looks up the
// bin at order.SourceNode, calls SyncOrClearForReleasedNoOwner (which
// drops the claim guard since the fallback bin isn't claimed by this
// order), and the manifest clears.
func TestHandleOrderRelease_BinIDNilFallbackClearsManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	// Bin physically at the line with manifest intact (the OLD bin
	// the line consumed down to zero — Edge knows it's empty, Core
	// still has the loaded state because there's no cycle telemetry).
	bin := &store.Bin{BinTypeID: 1, Label: "BIN-FALLBACK", NodeID: &lineNode.ID, Status: "staged"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	if err := db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART-A","qty":100}]}`, bp.Code, 100); err != nil {
		t.Fatalf("set manifest: %v", err)
	}

	// Order whose BinID is nil but whose SourceNode points at the line.
	// Mimics the production failure mode where claimComplexBins didn't
	// claim a bin for the order at creation time.
	order := &store.Order{
		EdgeUUID:     "uuid-fallback-clear",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusStaged,
		Quantity:     1,
		SourceNode:   lineNode.Name,
		DeliveryNode: "OUTBOUND-DEST",
		PayloadCode:  bp.Code,
		StepsJSON:    `[{"action":"wait","node":"` + lineNode.Name + `"},{"action":"pickup","node":"` + lineNode.Name + `"},{"action":"dropoff","node":"OUTBOUND-DEST"}]`,
		// BinID intentionally nil
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// Force StatusStaged (CreateOrder may default to pending).
	if err := db.UpdateOrderStatus(order.ID, StatusStaged, "test: fallback scenario"); err != nil {
		t.Fatalf("set order staged: %v", err)
	}

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// Operator release with NOTHING PULLED → remaining_uop=0.
	zero := 0
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-fallback-clear",
		RemainingUOP: &zero,
	})

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (cleared via source-node fallback)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0 (cleared via fallback)", got.UOPRemaining)
	}
	// The bin was NOT claimed by this order — the no-owner variant
	// should leave claimed_by alone (it was nil, stays nil).
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil (fallback should not claim)", got.ClaimedBy)
	}
}

// TestHandleOrderRelease_BinIDNilFallbackSyncsPartial verifies the
// fallback path for SEND PARTIAL BACK (positive remaining_uop).
func TestHandleOrderRelease_BinIDNilFallbackSyncsPartial(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)

	bin := &store.Bin{BinTypeID: 1, Label: "BIN-FALLBACK-PART", NodeID: &lineNode.ID, Status: "staged"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}
	manifest := `{"items":[{"catid":"PART-A","qty":100}]}`
	if err := db.SetBinManifest(bin.ID, manifest, bp.Code, 100); err != nil {
		t.Fatalf("set manifest: %v", err)
	}

	order := &store.Order{
		EdgeUUID:     "uuid-fallback-partial",
		StationID:    "line-1",
		OrderType:    OrderTypeComplex,
		Status:       StatusStaged,
		Quantity:     1,
		SourceNode:   lineNode.Name,
		DeliveryNode: "OUTBOUND-DEST",
		PayloadCode:  bp.Code,
		StepsJSON:    `[{"action":"wait","node":"` + lineNode.Name + `"},{"action":"pickup","node":"` + lineNode.Name + `"},{"action":"dropoff","node":"OUTBOUND-DEST"}]`,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(order.ID, StatusStaged, "test: fallback partial scenario"); err != nil {
		t.Fatalf("set order staged: %v", err)
	}

	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	partial := 37
	d.HandleOrderRelease(testEnvelope(), &protocol.OrderRelease{
		OrderUUID:    "uuid-fallback-partial",
		RemainingUOP: &partial,
	})

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 37 {
		t.Errorf("UOPRemaining = %d, want 37 (synced via fallback)", got.UOPRemaining)
	}
	if got.PayloadCode != bp.Code {
		t.Errorf("PayloadCode = %q, want %q (preserved on partial-back)", got.PayloadCode, bp.Code)
	}
} 