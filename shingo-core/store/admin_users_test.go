//go:build docker

package store

import "testing"

func TestAdminUserExists_Empty(t *testing.T) {
	db := testDB(t)

	exists, err := db.AdminUserExists()
	if err != nil {
		t.Fatalf("AdminUserExists empty: %v", err)
	}
	if exists {
		t.Error("AdminUserExists on empty table should be false")
	}
}

func TestCreateAdminUserAndGet(t *testing.T) {
	db := testDB(t)

	if err := db.CreateAdminUser("alice", "hash-alice"); err != nil {
		t.Fatalf("CreateAdminUser: %v", err)
	}

	got, err := db.GetAdminUser("alice")
	if err != nil {
		t.Fatalf("GetAdminUser hit: %v", err)
	}
	if got.Username != "alice" {
		t.Errorf("Username = %q, want %q", got.Username, "alice")
	}
	if got.PasswordHash != "hash-alice" {
		t.Errorf("PasswordHash = %q, want %q", got.PasswordHash, "hash-alice")
	}
	if got.ID == 0 {
		t.Error("ID should be assigned after insert")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be populated by DB default")
	}

	// AdminUserExists should now be true
	exists, err := db.AdminUserExists()
	if err != nil {
		t.Fatalf("AdminUserExists after create: %v", err)
	}
	if !exists {
		t.Error("AdminUserExists should be true after insert")
	}
}

func TestGetAdminUser_Miss(t *testing.T) {
	db := testDB(t)

	_, err := db.GetAdminUser("nobody")
	if err == nil {
		t.Error("GetAdminUser miss should return error, got nil")
	}
}

func TestCreateAdminUser_MultipleUsers(t *testing.T) {
	db := testDB(t)

	if err := db.CreateAdminUser("alice", "h1"); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if err := db.CreateAdminUser("bob", "h2"); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	a, err := db.GetAdminUser("alice")
	if err != nil {
		t.Fatalf("get alice: %v", err)
	}
	b, err := db.GetAdminUser("bob")
	if err != nil {
		t.Fatalf("get bob: %v", err)
	}
	if a.PasswordHash != "h1" {
		t.Errorf("alice hash = %q, want %q", a.PasswordHash, "h1")
	}
	if b.PasswordHash != "h2" {
		t.Errorf("bob hash = %q, want %q", b.PasswordHash, "h2")
	}
	if a.ID == b.ID {
		t.Errorf("alice and bob should have distinct IDs (both = %d)", a.ID)
	}
}
