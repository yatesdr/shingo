//go:build docker

package service

import (
	"testing"
)

func TestAuditService_Append_PersistsRow(t *testing.T) {
	db := testDB(t)
	svc := NewAuditService(db)

	if err := svc.Append("bin", 42, "status", "available", "flagged", "tester"); err != nil {
		t.Fatalf("Append: %v", err)
	}

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
	db := testDB(t)
	svc := NewAuditService(db)

	// Two different entities, same type.
	if err := svc.Append("bin", 1, "status", "", "available", "ui"); err != nil {
		t.Fatalf("Append bin/1: %v", err)
	}
	if err := svc.Append("bin", 2, "status", "", "flagged", "ui"); err != nil {
		t.Fatalf("Append bin/2: %v", err)
	}
	// Different type, same id as first.
	if err := svc.Append("order", 1, "created", "", "", "ui"); err != nil {
		t.Fatalf("Append order/1: %v", err)
	}

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
