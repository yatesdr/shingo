// Package testdb provides shared test infrastructure for shingo-edge
// integration tests. Each call to Open returns a fresh ephemeral SQLite
// database with the full migration set applied.
//
// Phase 6.4a added this package so external test packages (notably
// shingo-edge/service test files in `package service_test`) can build
// real DBs without depending on the package-private testDB variable in
// shingo-edge/www/helpers_test.go. Mirrors the pattern in
// shingo-core/internal/testdb/.
//
// Edge uses SQLite-on-disk per test (one DB per test, lifecycle bound
// to t.Cleanup) rather than core's testcontainers Postgres pattern;
// SQLite is cheap enough that there's no need for a shared container.
package testdb

import (
	"os"
	"path/filepath"
	"testing"

	"shingoedge/store"
)

// Open returns a *store.DB backed by a fresh SQLite file inside t.TempDir().
// The file is removed automatically when the test ends (t.Cleanup handles
// the directory removal). Migrations run as part of store.Open so the
// returned database has the full canonical schema.
func Open(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("testdb.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(path)
	})
	return db
}
