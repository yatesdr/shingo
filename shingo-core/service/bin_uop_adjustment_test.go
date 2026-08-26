//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/payloads"
)

// TestBinService_RecordCount_WarnsAndAcceptsAboveCapacity pins the warn-and-accept
// behaviour: a count above the payload's UOP capacity is APPLIED (a bin can be
// physically overpacked, and a hard block stopped the operator from recording the
// truth), but the result carries a warning for the bin tab.
func TestBinService_RecordCount_WarnsAndAcceptsAboveCapacity(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	p := &payloads.Payload{Code: "CAP-TEST", Description: "capacity test", UOPCapacity: 1200}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-CAP-HI", "CAP-TEST", 500)
	res, err := svc.RecordCount(bin, 1201, "admin")
	if err != nil {
		t.Fatalf("above-capacity count must be accepted, got error: %v", err)
	}
	if res.Actual != 1201 {
		t.Errorf("Actual = %d, want 1201", res.Actual)
	}
	if res.Warning == "" {
		t.Error("above-capacity count must return a warning")
	}
	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 1201 {
		t.Errorf("UOPRemaining = %d, want 1201 (count applied over capacity)", got.UOPRemaining)
	}
}

// TestBinService_RecordCount_OverCapacityWritesAuditLine pins the distinct
// over-capacity audit row (op=cycle_count_over_capacity) that carries the count
// vs the capacity, so a persistently-over count surfaces a mis-set capacity config.
func TestBinService_RecordCount_OverCapacityWritesAuditLine(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	p := &payloads.Payload{Code: "CAP-AUDIT", Description: "capacity audit", UOPCapacity: 100}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-CAP-AUD", "CAP-AUDIT", 50)
	if _, err := svc.RecordCount(bin, 150, "counter"); err != nil {
		t.Fatalf("RecordCount: %v", err)
	}

	var (
		count     int
		suggested int // before_uop carries the payload capacity
		actual    int // after_uop carries the operator's count
	)
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(before_uop),0), COALESCE(MAX(after_uop),0)
		FROM bin_uop_ledger WHERE bin_id=$1 AND op='cycle_count_over_capacity'`,
		bin.ID).Scan(&count, &suggested, &actual); err != nil {
		t.Fatalf("query over-capacity audit: %v", err)
	}
	if count != 1 {
		t.Fatalf("over-capacity audit rows = %d, want 1", count)
	}
	if suggested != 100 || actual != 150 {
		t.Errorf("audit capacity/count = %d/%d, want 100/150", suggested, actual)
	}
}

func TestBinService_RecordCount_RejectsNegative(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	p := &payloads.Payload{Code: "CAP-NEG", Description: "capacity test", UOPCapacity: 1200}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-CAP-NEG", "CAP-NEG", 500)
	_, err := svc.RecordCount(bin, -1, "admin")
	if err == nil {
		t.Fatal("expected error for actualUOP < 0, got nil")
	}
}

func TestBinService_RecordCount_AcceptsZero(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	p := &payloads.Payload{Code: "CAP-ZERO", Description: "capacity test", UOPCapacity: 1200}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-CAP-ZERO", "CAP-ZERO", 500)
	res, err := svc.RecordCount(bin, 0, "admin")
	if err != nil {
		t.Fatalf("RecordCount(0): %v", err)
	}
	if res.Actual != 0 {
		t.Errorf("Actual = %d, want 0", res.Actual)
	}
	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 0 {
		t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
	}
}

func TestBinService_RecordCount_AcceptsAtCapacity(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	p := &payloads.Payload{Code: "CAP-FULL", Description: "capacity test", UOPCapacity: 1200}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-CAP-FULL", "CAP-FULL", 500)
	res, err := svc.RecordCount(bin, 1200, "admin")
	if err != nil {
		t.Fatalf("RecordCount(1200): %v", err)
	}
	if res.Actual != 1200 {
		t.Errorf("Actual = %d, want 1200", res.Actual)
	}
	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 1200 {
		t.Errorf("UOPRemaining = %d, want 1200", got.UOPRemaining)
	}
}

func TestBinService_RecordCount_RejectsEmptyPayloadCode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-NO-PAY", "", 100)
	_, err := svc.RecordCount(bin, 50, "admin")
	if err == nil {
		t.Fatal("expected error for empty PayloadCode, got nil")
	}
}

func TestBinService_RecordCount_RejectsZeroCapacityPayload(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	p := &payloads.Payload{Code: "CAP-NOCAP", Description: "no capacity", UOPCapacity: 0}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-NOCAP", "CAP-NOCAP", 100)
	_, err := svc.RecordCount(bin, 50, "admin")
	if err == nil {
		t.Fatal("expected error for payload with UOPCapacity=0, got nil")
	}
}
