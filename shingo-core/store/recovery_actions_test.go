//go:build docker

package store

import "testing"

func TestRecordRecoveryAction_AndList(t *testing.T) {
	db := testDB(t)

	// Single record + read back
	if err := db.RecordRecoveryAction("unstuck_order", "order", 42, "manual unblock", "alice"); err != nil {
		t.Fatalf("RecordRecoveryAction: %v", err)
	}
	got, err := db.ListRecoveryActions(10)
	if err != nil {
		t.Fatalf("ListRecoveryActions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Action != "unstuck_order" {
		t.Errorf("Action = %q, want unstuck_order", got[0].Action)
	}
	if got[0].TargetType != "order" {
		t.Errorf("TargetType = %q, want order", got[0].TargetType)
	}
	if got[0].TargetID != 42 {
		t.Errorf("TargetID = %d, want 42", got[0].TargetID)
	}
	if got[0].Detail != "manual unblock" {
		t.Errorf("Detail = %q, want manual unblock", got[0].Detail)
	}
	if got[0].Actor != "alice" {
		t.Errorf("Actor = %q, want alice", got[0].Actor)
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("CreatedAt should be populated by DB default")
	}
}

func TestListRecoveryActions_OrderAndLimit(t *testing.T) {
	db := testDB(t)

	// Insert in order; ListRecoveryActions returns by id DESC, so the newest
	// (largest id) comes first.
	db.RecordRecoveryAction("a1", "order", 1, "first", "sys")
	db.RecordRecoveryAction("a2", "order", 2, "second", "sys")
	db.RecordRecoveryAction("a3", "order", 3, "third", "sys")
	db.RecordRecoveryAction("a4", "order", 4, "fourth", "sys")

	// Ordering: newest-first
	all, err := db.ListRecoveryActions(10)
	if err != nil {
		t.Fatalf("ListRecoveryActions(10): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("all len = %d, want 4", len(all))
	}
	if all[0].Action != "a4" {
		t.Errorf("newest.Action = %q, want a4", all[0].Action)
	}
	if all[3].Action != "a1" {
		t.Errorf("oldest.Action = %q, want a1", all[3].Action)
	}

	// Limit honored
	limited, err := db.ListRecoveryActions(2)
	if err != nil {
		t.Fatalf("ListRecoveryActions(2): %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limited len = %d, want 2", len(limited))
	}
	if limited[0].Action != "a4" {
		t.Errorf("limited[0].Action = %q, want a4 (newest)", limited[0].Action)
	}
	if limited[1].Action != "a3" {
		t.Errorf("limited[1].Action = %q, want a3", limited[1].Action)
	}
}
