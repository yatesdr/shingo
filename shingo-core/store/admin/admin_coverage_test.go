//go:build docker

package admin_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/admin"
)

func TestCoverage_AdminUserExists_Empty(t *testing.T) {
	db := testdb.Open(t)
	exists, err := admin.AnyExists(db.DB)
	if err != nil { t.Fatalf("AnyExists empty: %v", err) }
	if exists { t.Error("AnyExists on empty table should be false") }
}

func TestCoverage_CreateAdminUserAndGet(t *testing.T) {
	db := testdb.Open(t)
	if err := admin.Create(db.DB, "alice", "hash-alice"); err != nil { t.Fatalf("Create: %v", err) }
	got, err := admin.Get(db.DB, "alice")
	if err != nil { t.Fatalf("Get hit: %v", err) }
	if got.Username != "alice" { t.Errorf("Username = %q, want %q", got.Username, "alice") }
	if got.PasswordHash != "hash-alice" { t.Errorf("PasswordHash = %q, want %q", got.PasswordHash, "hash-alice") }
	if got.ID == 0 { t.Error("ID should be assigned") }
	if got.CreatedAt.IsZero() { t.Error("CreatedAt should be populated") }
	exists, err := admin.AnyExists(db.DB)
	if err != nil { t.Fatalf("AnyExists after create: %v", err) }
	if !exists { t.Error("AnyExists should be true after insert") }
}

func TestCoverage_GetAdminUser_Miss(t *testing.T) {
	db := testdb.Open(t)
	_, err := admin.Get(db.DB, "nobody")
	if err == nil { t.Error("Get miss should return error, got nil") }
}

func TestCoverage_CreateAdminUser_MultipleUsers(t *testing.T) {
	db := testdb.Open(t)
	if err := admin.Create(db.DB, "alice", "h1"); err != nil { t.Fatalf("create alice: %v", err) }
	if err := admin.Create(db.DB, "bob", "h2"); err != nil { t.Fatalf("create bob: %v", err) }
	a, err := admin.Get(db.DB, "alice")
	if err != nil { t.Fatalf("get alice: %v", err) }
	b, err := admin.Get(db.DB, "bob")
	if err != nil { t.Fatalf("get bob: %v", err) }
	if a.PasswordHash != "h1" { t.Errorf("alice hash = %q, want %q", a.PasswordHash, "h1") }
	if b.PasswordHash != "h2" { t.Errorf("bob hash = %q, want %q", b.PasswordHash, "h2") }
	if a.ID == b.ID { t.Errorf("alice and bob should have distinct IDs (both = %d)", a.ID) }
}
