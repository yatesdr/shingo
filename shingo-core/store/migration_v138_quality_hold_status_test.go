//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestV138_QualityHoldStatusBecomesFlagged: v138 retires the old quality_hold
// bin STATUS value. A bin still in it becomes flagged; every other status is
// left alone; and the containment marker (the bins.quality_hold boolean, a
// different fact with the same name) is neither read nor written.
func TestV138_QualityHoldStatusBecomesFlagged(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)

	held := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "V138-OLD-HOLD")
	contained := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "V138-CONTAINED")
	maint := testdb.CreateBinAtNode(t, db, "PART-A", sd.StorageNode.ID, "V138-MAINT")
	if _, err := db.Exec(`UPDATE bins SET status='quality_hold' WHERE id=$1`, held.ID); err != nil {
		t.Fatalf("seed old status: %v", err)
	}
	if _, err := db.Exec(`UPDATE bins SET quality_hold=TRUE, hold_by='v138-test' WHERE id=$1`, contained.ID); err != nil {
		t.Fatalf("seed containment marker: %v", err)
	}
	if _, err := db.Exec(`UPDATE bins SET status='maintenance' WHERE id=$1`, maint.ID); err != nil {
		t.Fatalf("seed maintenance: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 138`); err != nil {
		t.Fatalf("clear v138 row: %v", err)
	}

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v138: %v", err)
	}
	defer migrated.Close()

	status := func(id int64) string {
		t.Helper()
		var s string
		if err := migrated.QueryRow(`SELECT status FROM bins WHERE id=$1`, id).Scan(&s); err != nil {
			t.Fatalf("read status bin %d: %v", id, err)
		}
		return s
	}
	if got := status(held.ID); got != "flagged" {
		t.Errorf("old quality_hold-status bin = %q after v138, want flagged", got)
	}
	if got := status(maint.ID); got != "maintenance" {
		t.Errorf("maintenance bin = %q after v138, want maintenance (the UPDATE must be keyed)", got)
	}
	var hold bool
	var st string
	if err := migrated.QueryRow(`SELECT quality_hold, status FROM bins WHERE id=$1`, contained.ID).Scan(&hold, &st); err != nil {
		t.Fatalf("read contained bin: %v", err)
	}
	if !hold || st != "available" {
		t.Errorf("contained bin: quality_hold=%v status=%q, want true/available — v138 must not touch the containment marker", hold, st)
	}
}
