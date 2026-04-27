//go:build docker

package service

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/orders"
)

func testDB(t *testing.T) *store.DB {
	return testdb.Open(t)
}

// createTestBin creates a bin at the given node with a manifest and returns it.
func createTestBin(t *testing.T, db *store.DB, nodeID int64, label, payloadCode string, uop int) *bins.Bin {
	t.Helper()
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		bt = &bins.BinType{Code: "DEFAULT", Description: "Default test bin type"}
		if err := db.CreateBinType(bt); err != nil {
			t.Fatalf("create default bin type: %v", err)
		}
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin %s: %v", label, err)
	}
	if payloadCode != "" {
		if err := db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART","qty":100}]}`, payloadCode, uop); err != nil {
			t.Fatalf("set manifest for bin %s: %v", label, err)
		}
		if err := db.ConfirmBinManifest(bin.ID, ""); err != nil {
			t.Fatalf("confirm manifest for bin %s: %v", label, err)
		}
	}
	got, _ := db.GetBin(bin.ID)
	return got
}

var testOrderSeq int64

func createTestOrder(t *testing.T, db *store.DB, nodeID int64) *orders.Order {
	t.Helper()
	seq := atomic.AddInt64(&testOrderSeq, 1)
	node, _ := db.GetNode(nodeID)
	order := &orders.Order{
		EdgeUUID:     fmt.Sprintf("test-order-%s-%d", t.Name(), seq),
		StationID:    "test",
		OrderType:    "move",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: node.Name,
	}
	if err := db.CreateOrder(order); err != nil {
		t.Fatalf("create order: %v", err)
	}
	return order
}

func TestBinManifestService_ClearForReuse(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CLR-1", "PART-A", 100)

	// Verify bin has manifest before clear
	if bin.PayloadCode == "" {
		t.Fatal("expected bin to have payload_code before clear")
	}
	if bin.Manifest == nil {
		t.Fatal("expected bin to have manifest before clear")
	}

	// Clear the bin
	if err := svc.ClearForReuse(bin.ID); err != nil {
		t.Fatalf("ClearForReuse: %v", err)
	}

	// Verify cleared state
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin after clear: %v", err)
	}
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty", got.PayloadCode)
	}
	if got.Manifest != nil && *got.Manifest != "" {
		t.Errorf("Manifest = %v, want nil or empty", got.Manifest)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed = true, want false")
	}
}

func TestBinManifestService_ClearForReuse_MakesVisibleToFindEmpty(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	// Link payload to bin type for FindEmptyCompatibleBin
	db.SetPayloadBinTypes(sd.Payload.ID, []int64{sd.BinType.ID})

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-VIS-1", sd.Payload.Code, 100)

	// Bin with manifest should NOT be found by FindEmptyCompatibleBin
	_, err := db.FindEmptyCompatibleBin(sd.Payload.Code, "", 0)
	if err == nil {
		t.Fatal("expected FindEmptyCompatibleBin to return error for bin with manifest")
	}

	// Clear the bin
	if err := svc.ClearForReuse(bin.ID); err != nil {
		t.Fatalf("ClearForReuse: %v", err)
	}

	// Now FindEmptyCompatibleBin should find it
	found, err := db.FindEmptyCompatibleBin(sd.Payload.Code, "", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatibleBin after clear: %v", err)
	}
	if found.ID != bin.ID {
		t.Errorf("found bin %d, want %d", found.ID, bin.ID)
	}
}

func TestBinManifestService_SyncUOP_PreservesManifest(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-UOP-1", "PART-A", 100)

	originalManifest := *bin.Manifest
	originalPayloadCode := bin.PayloadCode

	// Sync UOP to partial consumption value
	if err := svc.SyncUOP(bin.ID, 42); err != nil {
		t.Fatalf("SyncUOP: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 42 {
		t.Errorf("UOPRemaining = %d, want 42", got.UOPRemaining)
	}
	if got.PayloadCode != originalPayloadCode {
		t.Errorf("PayloadCode = %q, want %q (should be preserved)", got.PayloadCode, originalPayloadCode)
	}
	if got.Manifest == nil || *got.Manifest != originalManifest {
		t.Errorf("Manifest changed after SyncUOP — should be preserved")
	}
}

func TestBinManifestService_ClearAndClaim_Atomic(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-AC-1", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// Atomic clear and claim
	if err := svc.ClearAndClaim(bin.ID, order.ID); err != nil {
		t.Fatalf("ClearAndClaim: %v", err)
	}

	got, _ := db.GetBin(bin.ID)

	// Verify manifest cleared
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}

	// Verify claimed
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, order.ID)
	}
}

