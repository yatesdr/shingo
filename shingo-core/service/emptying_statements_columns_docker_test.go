//go:build docker

package service

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// emptying_statements_columns_docker_test.go — the columns each of the three
// statements that empty a carrier writes, read off the row.
//
// The three: ClearForReuseTx (the CLEAR, and every door that clears for reuse),
// clearAndClaimTx (dispatch claiming a carrier it empties), and the zero arm of
// syncOrClearForReleased (a release that says nothing is left). Each is run on
// a bin whose every column has been moved off its default, and the set of
// columns that changed is compared, by name, with the set below. A column the
// statement starts to write, or stops writing, moves the set.
//
// delta_epoch is in every set: it is the epoch bump each function makes in the
// same transaction (bumpEpoch), not the UPDATE's SET list, and it is listed so
// the set is the whole of what the row sees.

// binRowJSON is the bin's row, whole.
func binRowJSON(t *testing.T, db *store.DB, id int64) map[string]any {
	t.Helper()
	var raw []byte
	testutil.MustNoErr(t, db.QueryRow(`SELECT to_jsonb(b) FROM bins b WHERE id=$1`, id).Scan(&raw), "read bin row")
	var row map[string]any
	testutil.MustNoErr(t, json.Unmarshal(raw, &row), "decode bin row")
	return row
}

// changedColumns names every column whose value differs between two rows.
func changedColumns(before, after map[string]any) []string {
	var out []string
	for k, v := range after {
		if !reflect.DeepEqual(before[k], v) {
			out = append(out, k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// loadedBinOffDefaults is a loaded bin with every nullable time set, every
// text column non-blank and updated_at in the past, so a statement that writes
// a column, even to the value the column would take by default, is seen.
func loadedBinOffDefaults(t *testing.T, db *store.DB, nodeID int64, label string) *bins.Bin {
	t.Helper()
	bin := createTestBin(t, db, nodeID, label, "PART-A", 100)
	_, err := db.Exec(`UPDATE bins SET description='off-default', loaded_at='2020-01-01T00:00:00Z',
		undeclared_carrier_at='2020-01-02T00:00:00Z', last_counted_at='2020-01-03T00:00:00Z',
		last_counted_by='counter', anomaly_at='2020-01-04T00:00:00Z', anomaly_note='note',
		staged_at='2020-01-05T00:00:00Z', staged_expires_at='2020-01-06T00:00:00Z',
		lifted_at='2020-01-07T00:00:00Z', updated_at='2020-01-08T00:00:00Z'
		WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "move the bin's columns off their defaults")
	return bin
}

var emptiedColumns = []string{
	"delta_epoch", "loaded_at", "manifest", "manifest_confirmed", "payload_code",
	"undeclared_carrier_at", "uop_remaining", "updated_at",
}

func withColumns(base []string, more ...string) []string {
	out := append(append([]string{}, base...), more...)
	sort.Strings(out)
	return out
}

func TestEmptyingStatements_ColumnLists(t *testing.T) {
	t.Parallel()

	t.Run("ClearForReuseTx, type kept", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		sd := testdb.SetupStandardData(t, db)
		svc := NewBinManifestService(db, EpochAnnounce{})
		bin := loadedBinOffDefaults(t, db, sd.StorageNode.ID, "COLS-CLEAR")
		before := binRowJSON(t, db, bin.ID)

		_, err := svc.ClearForReuse(bin.ID, nil, protocol.DeclaredByLifecycle)
		testutil.MustNoErr(t, err, "ClearForReuse")
		if got := changedColumns(before, binRowJSON(t, db, bin.ID)); !reflect.DeepEqual(got, emptiedColumns) {
			t.Errorf("columns written = %v, want %v", got, emptiedColumns)
		}
	})

	t.Run("ClearForReuseTx, re-typed", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		sd := testdb.SetupStandardData(t, db)
		svc := NewBinManifestService(db, EpochAnnounce{})
		bin := loadedBinOffDefaults(t, db, sd.StorageNode.ID, "COLS-RETYPE")
		other := &bins.BinType{Code: "COLS-KD-1"}
		testutil.MustNoErr(t, db.CreateBinType(other), "create the other type")
		before := binRowJSON(t, db, bin.ID)

		_, err := svc.ClearForReuse(bin.ID, &other.ID, protocol.DeclaredByLifecycle)
		testutil.MustNoErr(t, err, "ClearForReuse re-typed")
		want := withColumns(emptiedColumns, "bin_type_id")
		if got := changedColumns(before, binRowJSON(t, db, bin.ID)); !reflect.DeepEqual(got, want) {
			t.Errorf("columns written = %v, want %v", got, want)
		}
	})

	t.Run("clearAndClaimTx", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		sd := testdb.SetupStandardData(t, db)
		svc := NewBinManifestService(db, EpochAnnounce{})
		bin := loadedBinOffDefaults(t, db, sd.StorageNode.ID, "COLS-CLAIM")
		order := createTestOrder(t, db, sd.LineNode.ID)
		testdb.ReserveBin(t, db, order.ID, bin.ID)
		before := binRowJSON(t, db, bin.ID)

		testutil.MustNoErr(t, svc.ClearAndClaim(bin.ID, order.ID), "ClearAndClaim")
		want := withColumns(emptiedColumns, "claimed_by")
		if got := changedColumns(before, binRowJSON(t, db, bin.ID)); !reflect.DeepEqual(got, want) {
			t.Errorf("columns written = %v, want %v", got, want)
		}
	})

	t.Run("syncOrClearForReleased, nothing left", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		sd := testdb.SetupStandardData(t, db)
		svc := NewBinManifestService(db, EpochAnnounce{})
		bin := loadedBinOffDefaults(t, db, sd.StorageNode.ID, "COLS-RELEASE")
		order := createTestOrder(t, db, sd.LineNode.ID)
		claimBinForTest(t, db, bin.ID, order.ID)
		before := binRowJSON(t, db, bin.ID)

		zero := 0
		testutil.MustNoErr(t, svc.SyncOrClearForReleased(bin.ID, order.ID, &zero, "", ""), "release empty")
		if got := changedColumns(before, binRowJSON(t, db, bin.ID)); !reflect.DeepEqual(got, emptiedColumns) {
			t.Errorf("columns written = %v, want %v", got, emptiedColumns)
		}
	})
}
