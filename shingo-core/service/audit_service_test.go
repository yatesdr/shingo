//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

func TestAuditService_Append_PersistsRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewAuditService(db)

	testutil.MustNoErr(t, svc.Append("bin", 42, "status", "available", "flagged", "tester"), "Append")

	entries, err := svc.ListForEntity("bin", 42)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	got := entries[0]
	if got.EntityType != "bin" {
		t.Errorf("EntityType = %q, want bin", got.EntityType)
	}
	if got.EntityID != 42 {
		t.Errorf("EntityID = %d, want 42", got.EntityID)
	}
	if got.Action != "status" {
		t.Errorf("Action = %q, want status", got.Action)
	}
	if got.OldValue != "available" || got.NewValue != "flagged" {
		t.Errorf("Old/New = %q/%q, want available/flagged", got.OldValue, got.NewValue)
	}
	if got.Actor != "tester" {
		t.Errorf("Actor = %q, want tester", got.Actor)
	}

	// Verify directly through *store.DB so we know the row is real, not just
	// echoed from a service-side cache.
	dbRows, err := db.ListEntityAudit("bin", 42)
	if err != nil {
		t.Fatalf("db.ListEntityAudit: %v", err)
	}
	if len(dbRows) != 1 || dbRows[0].ID != got.ID {
		t.Errorf("db rows = %+v, want 1 matching service id %d", dbRows, got.ID)
	}
}

func TestAuditService_ListForEntity_OrderedNewestFirst(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewAuditService(db)

	for _, action := range []string{"a", "b", "c"} {
		if err := svc.Append("order", 7, action, "", "", "ui"); err != nil {
			t.Fatalf("Append %s: %v", action, err)
		}
	}

	entries, err := svc.ListForEntity("order", 7)
	if err != nil {
		t.Fatalf("ListForEntity: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
	// Most recent first: actions written in a,b,c order should come back as c,b,a.
	if entries[0].Action != "c" || entries[1].Action != "b" || entries[2].Action != "a" {
		t.Errorf("order = %q,%q,%q, want c,b,a",
			entries[0].Action, entries[1].Action, entries[2].Action)
	}
}

func TestAuditService_ListForEntity_FiltersByEntity(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewAuditService(db)

	// Two different entities, same type.
	testutil.MustNoErr(t, svc.Append("bin", 1, "status", "", "available", "ui"), "Append bin/1")
	testutil.MustNoErr(t, svc.Append("bin", 2, "status", "", "flagged", "ui"), "Append bin/2")
	// Different type, same id as first.
	testutil.MustNoErr(t, svc.Append("order", 1, "created", "", "", "ui"), "Append order/1")

	bin1, err := svc.ListForEntity("bin", 1)
	if err != nil {
		t.Fatalf("ListForEntity bin/1: %v", err)
	}
	if len(bin1) != 1 || bin1[0].EntityID != 1 || bin1[0].EntityType != "bin" {
		t.Errorf("bin/1 = %+v, want exactly the bin/1 row", bin1)
	}

	order1, err := svc.ListForEntity("order", 1)
	if err != nil {
		t.Fatalf("ListForEntity order/1: %v", err)
	}
	if len(order1) != 1 || order1[0].EntityID != 1 || order1[0].EntityType != "order" {
		t.Errorf("order/1 = %+v, want exactly the order/1 row", order1)
	}
}

// TestAuditService_ListBinUOPDiscrepancies_FiltersToRealDivergence verifies the
// discrepancy ledger: it surfaces stale-epoch drops, negative remaining, and
// release-empties that still carried counted parts — but not clean empties or
// ordinary cycle counts.
func TestAuditService_ListBinUOPDiscrepancies_FiltersToRealDivergence(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := NewAuditService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-DISCREP", "PART-A", 100)
	pi := func(n int) *int { return &n }

	// Clean release-empty (before == after == 0): NOT a discrepancy.
	testutil.MustNoErr(t, audit.AppendBinUOP(db.DB, bin.ID, pi(0), 0,
		audit.OpReleasedEmpty, "test", nil, "PART-A", "op", audit.BinUOPContext{}), "clean empty")
	// Lossy release-empty (40 counted, released as empty): discrepancy.
	testutil.MustNoErr(t, audit.AppendBinUOP(db.DB, bin.ID, pi(40), 0,
		audit.OpReleasedEmpty, "test", nil, "PART-A", "op", audit.BinUOPContext{}), "lossy empty")
	// Stale-epoch dropped observation (before == after): discrepancy.
	testutil.MustNoErr(t, audit.AppendBinUOPOverride(db.DB, bin.ID, 50, 50,
		audit.OpStaleEpochDropped, "test", nil, "PART-A", "ALN", []byte(`{"delta":-3}`)), "stale drop")
	// Negative remaining (after_uop < 0): discrepancy regardless of op.
	testutil.MustNoErr(t, audit.AppendBinUOP(db.DB, bin.ID, pi(1), -2,
		"bin_uop_delta", "test", nil, "PART-A", "ALN", audit.BinUOPContext{}), "negative remaining")
	// Ordinary cycle count (before == after, non-negative): NOT a discrepancy.
	testutil.MustNoErr(t, audit.AppendBinUOP(db.DB, bin.ID, pi(100), 100,
		audit.OpCycleCount, "test", nil, "PART-A", "op", audit.BinUOPContext{}), "cycle count")

	rows, err := svc.ListBinUOPDiscrepancies(100, 0)
	testutil.MustNoErr(t, err, "ListBinUOPDiscrepancies")

	got := map[string]int{}
	for _, r := range rows {
		if r.BinID == bin.ID {
			got[r.Op]++
		}
	}
	if got[audit.OpStaleEpochDropped] != 1 {
		t.Errorf("stale_epoch_dropped = %d, want 1", got[audit.OpStaleEpochDropped])
	}
	if got["bin_uop_delta"] != 1 {
		t.Errorf("negative bin_uop_delta = %d, want 1", got["bin_uop_delta"])
	}
	if got[audit.OpReleasedEmpty] != 1 {
		t.Errorf("released_empty = %d, want 1 (lossy included, clean excluded)", got[audit.OpReleasedEmpty])
	}
	if got[audit.OpCycleCount] != 0 {
		t.Errorf("cycle_count = %d, want 0 (not a discrepancy)", got[audit.OpCycleCount])
	}
}
