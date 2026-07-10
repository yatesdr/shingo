//go:build docker

package service

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
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
		testutil.MustNoErr(t, db.CreateBinType(bt), "create default bin type")
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
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	return order
}

func TestBinManifestService_ClearForReuse(t *testing.T) {
	t.Parallel()
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
	if _, err := svc.ClearForReuse(bin.ID, nil); err != nil {
		t.Fatalf("ClearForReuse"+": %v", err)
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
	t.Parallel()
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
	if _, err := svc.ClearForReuse(bin.ID, nil); err != nil {
		t.Fatalf("ClearForReuse"+": %v", err)
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

// (Item 14 D8: TestBinManifestService_SyncUOP_PreservesManifest deleted
// alongside the SyncUOP function it exercised. Production has zero
// callers — partial-consumption sync goes through ApplyBinUOPDelta in
// the post-bin-as-truth flow. SyncUOPAndClaim covers the
// claim-with-uop case directly.)

func TestBinManifestService_ClearForReuse_SetsBinTypeID(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	// Create a distinct bin type to stamp; CreateBinType fills bt.ID on success.
	dunnage := &bins.BinType{Code: "45x48-KD-T1", Description: "Knockdown dunnage"}
	testutil.MustNoErr(t, db.CreateBinType(dunnage), "create dunnage bin type")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DUNNAGE-1", "PART-A", 50)
	origTypeID := bin.BinTypeID

	before, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin pre-clear: %v", err)
	}
	newEpoch, err := svc.ClearForReuse(bin.ID, &dunnage.ID)
	if err != nil {
		t.Fatalf("ClearForReuse with binTypeID: %v", err)
	}

	after, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin post-clear: %v", err)
	}
	if after.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (manifest not cleared)", after.PayloadCode)
	}
	if after.BinTypeID != dunnage.ID {
		t.Errorf("BinTypeID = %d, want %d (dunnage type not stamped)", after.BinTypeID, dunnage.ID)
	}
	if after.BinTypeID == origTypeID {
		t.Error("BinTypeID unchanged from original — COALESCE did not write")
	}
	if newEpoch <= before.DeltaEpoch {
		t.Errorf("delta_epoch = %d, want > %d (epoch must bump on clear)", newEpoch, before.DeltaEpoch)
	}

	// Audit detail must record from→to codes, not just a bare boolean.
	var rawDetail []byte
	testutil.MustNoErr(t, db.QueryRow(
		`SELECT detail FROM bin_uop_audit WHERE bin_id=$1 ORDER BY id DESC LIMIT 1`, bin.ID,
	).Scan(&rawDetail), "load audit detail")
	var detail map[string]string
	testutil.MustNoErr(t, json.Unmarshal(rawDetail, &detail), "parse audit detail")
	if detail["dunnage_to"] != "45x48-KD-T1" {
		t.Errorf("audit detail dunnage_to = %q, want %q", detail["dunnage_to"], "45x48-KD-T1")
	}
	// from should be the original type code ("DEFAULT" from SetupStandardData).
	if detail["dunnage_from"] == "" {
		t.Errorf("audit detail dunnage_from is empty, want the prior bin-type code")
	}
}

func TestBinManifestService_ClearForReuse_NilLeavesTypeUnchanged(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DUNNAGE-2", "PART-B", 50)
	wantTypeID := bin.BinTypeID

	if _, err := svc.ClearForReuse(bin.ID, nil); err != nil {
		t.Fatalf("ClearForReuse(nil): %v", err)
	}

	after, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin post-clear: %v", err)
	}
	if after.BinTypeID != wantTypeID {
		t.Errorf("BinTypeID = %d, want %d (nil must leave type unchanged)", after.BinTypeID, wantTypeID)
	}
	if after.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty", after.PayloadCode)
	}
}

