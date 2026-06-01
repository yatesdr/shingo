//go:build docker

package audit_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

func TestCoverage_AuditLog(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	audit.Append(db.DB, "order", 1, "created", "", "new order", "system")
	audit.Append(db.DB, "order", 1, "dispatched", "pending", "dispatched", "system")
	audit.Append(db.DB, "node", 2, "updated", "", "S1", "admin")
	entries, err := audit.List(db.DB, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("len = %d, want 3", len(entries))
	}
	if entries[0].Action != "updated" {
		t.Errorf("first entry action = %q, want %q", entries[0].Action, "updated")
	}
	orderEntries, _ := audit.ListForEntity(db.DB, "order", 1)
	if len(orderEntries) != 2 {
		t.Errorf("order entries = %d, want 2", len(orderEntries))
	}
}
