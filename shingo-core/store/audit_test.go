//go:build docker

package store

import "testing"

func TestAuditLog(t *testing.T) {
	db := testDB(t)

	db.AppendAudit("order", 1, "created", "", "new order", "system")
	db.AppendAudit("order", 1, "dispatched", "pending", "dispatched", "system")
	db.AppendAudit("node", 2, "updated", "", "S1", "admin")

	// List all
	entries, err := db.ListAuditLog(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("len = %d, want 3", len(entries))
	}
	// Most recent first
	if entries[0].Action != "updated" {
		t.Errorf("first entry action = %q, want %q", entries[0].Action, "updated")
	}

	// List by entity
	orderEntries, _ := db.ListEntityAudit("order", 1)
	if len(orderEntries) != 2 {
		t.Errorf("order entries = %d, want 2", len(orderEntries))
	}
}