func TestBinManifestService_ClearAndClaim_Atomic(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-AC-1", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// Atomic clear and claim (guard requires a pending reservation first)
	testdb.ReserveBin(t, db, order.ID, bin.ID)
	testutil.MustNoErr(t, svc.ClearAndClaim(bin.ID, order.ID), "ClearAndClaim")

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
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-AC-2", "PART-A", 100)
	order1 := createTestOrder(t, db, sd.LineNode.ID)
	order2 := createTestOrder(t, db, sd.LineNode.ID)

	// First claim succeeds (guard requires a pending reservation first)
	testdb.ReserveBin(t, db, order1.ID, bin.ID)
	testutil.MustNoErr(t, svc.ClearAndClaim(bin.ID, order1.ID), "first ClearAndClaim")

	// Second claim must fail (bin already claimed) — claimed_by IS NULL fails
	// before the reservation guard, so order2 needs no reservation to prove this.
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
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SC-1", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	originalManifest := *bin.Manifest

	// Sync UOP and claim atomically (guard requires a pending reservation first)
	testdb.ReserveBin(t, db, order.ID, bin.ID)
	testutil.MustNoErr(t, svc.SyncUOPAndClaim(bin.ID, order.ID, 37), "SyncUOPAndClaim")

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
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CD-1", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// nil = no UOP change, plain claim
	testutil.MustNoErr(t, svc.ClaimForDispatch(bin.ID, order.ID, nil), "ClaimForDispatch(nil)")

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
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CD-2", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// 0 = full depletion, clear manifest + claim
	zero := 0
	testutil.MustNoErr(t, svc.ClaimForDispatch(bin.ID, order.ID, &zero), "ClaimForDispatch(0)")

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

// TestBinManifestService_ClaimForDispatch_NegativeClearsManifest pins the
// audit-the-class fix shipped with the bin-18 underflow remediation. The
// branch is unreachable from today's callers (complex_dispatch passes nil),
// but a future Edge build that threads RemainingUOP at intake — as the
// comment at complex_dispatch.go:248-252 anticipates — would inherit the
// <= 0 semantics. SME-lock washout: a captured count that exceeded the
// tracked count produces a depleted bin, not a partial one; ClearAndClaim
// is the right branch.
func TestBinManifestService_ClaimForDispatch_NegativeClearsManifest(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CD-NEG", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	neg := -1
	testutil.MustNoErr(t, svc.ClaimForDispatch(bin.ID, order.ID, &neg), "ClaimForDispatch(-1)")

	got, _ := db.GetBin(bin.ID)

	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, order.ID)
	}
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty (negative remainingUOP must take ClearAndClaim)", got.PayloadCode)
	}
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}
}

func TestBinManifestService_ClaimForDispatch_PositiveSyncsUOP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CD-3", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// >0 = partial consumption, sync UOP + claim
	partial := 55
	testutil.MustNoErr(t, svc.ClaimForDispatch(bin.ID, order.ID, &partial), "ClaimForDispatch(55)")

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
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	// Create an empty bin (no manifest)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SFP-1", "", 0)

	manifest := `{"items":[{"catid":"WIDGET","qty":50}]}`
	if _, err := svc.SetForProduction(bin.ID, manifest, "WIDGET-X", 200); err != nil {
		t.Fatalf("SetForProduction"+": %v", err)
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
		var gotJSON, wantJSON any
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
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-CNF-1", "PART-A", 100)

	// Unconfirm first so we can test confirm
	db.Exec("UPDATE bins SET manifest_confirmed=false WHERE id=$1", bin.ID)

	testutil.MustNoErr(t, svc.Confirm(bin.ID, "2026-03-30T12:00:00Z"), "Confirm")

	got, _ := db.GetBin(bin.ID)
	if !got.ManifestConfirmed {
		t.Error("ManifestConfirmed = false, want true after Confirm")
	}
	if got.LoadedAt == nil {
		t.Error("LoadedAt = nil, want non-nil after Confirm")
	}
}

