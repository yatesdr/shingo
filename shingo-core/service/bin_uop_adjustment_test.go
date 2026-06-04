//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/payloads"
)

func TestBinService_RecordCount_RejectsAboveCapacity(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	p := &payloads.Payload{Code: "CAP-TEST", Description: "capacity test", UOPCapacity: 1200}
	testutil.MustNoErr(t, db.CreatePayload(p), "create payload")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-CAP-HI", "CAP-TEST", 500)
	_, err := svc.RecordCount(bin, 1201, "admin")
	if err == nil {
		t.Fatal("expected error for actualUOP > UOPCapacity, got nil")
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
