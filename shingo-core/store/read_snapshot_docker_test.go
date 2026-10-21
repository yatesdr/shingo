//go:build docker

package store_test

import (
	"database/sql"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestReadSnapshot_OneSnapshotReadOnly pins what the bins snapshot build leans
// on: every read inside ReadSnapshot sees the snapshot of its first statement
// (a row committed elsewhere after it is not seen by a later read in the same
// call), and the transaction refuses a write.
func TestReadSnapshot_OneSnapshotReadOnly(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	count := func(q interface {
		QueryRow(string, ...any) *sql.Row
	}) int {
		var n int
		testutil.MustNoErr(t, q.QueryRow(`SELECT COUNT(*) FROM node_types WHERE code LIKE 'RS-%'`).Scan(&n), "count")
		return n
	}
	var first, second int
	err := store.ReadSnapshot(db.DB, func(tx *sql.Tx) error {
		first = count(tx)
		if _, err := db.DB.Exec(`INSERT INTO node_types (code, name) VALUES ('RS-1', 'synthetic')`); err != nil {
			return err
		}
		second = count(tx)
		if _, err := tx.Exec(`INSERT INTO node_types (code, name) VALUES ('RS-2', 'synthetic')`); err == nil {
			t.Error("a write inside the read snapshot was accepted")
		}
		return nil
	})
	if err == nil {
		t.Error("the snapshot committed after a refused write; want the aborted transaction's error")
	}
	if first != 0 || second != 0 {
		t.Errorf("reads in one snapshot saw %d then %d; want 0 and 0 (the row committed between them is after the snapshot)", first, second)
	}
	if got := count(db.DB); got != 1 {
		t.Errorf("after the snapshot: %d rows, want the one committed outside it", got)
	}
}