// TestBinManifestService_RecordProducedBin_AtomicCountAndConfirm pins the
// manifest atomicity fix: RecordProducedBin sets the manifest AND confirms it in
// one transaction, so a produce finalize never leaves a counted-but-unconfirmed
// bin (invisible to kanban — manifest_confirmed gates drain/retrieve sources).
// The count, the
// confirm, and both audit rows land together.
func TestBinManifestService_RecordProducedBin_AtomicCountAndConfirm(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-RPB-1", "", 0)

	manifest := `{"items":[{"catid":"WIDGET","qty":50}]}`
	testutil.MustNoErr(t, svc.RecordProducedBin(bin.ID, manifest, "WIDGET-X", 200, "2026-03-30T12:00:00Z"), "RecordProducedBin")

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 200 {
		t.Errorf("UOPRemaining = %d, want 200 (count recorded)", got.UOPRemaining)
	}
	if !got.ManifestConfirmed {
		t.Error("ManifestConfirmed = false, want true — count and confirm must land together")
	}
	if got.LoadedAt == nil {
		t.Error("LoadedAt = nil, want non-nil after confirm")
	}
	var nSet, nConfirmed int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit WHERE bin_id=$1 AND op=$2`,
		bin.ID, string(audit.OpSetForProduction)).Scan(&nSet), "count set-for-production audit")
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit WHERE bin_id=$1 AND op=$2`,
		bin.ID, string(audit.OpManifestConfirmed)).Scan(&nConfirmed), "count manifest-confirmed audit")
	if nSet != 1 || nConfirmed != 1 {
		t.Errorf("audit rows: OpSetForProduction=%d OpManifestConfirmed=%d, want 1 each (one atomic tx)", nSet, nConfirmed)
	}
}

