package testdb

import (
	"testing"

	"shingoedge/store"
)

// SeedProcessStyle inserts a process + style and returns their ids, for the
// tests that need a minimal parent hierarchy. shingo-edge/store keeps its own
// copy (store_test.go): its in-package tests cannot import this package.
func SeedProcessStyle(t *testing.T, db *store.DB, procName, styleName string) (int64, int64) {
	t.Helper()
	pid, err := db.CreateProcess(procName, "desc", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := db.CreateStyle(styleName, "style desc", pid)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	return pid, sid
}