func TestBinManifestService_ClearAndClaim_FailsIfAlreadyClaimed(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-AC-2", "PART-A", 100)
	order1 := createTestOrder(t, db, sd.LineNode.ID)
	order2 := createTestOrder(t, db, sd.LineNode.ID)

	// First claim succeeds
	if err := svc.ClearAndClaim(bin.ID, order1.ID); err != nil {
		t.Fatalf("first ClearAndClaim: %v", err)
	}

	// Second claim must fail (bin already claimed)
	err := svc.ClearAndClaim(bin.ID, order2.ID)
	if err == nil {
		t.Fatal("expected second ClearAndClaim to fail, got nil")
	}

	// Verify original claim intact
	got, _ := db.GetBin(bin.ID)
	if got.ClaimedBy == nil || *got.ClaimedBy != order1.ID {
		t.Errorf("ClaimedBy = %v, want %d (original claim)", got.ClaimedBy, order1.ID)
	}
}

func TestBinManifestService_SyncUOPAndClaim(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SC-1", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	originalManifest := *bin.Manifest

	// Sync UOP and claim atomically
	if err := svc.SyncUOPAndClaim(bin.ID, order.ID, 37); err != nil {
		t.Fatalf("SyncUOPAndClaim: %v", err)
	}

	got, _ := db.GetBin(bin.ID)

	// Verify UOP synced
	if got.UOPRemaining != 37 {
		t.Errorf("UOPRemaining = %d, want 37", got.UOPRemaining)
	}

	// Verify manifest preserved
	if got.Manifest == nil || *got.Manifest != originalManifest {
		t.Error("Manifest changed after SyncUOPAndClaim — should be preserved")
	}
	if got.PayloadCode != bin.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q", got.PayloadCode, bin.PayloadCode)
	}

	// Verify claimed
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, order.ID)
	}
}

func TestBinManifestService_ClaimForDispatch_NilIsPlainClaim(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CD-1", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// nil = no UOP change, plain claim
	if err := svc.ClaimForDispatch(bin.ID, order.ID, nil); err != nil {
		t.Fatalf("ClaimForDispatch(nil): %v", err)
	}

	got, _ := db.GetBin(bin.ID)

	// Claimed
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, order.ID)
	}

	// Manifest and UOP unchanged
	if got.PayloadCode != bin.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q (unchanged)", got.PayloadCode, bin.PayloadCode)
	}
	if got.UOPRemaining != bin.UOPRemaining {
		t.Errorf("UOPRemaining = %d, want %d (unchanged)", got.UOPRemaining, bin.UOPRemaining)
	}
}

func TestBinManifestService_ClaimForDispatch_ZeroClearsManifest(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CD-2", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// 0 = full depletion, clear manifest + claim
	zero := 0
	if err := svc.ClaimForDispatch(bin.ID, order.ID, &zero); err != nil {
		t.Fatalf("ClaimForDispatch(0): %v", err)
	}

	got, _ := db.GetBin(bin.ID)

	// Claimed
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, order.ID)
	}

	// Manifest cleared
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (cleared)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}
}

func TestBinManifestService_ClaimForDispatch_PositiveSyncsUOP(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CD-3", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// >0 = partial consumption, sync UOP + claim
	partial := 55
	if err := svc.ClaimForDispatch(bin.ID, order.ID, &partial); err != nil {
		t.Fatalf("ClaimForDispatch(55): %v", err)
	}

	got, _ := db.GetBin(bin.ID)

	// Claimed
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, order.ID)
	}

	// UOP synced but manifest preserved
	if got.UOPRemaining != 55 {
		t.Errorf("UOPRemaining = %d, want 55", got.UOPRemaining)
	}
	if got.PayloadCode != bin.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q (preserved)", got.PayloadCode, bin.PayloadCode)
	}
}