// TestBinManifestService_RecordProducedBin_RollsBackOnError pins the no-half-
// state guarantee: when RecordProducedBin errors (here: a nonexistent bin) the
// whole transaction rolls back — no count, no confirm, no audit rows. There is
// never a counted-but-unconfirmed bin.
func TestBinManifestService_RecordProducedBin_RollsBackOnError(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	const ghostBin int64 = 999999
	if err := svc.RecordProducedBin(ghostBin, `{"items":[]}`, "WIDGET-X", 200, ""); err == nil {
		t.Fatal("RecordProducedBin on a nonexistent bin: want error, got nil")
	}
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit WHERE bin_id=$1`, ghostBin).Scan(&n),
		"count audit rows for failed record")
	if n != 0 {
		t.Errorf("audit rows after a failed record = %d, want 0 (rolled back, no half-state)", n)
	}
}

func TestBinManifestService_Unconfirm(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	// createTestBin confirms by default; Unconfirm should flip it back.
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-UNC-1", "PART-A", 100)
	if !bin.ManifestConfirmed {
		t.Fatal("expected test bin to start confirmed")
	}

	testutil.MustNoErr(t, svc.Unconfirm(bin.ID), "Unconfirm")

	got, _ := db.GetBin(bin.ID)
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed = true, want false after Unconfirm")
	}
}

func TestBinManifestService_ClearAndClaim_FailsIfLocked(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-LCK-1", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)

	// Reserve first so the ONLY reason the claim fails below is the lock, not a
	// missing reservation (otherwise the test would pass for the wrong reason).
	testdb.ReserveBin(t, db, order.ID, bin.ID)

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

// TestBinManifestService_ClaimForDispatch_ConcurrentRace verifies that when N
// goroutines race ClaimForDispatch on the same bin with adversarial remainingUOP
// values (mix of ClearAndClaim, SyncUOPAndClaim, and plain ClaimBin), exactly
// one wins and the bin ends up in the correct state for that winner's operation.
//
// N=5 exercises three distinct claim operations concurrently — a more adversarial
// surface than the original N=2 (ClearAndClaim vs SyncUOPAndClaim only).
func TestBinManifestService_ClaimForDispatch_ConcurrentRace(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-RACE-N", "PART-A", 100)
	originalPayloadCode := bin.PayloadCode

	// Five adversarial callers:
	//   [0] ClearAndClaim  (remainingUOP=0)   — clears manifest
	//   [1] SyncUOPAndClaim(remainingUOP=42)  — preserves manifest, sets UOP=42
	//   [2] SyncUOPAndClaim(remainingUOP=77)  — preserves manifest, sets UOP=77
	//   [3] ClaimBin       (remainingUOP=nil) — plain claim, no manifest change
	//   [4] ClearAndClaim  (remainingUOP=0)   — second clear contender
	const N = 5
	uops := [N]*int{func() *int { v := 0; return &v }(),
		func() *int { v := 42; return &v }(),
		func() *int { v := 77; return &v }(),
		nil,
		func() *int { v := 0; return &v }(),
	}

	orders := make([]int64, N)
	for i := 0; i < N; i++ {
		o := createTestOrder(t, db, sd.LineNode.ID)
		orders[i] = o.ID
	}

	// Release all goroutines at once via a closed channel to maximise contention.
	ready := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-ready
			errs[i] = svc.ClaimForDispatch(bin.ID, orders[i], uops[i])
		}()
	}
	close(ready)
	wg.Wait()

	// Exactly one goroutine must succeed.
	winnerIdx := -1
	for i, err := range errs {
		if err == nil {
			if winnerIdx != -1 {
				t.Errorf("multiple winners: goroutine %d and goroutine %d both succeeded", winnerIdx, i)
			}
			winnerIdx = i
		}
	}
	if winnerIdx == -1 {
		t.Fatalf("no winner: all %d goroutines failed (errs: %v)", N, errs)
	}

	// Bin must be claimed and in a state consistent with the winner's operation.
	got, _ := db.GetBin(bin.ID)
	if got.ClaimedBy == nil {
		t.Fatal("bin should be claimed by the winner")
	}
	if *got.ClaimedBy != orders[winnerIdx] {
		t.Errorf("ClaimedBy = %d, want winner order %d", *got.ClaimedBy, orders[winnerIdx])
	}

	switch winnerIdx {
	case 0, 4: // ClearAndClaim won
		if got.PayloadCode != "" {
			t.Errorf("ClearAndClaim won but PayloadCode = %q, want empty", got.PayloadCode)
		}
		if got.UOPRemaining != 0 {
			t.Errorf("ClearAndClaim won but UOPRemaining = %d, want 0", got.UOPRemaining)
		}
	case 1: // SyncUOPAndClaim(42) won
		if got.PayloadCode != originalPayloadCode {
			t.Errorf("SyncUOPAndClaim(42) won but PayloadCode = %q, want %q", got.PayloadCode, originalPayloadCode)
		}
		if got.UOPRemaining != 42 {
			t.Errorf("SyncUOPAndClaim(42) won but UOPRemaining = %d, want 42", got.UOPRemaining)
		}
	case 2: // SyncUOPAndClaim(77) won
		if got.PayloadCode != originalPayloadCode {
			t.Errorf("SyncUOPAndClaim(77) won but PayloadCode = %q, want %q", got.PayloadCode, originalPayloadCode)
		}
		if got.UOPRemaining != 77 {
			t.Errorf("SyncUOPAndClaim(77) won but UOPRemaining = %d, want 77", got.UOPRemaining)
		}
	case 3: // ClaimBin (nil UOP) won — no manifest change
		if got.PayloadCode != originalPayloadCode {
			t.Errorf("ClaimBin won but PayloadCode = %q, want %q (unchanged)", got.PayloadCode, originalPayloadCode)
		}
		if got.UOPRemaining != 100 {
			t.Errorf("ClaimBin won but UOPRemaining = %d, want 100 (unchanged)", got.UOPRemaining)
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

// claimBinForTest claims a bin for an order so SyncOrClearForReleased's
// already-claimed precondition is met. Delegates to testdb.ClaimBinForTest,
// which reserves-then-claims (Acquire -> ClaimBin -> Confirm) as the demoted-CAS
// guard now requires — the same sequence ClaimForDispatch runs at dispatch time.
func claimBinForTest(t *testing.T, db *store.DB, binID, orderID int64) {
	t.Helper()
	testdb.ClaimBinForTest(t, db, binID, orderID)
}

func TestBinManifestService_SyncOrClearForReleased_NilIsNoOp(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-NIL", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, nil, "", ""), "SyncOrClearForReleased(nil)")

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
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-ZERO", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	zero := 0
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, &zero, "", ""), "SyncOrClearForReleased(0)")

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

// TestBinManifestService_SyncOrClearForReleased_ZeroBumpsEpoch verifies the
// release-empty path bumps delta_epoch, so a late tick from the retired load
// is recognizably cross-epoch and the applier drops it instead of corrupting
// the next load's count.
func TestBinManifestService_SyncOrClearForReleased_ZeroBumpsEpoch(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-EPOCH0", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	var before int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, bin.ID).Scan(&before), "epoch before")

	zero := 0
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, &zero, "", ""), "release empty")

	var after int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, bin.ID).Scan(&after), "epoch after")
	if after != before+1 {
		t.Errorf("delta_epoch = %d, want %d (release-empty must bump the epoch)", after, before+1)
	}
}

// TestBinManifestService_SyncOrClearForReleased_PositiveBumpsEpoch pins that
// the partial-release path also bumps delta_epoch — the authoritative count
// changed out-of-band from the delta stream, so the old generation's in-flight
// ticks must not double-count against the synced value.
func TestBinManifestService_SyncOrClearForReleased_PositiveBumpsEpoch(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-EPOCHP", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	var before int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, bin.ID).Scan(&before), "epoch before")

	partial := 80
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, &partial, "", ""), "release partial")

	var after int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, bin.ID).Scan(&after), "epoch after")
	if after != before+1 {
		t.Errorf("delta_epoch = %d, want %d (partial release must bump the epoch)", after, before+1)
	}
}

func TestBinManifestService_SyncOrClearForReleased_PositiveSyncsUOP(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-POS", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	partial := 800
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, &partial, "", ""), "SyncOrClearForReleased(800)")

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 800 {
		t.Errorf("UOPRemaining = %d, want 800", got.UOPRemaining)
	}
	if got.PayloadCode != bin.PayloadCode {
		t.Errorf("PayloadCode = %q, want %q (preserved)", got.PayloadCode, bin.PayloadCode)
	}
	// Post-#15: the manifest is reconstructed (not preserved unchanged)
	// to reflect the new uop_remaining. Single-payload normalization
	// makes this fully recoverable from payload_code + remainingUOP.
	if got.Manifest == nil {
		t.Fatal("Manifest = nil; want reconstructed single-payload manifest")
	}
	parsed, err := got.ParseManifest()
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("manifest items = %d, want 1 (single-payload normalization)", len(parsed.Items))
	}
	if parsed.Items[0].CatID != bin.PayloadCode {
		t.Errorf("manifest item CatID = %q, want %q (= payload_code)", parsed.Items[0].CatID, bin.PayloadCode)
	}
	if parsed.Items[0].Quantity != int64(partial) {
		t.Errorf("manifest item Quantity = %d, want %d (= remainingUOP)", parsed.Items[0].Quantity, partial)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d (preserved)", got.ClaimedBy, order.ID)
	}
}

// TestBinManifestService_SyncOrClearForReleased_PreservesLoadedAt is the D8
// invariant: SEND PARTIAL BACK (the remainingUOP>0 branch) must NOT re-stamp
// loaded_at. The dedicated-loader ranker's "a kept partial is the oldest bin of X
// and re-consumes itself" property depends on this — a loaded_at=NOW() here would
// silently invert the ranker (the partial would become the newest, never drained).
func TestBinManifestService_SyncOrClearForReleased_PreservesLoadedAt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-LA", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	// Backdate loaded_at to a known, non-null instant so the invariant is testable.
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := db.DB.Exec(`UPDATE bins SET loaded_at=$1 WHERE id=$2`, want, bin.ID); err != nil {
		t.Fatalf("backdate loaded_at: %v", err)
	}

	partial := 800
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, &partial, "", ""), "SyncOrClearForReleased(800)")

	got, _ := db.GetBin(bin.ID)
	if got.LoadedAt == nil {
		t.Fatal("loaded_at = NULL after partial return; want it preserved")
	}
	if !got.LoadedAt.Equal(want) {
		t.Errorf("loaded_at = %v, want %v (UNCHANGED across SEND PARTIAL BACK — the ranker depends on it)", got.LoadedAt.UTC(), want)
	}
	// Sanity: the partial DID sync, so we know the >0 branch actually ran.
	if got.UOPRemaining != 800 {
		t.Errorf("UOPRemaining = %d, want 800 (partial branch must have run)", got.UOPRemaining)
	}
}

// TestBinManifestService_SyncOrClearForReleased_WrongOrderRejected verifies
// the claimed_by=$orderID guard. A stale release (e.g. the bin was reassigned
// to a different order between staging and release) must not stomp the new
// claim's bin state.
func TestBinManifestService_SyncOrClearForReleased_WrongOrderRejected(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-WRONG", "PART-A", 100)
	realOwner := createTestOrder(t, db, sd.LineNode.ID)
	staleOrder := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, realOwner.ID)

	zero := 0
	err := svc.SyncOrClearForReleased(bin.ID, staleOrder.ID, &zero, "", "")
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
	t.Parallel()
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
	err := svc.SyncOrClearForReleased(bin.ID, order.ID, &zero, "", "")
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
// audits (ApplyComplexPlan, etc.).
func TestBinManifestService_SyncOrClearForReleased_ActorOnAuditRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	// Named actor (e.g. the operator's station name from called_by)
	binNamed := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-ACTOR-N", "PART-A", 100)
	orderNamed := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, binNamed.ID, orderNamed.ID)
	zero := 0
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(binNamed.ID, orderNamed.ID, &zero, "", "stephen-station-7"), "SyncOrClearForReleased named actor")

	// Empty actor — should fall back to "system" in the audit row
	binSystem := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-ACTOR-S", "PART-A", 100)
	orderSystem := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, binSystem.ID, orderSystem.ID)
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(binSystem.ID, orderSystem.ID, &zero, "", ""), "SyncOrClearForReleased empty actor")

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
		testutil.MustNoErr(t, rows.Scan(&id, &actor), "scan audit_log")
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
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-SOC-IDEMP", "PART-A", 100)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	partial := 250
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, &partial, "", ""), "first SyncOrClearForReleased")
	testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, &partial, "", ""), "second SyncOrClearForReleased should succeed (idempotent)")

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 250 {
		t.Errorf("UOPRemaining = %d, want 250 (idempotent retry)", got.UOPRemaining)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d (preserved across retries)", got.ClaimedBy, order.ID)
	}
}

// TestBinManifestService_SetFromTemplate pins the Item 19 audit-
// completeness contract: the SetFromTemplate wrapper resolves a
// payload template AND writes the bin via SetForProduction, which
// audits via bin_uop_audit. Pre-Item-19 the dispatch ingest path
// and the operator load-payload action called the lower-level
// *store.DB.SetBinManifestFromTemplate which bypassed audit; Item
// 10's UI surface made the audit-bypass a real gap.
func TestBinManifestService_SetFromTemplate(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-TMPL-1", "INITIAL", 0)

	// Apply the template — uopOverride=0 falls back to template's UOPCapacity.
	if _, err := svc.SetFromTemplate(bin.ID, sd.Payload.Code, 0); err != nil {
		t.Fatalf("SetFromTemplate"+": %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != sd.Payload.Code {
		t.Errorf("PayloadCode = %q, want %q", got.PayloadCode, sd.Payload.Code)
	}
	if got.UOPRemaining != sd.Payload.UOPCapacity {
		t.Errorf("UOPRemaining = %d, want %d (payload default)",
			got.UOPRemaining, sd.Payload.UOPCapacity)
	}

	// Audit row must exist with op=set_for_production.
	var auditCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit
		WHERE bin_id=$1 AND op='set_for_production'`, bin.ID).Scan(&auditCount); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("set_for_production audit rows = %d, want 1 (Item 19: SetFromTemplate must audit)",
			auditCount)
	}

	// Override uopOverride.
	if _, err := svc.SetFromTemplate(bin.ID, sd.Payload.Code, 50); err != nil {
		t.Fatalf("SetFromTemplate override"+": %v", err)
	}
	got2, _ := db.GetBin(bin.ID)
	if got2.UOPRemaining != 50 {
		t.Errorf("UOPRemaining after override = %d, want 50", got2.UOPRemaining)
	}
}

