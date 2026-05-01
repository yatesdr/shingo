// Package testharness exposes Edge test fixtures across module
// boundaries. Same rationale as shingocore/testharness — used by the
// integration module to construct a real Edge engine + DB without
// duplicating setup boilerplate.
//
// Production code MUST NOT import this package.
package testharness

import (
	"path/filepath"
	"testing"

	"shingoedge/orders"
	"shingoedge/store"
)

// OpenDB opens a fresh SQLite DB in a t.TempDir() with all migrations
// applied. The database is closed and removed via t.Cleanup. Mirrors
// the per-test isolation Core gets from testcontainers.
func OpenDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Re-export commonly-needed types so integration callers don't have to
// import deeper Edge packages just for type declarations.
type (
	OrderManager = orders.Manager
)
