//go:build docker

package testdb

import "testing"

// TestSweepProbe_RunsOnAMigratedDatabase: the end-of-test sweep probe runs on a
// freshly migrated database. When it does not (a column it names was dropped),
// every sweep returns before it asks anything, after every test, and nothing
// says so: orders.bin_id left at v174 and the probe kept reading it.
func TestSweepProbe_RunsOnAMigratedDatabase(t *testing.T) {
	db := Open(t)
	if _, err := db.DB.Exec(sweepProbeSQL); err != nil {
		t.Fatalf("the sweep probe fails on a migrated database, so no end-of-test sweep runs: %v", err)
	}
}