// TestRegression_ReleaseUnderpack_BinClearsToZero pins the
// underpack release contract on the manifest side: same wire shape
// as RELEASE EMPTY (RemainingUOP=&0), same end state (manifest
// cleared, uop=0, claim preserved). The disposition kind doesn't
// affect the bin write — only the audit op tag.
func TestRegression_ReleaseUnderpack_BinClearsToZero(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-UNDERPACK-1", "PART-UP", 1190)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	zero := 0
	if err := svc.SyncOrClearForReleased(bin.ID, order.ID, &zero,
		protocol.DispositionReleaseUnderpack, "operator-x"); err != nil {
		t.Fatalf("SyncOrClearForReleased underpack: %v", err)
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
	if got.ClaimedBy == nil || *got.ClaimedBy != order.ID {
		t.Errorf("ClaimedBy = %v, want %d (preserved)", got.ClaimedBy, order.ID)
	}
}

// TestRegression_ReleaseUnderpack_AuditRecordsMissingDelta pins the
// audit-row contract: op=released_underpack, before_uop = the bin's
// pre-release count (the system's expected count == "suggested"),
// after_uop = 0. The gap (before_uop - after_uop) is the missing-
// inventory delta forensics will trend.
//
// Without the distinct op tag the missing-inventory pattern would
// be indistinguishable from the system-and-operator-agreed-empty
// case (RELEASE EMPTY at runtime=0). Forensics need to be able to
// query for op=released_underpack and find every "labeled bin
// short-counted by N" event.
func TestRegression_ReleaseUnderpack_AuditRecordsMissingDelta(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	const expectedAtClick = 12
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-UNDERPACK-AUDIT", "PART-UPA", expectedAtClick)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	zero := 0
	if err := svc.SyncOrClearForReleased(bin.ID, order.ID, &zero,
		protocol.DispositionReleaseUnderpack, "operator-y"); err != nil {
		t.Fatalf("SyncOrClearForReleased underpack: %v", err)
	}

	// The audit row must use OpReleasedUnderpack so a forensics
	// query can target underpack events specifically.
	var (
		op        string
		beforeUOP int
		afterUOP  int
		actor     string
	)
	if err := db.QueryRow(`SELECT op, before_uop, after_uop, actor
		FROM bin_uop_audit WHERE bin_id=$1 AND op=$2`,
		bin.ID, audit.OpReleasedUnderpack).Scan(&op, &beforeUOP, &afterUOP, &actor); err != nil {
		t.Fatalf("read released_underpack audit row: %v", err)
	}
	if beforeUOP != expectedAtClick {
		t.Errorf("before_uop = %d, want %d (system's expected count at click time)",
			beforeUOP, expectedAtClick)
	}
	if afterUOP != 0 {
		t.Errorf("after_uop = %d, want 0", afterUOP)
	}
	missing := beforeUOP - afterUOP
	if missing != expectedAtClick {
		t.Errorf("missing-inventory delta = %d, want %d (gap forensics will read)",
			missing, expectedAtClick)
	}
	if actor != "operator-y" {
		t.Errorf("actor = %q, want operator-y", actor)
	}

	// And NO released_empty audit row should exist for this bin —
	// underpack is its own thing, not an extra event.
	var emptyCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit
		WHERE bin_id=$1 AND op=$2`, bin.ID, audit.OpReleasedEmpty).Scan(&emptyCount); err != nil {
		t.Fatalf("count released_empty rows: %v", err)
	}
	if emptyCount != 0 {
		t.Errorf("released_empty rows = %d, want 0 (underpack must not also write released_empty)",
			emptyCount)
	}
}

// TestRegression_ReleaseUnderpack_ManifestClears is the focused
// manifest-side pin: payload_code, manifest, manifest_confirmed,
// loaded_at all reset on underpack release (same as RELEASE EMPTY).
// Companion to the bin-clears test above; this one targets the
// fields the empty-bin-pool query reads.
func TestRegression_ReleaseUnderpack_ManifestClears(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewBinManifestService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-UNDERPACK-MANIFEST", "PART-UPM", 47)
	order := createTestOrder(t, db, sd.LineNode.ID)
	claimBinForTest(t, db, bin.ID, order.ID)

	// Confirm the manifest first so loaded_at is set; underpack
	// release must clear that too.
	testutil.MustNoErr(t, svc.Confirm(bin.ID, ""), "Confirm")
	pre, _ := db.GetBin(bin.ID)
	if !pre.ManifestConfirmed {
		t.Fatalf("pre-release ManifestConfirmed = false, want true (Confirm should have set it)")
	}

	zero := 0
	if err := svc.SyncOrClearForReleased(bin.ID, order.ID, &zero,
		protocol.DispositionReleaseUnderpack, "operator-z"); err != nil {
		t.Fatalf("SyncOrClearForReleased underpack: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty", got.PayloadCode)
	}
	if got.Manifest != nil {
		t.Errorf("Manifest = %v, want nil", got.Manifest)
	}
	if got.ManifestConfirmed {
		t.Error("ManifestConfirmed = true, want false (cleared)")
	}
	if got.LoadedAt != nil {
		t.Errorf("LoadedAt = %v, want nil (cleared)", got.LoadedAt)
	}
}
