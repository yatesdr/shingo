// Package testdb is shingo-edge's shared test harness: a migrated SQLite
// database per test, and the seeders more than one package builds the same
// way. Mirrors shingo-core/internal/testdb.
//
// Each Open is a COPY of a template migrated once per test binary, not a run
// of the migration chain. A migration-chain store.Open is ~120 statements on a
// one-connection SQLite; copying the migrated file is one sequential write.
// The template is built by the real store.Open — so the migrations and the
// post-migration schema check run for real, once — and every copy is opened
// with store.OpenMigrated, which runs the schema check again. Tests that are
// ABOUT the migration chain call store.Open themselves.
//
// The template is kept as bytes, not as a file: there is no end-of-binary hook
// for a library package, so a template file would outlive every run.
//
// shingo-edge/store's own in-package tests cannot import this package (it
// imports store), so store keeps an in-package copy of the pool
// (store/store_test.go, coverageDB).
package testdb

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"shingoedge/store"
)

var (
	templateOnce  sync.Once
	templateBytes []byte
	templateErr   error
)

// buildTemplate migrates a database through store.Open, closes it and keeps
// its file. SQLite in WAL mode checkpoints into the main file when the last
// connection closes, so the one file is the whole database.
func buildTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "shingo-edge-testdb-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "template.db")
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Path writes a fresh copy of the migrated template into t.TempDir() and
// returns its path, unopened: for the tests that open, close and reopen one
// file. Open and OpenCounting open one of these.
func Path(t testing.TB) string {
	t.Helper()
	templateOnce.Do(func() { templateBytes, templateErr = buildTemplate() })
	if templateErr != nil {
		t.Fatalf("testdb: build template: %v", templateErr)
	}
	path := filepath.Join(t.TempDir(), "test.db")
	if err := os.WriteFile(path, templateBytes, 0o600); err != nil {
		t.Fatalf("testdb: copy template: %v", err)
	}
	return path
}

// Open returns a *store.DB on a fresh copy of the migrated template, in
// t.TempDir(), closed when the test ends.
func Open(t testing.TB) *store.DB {
	t.Helper()
	db, err := store.OpenMigrated(Path(t))
	if err != nil {
		t.Fatalf("testdb.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// OpenCounting is Open with every statement counted, for the tests that pin a
// path to a number of queries. store.OpenCounting opens as store.Open does, so
// it runs the migration chain over the copy (a no-op on a migrated file); the
// caller resets the counter before the statements it counts.
func OpenCounting(t testing.TB) (*store.DB, *store.QueryCounter) {
	t.Helper()
	db, counter, err := store.OpenCounting(Path(t))
	if err != nil {
		t.Fatalf("testdb.OpenCounting: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, counter
}
