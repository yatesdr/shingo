//go:build docker

package main

import (
	"testing"

	"shingocore/internal/testdb"
)

// TestEverySoakstatQueryRunsAgainstTheMigratedSchema runs every measure's and
// every invariant check's SQL — all of collect() — against the real migrated
// schema and fails on any query error.
//
// WHY. checkNegativeTotalUOP asked for `SUM(uop_count)`, a column bins never
// had, from the day it was written; scalar() swallowed the error and the check
// reported clean on every soak. The stub tests (scalar_test.go) prove an error
// is REPORTED; only the real schema proves there is none to report. A column
// renamed by a migration, a table dropped, a typo in a join — each would now
// surface as FAILED TO RUN on the rig, but this test surfaces it in CI first.
//
// One non-bare carrier is seeded so checkExhaustedCarrierPool's per-type
// sourceable count (the scalar inside its loop) executes too; without a pool
// that query is never sent.
//
// VERIFIED RED BY: planting the historical defect (`SUM(uop_count)` in
// checkNegativeTotalUOP) — this test fails naming it.
func TestEverySoakstatQueryRunsAgainstTheMigratedSchema(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	typeCode, nodeID := poolFixture(t, db, "EVERYQUERY")
	seedCarrier(t, db, typeCode, nodeID, typeCode+"-1", "")

	r := collect(db, "")
	for _, l := range r.failedToRun {
		t.Errorf("%s", l)
	}
	if len(r.failedToRun) > 0 {
		t.Errorf("%d soakstat quer(y/ies) failed against the migrated schema — the soak would "+
			"report these instruments as FAILED TO RUN on every run", len(r.failedToRun))
	}
}