func TestBinManifestService_SetForProduction(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	// Create an empty bin (no manifest)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SFP-1", "", 0)

	manifest := `{"items":[{"catid":"WIDGET","qty":50}]}`
	if err := svc.SetForProduction(bin.ID, manifest, "WIDGET-X", 200); err != nil {
		t.Fatalf("SetForProduction: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "WIDGET-X" {
		t.Errorf("PayloadCode = %q, want %q", got.PayloadCode, "WIDGET-X")
	}
	if got.UOPRemaining != 200 {
		t.Errorf("UOPRemaining = %d, want 200", got.UOPRemaining)
	}
	if got.Manifest == nil {
		t.Error("Manifest is nil, expected non-nil")
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
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed = true, want false (not confirmed yet)")
	}
}

func TestBinManifestService_Confirm(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CNF-1", "PART-A", 100)

	// Unconfirm first so we can test confirm
	db.Exec("UPDATE bins SET manifest_confirmed=false WHERE id=$1", bin.ID)

	if err := svc.Confirm(bin.ID, "2026-03-30T12:00:00Z"); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if !got.ManifestConfirmed {
		t.Error("ManifestConfirmed = false, want true after Confirm")
	}
	if got.LoadedAt == nil {
		t.Error("LoadedAt = nil, want non-nil after Confirm")
	}
}

func TestBinManifestService_Unconfirm(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	// createTestBin confirms by default; Unconfirm should flip it back.
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-UNC-1", "PART-A", 100)
	if !bin.ManifestConfirmed {
		t.Fatal("expected test bin to start confirmed")
	}

	if err := svc.Unconfirm(bin.ID); err != nil {
		t.Fatalf("Unconfirm: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed = true, want false after Unconfirm")
	}
}

func TestBinManifestService_ClearAndClaim_FailsIfLocked(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-LCK-1", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// Lock the bin
	db.Exec("UPDATE bins SET locked=true WHERE id=$1", bin.ID)

	err := svc.ClearAndClaim(bin.ID, order.ID)
	if err == nil {
		t.Fatal("expected ClearAndClaim to fail on locked bin, got nil")
	}

	// Verify bin unchanged
	got, _ := db.GetBin(bin.ID)
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil (locked bin should not be claimable)", got.ClaimedBy)
	}
}

