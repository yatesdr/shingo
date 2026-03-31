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