// TestBinManifestService_ClaimForDispatch_ConcurrentRace verifies that when two
// goroutines race ClaimForDispatch on the same bin with different remainingUOP
// values (one ClearAndClaim, one SyncUOPAndClaim), exactly one wins and the bin
// ends up in the correct state for the winner's operation.
func TestBinManifestService_ClaimForDispatch_ConcurrentRace(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	// Create a bin with a manifest
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-RACE-1", "PART-A", 100)
	order1 := createTestOrder(t, db, sd.LineNode.ID)
	order2 := createTestOrder(t, db, sd.LineNode.ID)

	originalPayloadCode := bin.PayloadCode

	// Goroutine 1: ClearAndClaim (remainingUOP=0, clears manifest)
	// Goroutine 2: SyncUOPAndClaim (remainingUOP=42, preserves manifest)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)

	go func() {
		defer wg.Done()
		zero := 0
		errs[0] = svc.ClaimForDispatch(bin.ID, order1.ID, &zero)
	}()
	go func() {
		defer wg.Done()
		partial := 42
		errs[1] = svc.ClaimForDispatch(bin.ID, order2.ID, &partial)
	}()
	wg.Wait()

	// Exactly one should succeed
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		}
	}
	if successCount != 1 {
		t.Errorf("expected exactly 1 successful claim, got %d (errs: %v)", successCount, errs)
	}

	// Verify bin is in a consistent state
	got, _ := db.GetBin(bin.ID)
	if got.ClaimedBy == nil {
		t.Fatal("bin should be claimed by the winner")
	}

	// Verify manifest state matches the winner's operation
	if errs[0] == nil {
		// ClearAndClaim won: manifest should be cleared
		if got.PayloadCode != "" {
			t.Errorf("ClearAndClaim won but PayloadCode = %q, want empty", got.PayloadCode)
		}
		if got.UOPRemaining != 0 {
			t.Errorf("ClearAndClaim won but UOPRemaining = %d, want 0", got.UOPRemaining)
		}
	} else {
		// SyncUOPAndClaim won: manifest preserved, UOP=42
		if got.PayloadCode != originalPayloadCode {
			t.Errorf("SyncUOPAndClaim won but PayloadCode = %q, want %q", got.PayloadCode, originalPayloadCode)
		}
		if got.UOPRemaining != 42 {
			t.Errorf("SyncUOPAndClaim won but UOPRemaining = %d, want 42", got.UOPRemaining)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// SyncOrClearForReleased — late-binding manifest mutation on already-claimed
// bins. Used by HandleOrderRelease to apply the operator's release-time
// disposition (NOTHING PULLED → 0, SEND PARTIAL BACK → positive, legacy → nil).
//
// Invariants under test:
//   - nil  → no-op (manifest, UOP, claim untouched)
//   - 0    → manifest cleared, claim preserved
//   - >0   → UOP synced, manifest + claim preserved
//   - guard: claimed_by must equal the supplied orderID
//   - guard: locked=false (bins under active fleet handling are off-limits)
//   - retry-safe: repeating the same call leaves the row in the same state
// ──────────────────────────────────────────────────────────────────────────

// claimBinForTest sets claimed_by directly so SyncOrClearForReleased's
// already-claimed precondition is met. Mirrors what claimComplexBins would
// have done at order creation time.
func claimBinForTest(t *testing.T, db *store.DB, binID, orderID int64) {
	t.Helper()
	if err := db.ClaimBin(binID, orderID); err != nil {
		t.Fatalf("claim bin %d for order %d: %v", binID, orderID, err)
	}
}

func TestBinManifestService_SyncOrClearForReleased_NilIsNoOp(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-NIL", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	if err := svc.SyncOrClearForReleased(bin.ID, order.ID, nil, ""); err != nil {
		t.Fatalf("SyncOrClearForReleased(nil): %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != bin.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q (unchanged)", got.PayloadCode, bin.PayloadCode)
	}
	if got.UOPRemaining != bin.UOPRemaining {
		t.Errorf("UOPRemaining = %d, want %d (unchanged)", got.UOPRemaining, bin.UOPRemaining)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d (preserved)", got.ClaimedBy, order.ID)
	}
}

func TestBinManifestService_SyncOrClearForReleased_ZeroClearsManifest(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-ZERO", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	zero := 0
	if err := svc.SyncOrClearForReleased(bin.ID, order.ID, &zero, ""); err != nil {
		t.Fatalf("SyncOrClearForReleased(0): %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (cleared)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}
	if got.Manifest != nil {
		t.Errorf("Manifest = %v, want nil (cleared)", got.Manifest)
	}
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed = true, want false (cleared)")
	}
	// Claim must be preserved — release does not unclaim the bin.
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d (preserved)", got.ClaimedBy, order.ID)
	}
}

func TestBinManifestService_SyncOrClearForReleased_PositiveSyncsUOP(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-POS", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	originalManifest := *bin.Manifest

	partial := 800
	if err := svc.SyncOrClearForReleased(bin.ID, order.ID, &partial, ""); err != nil {
		t.Fatalf("SyncOrClearForReleased(800): %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 800 {
		t.Errorf("UOPRemaining = %d, want 800", got.UOPRemaining)
	}
	if got.PayloadCode != bin.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q (preserved)", got.PayloadCode, bin.PayloadCode)
	}
	if got.Manifest == nil || *got.Manifest != originalManifest {
		t.Error("Manifest changed; want preserved")
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d (preserved)", got.ClaimedBy, order.ID)
	}
}

// TestBinManifestService_SyncOrClearForReleased_WrongOrderRejected verifies
// the claimed_by=$orderID guard. A stale release (e.g. the bin was reassigned
// to a different order between staging and release) must not stomp the new
// claim's bin state.
func TestBinManifestService_SyncOrClearForReleased_WrongOrderRejected(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-WRONG", "PART-A", 100)
	realOwner := createTestOrder(t, db, sd.LineNode.ID)
	staleOrder := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, realOwner.ID)

	zero := 0
	err := svc.SyncOrClearForReleased(bin.ID, staleOrder.ID, &zero, "")
	if err == nil {
		t.Fatal("expected error when orderID does not match claimed_by, got nil")
	}

	// Bin must be untouched.
	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != bin.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q (untouched after rejected sync)", got.PayloadCode, bin.PayloadCode)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != realOwner.ID {
		t.Errorf("ClaimedBy = %v, want %d (real owner preserved)", got.ClaimedBy, realOwner.ID)
	}
}

// TestBinManifestService_SyncOrClearForReleased_LockedRejected verifies the
// locked=false guard. A bin under active fleet handling (locked=true) must
// not have its manifest mutated mid-flight.
func TestBinManifestService_SyncOrClearForReleased_LockedRejected(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-LOCK", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)
	if _, err := db.Exec("UPDATE bins SET locked=true WHERE id=$1", bin.ID); err != nil {
		t.Fatalf("lock bin: %v", err)
	}

	zero := 0
	err := svc.SyncOrClearForReleased(bin.ID, order.ID, &zero, "")
	if err == nil {
		t.Fatal("expected error when bin is locked, got nil")
	}

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != bin.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q (untouched on locked bin)", got.PayloadCode, bin.PayloadCode)
	}
}

// TestBinManifestService_SyncOrClearForReleased_ActorOnAuditRow verifies
// that the caller's actor identity lands on the audit row, and that an
// empty actor falls back to "system" for consistency with other bin
// audits (claimComplexBins, etc.).
func TestBinManifestService_SyncOrClearForReleased_ActorOnAuditRow(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	// Named actor (e.g. the operator's station name from called_by)
	binNamed := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-ACTOR-N", "PART-A", 100)
	orderNamed := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, binNamed.ID, orderNamed.ID)
	zero := 0
	if err := svc.SyncOrClearForReleased(binNamed.ID, orderNamed.ID, &zero, "stephen-station-7"); err != nil {
		t.Fatalf("SyncOrClearForReleased named actor: %v", err)
	}

	// Empty actor — should fall back to "system" in the audit row
	binSystem := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-ACTOR-S", "PART-A", 100)
	orderSystem := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, binSystem.ID, orderSystem.ID)
	if err := svc.SyncOrClearForReleased(binSystem.ID, orderSystem.ID, &zero, ""); err != nil {
		t.Fatalf("SyncOrClearForReleased empty actor: %v", err)
	}

	// Query audit log for both bins and verify the actor column.
	rows, err := db.Query(`
		SELECT entity_id, actor FROM audit_log
		WHERE entity_type='bin' AND action='released_empty' AND entity_id IN ($1, $2)
		ORDER BY id`,
		binNamed.ID, binSystem.ID)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	defer rows.Close()
	seen := map[int64]string{}
	for rows.Next() {
		var id int64
		var actor string
		if err := rows.Scan(&id, &actor); err != nil {
			t.Fatalf("scan audit_log: %v", err)
		}
		seen[id] = actor
	}
	if seen[binNamed.ID] != "stephen-station-7" {
		t.Errorf("named-actor audit: got %q, want %q", seen[binNamed.ID], "stephen-station-7")
	}
	if seen[binSystem.ID] != "system" {
		t.Errorf("empty-actor audit: got %q, want %q (fallback)", seen[binSystem.ID], "system")
	}
}

// TestBinManifestService_SyncOrClearForReleased_IdempotentRetry verifies that
// running the same call twice (e.g. retry after a transient failure on the
// caller side) leaves the bin in the same end state and does not error on
// the second call.
func TestBinManifestService_SyncOrClearForReleased_IdempotentRetry(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-IDEMP", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	partial := 250
	if err := svc.SyncOrClearForReleased(bin.ID, order.ID, &partial, ""); err != nil {
		t.Fatalf("first SyncOrClearForReleased: %v", err)
	}
	if err := svc.SyncOrClearForReleased(bin.ID, order.ID, &partial, ""); err != nil {
		t.Fatalf("second SyncOrClearForReleased should succeed (idempotent): %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 250 {
		t.Errorf("UOPRemaining = %d, want 250 (idempotent retry)", got.UOPRemaining)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d (preserved across retries)", got.ClaimedBy, order.ID)
	}
}
