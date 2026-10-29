//go:build docker

package service

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
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
//
// totes_on_cart joined the set with the tote numbers: an emptied carrier's
// count of totes is unknown (NULL) until a person declares it, so the fixture
// stands 3 totes on the cart and every one of the three must blank them. They
// share one SET fragment (emptiedCarrierSET in bin_manifest.go).

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
		lifted_at='2020-01-07T00:00:00Z', updated_at='2020-01-08T00:00:00Z', totes_on_cart=3,
		held_since='2020-01-09T00:00:00Z'
		WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "move the bin's columns off their defaults")
	return bin
}

var emptiedColumns = []string{
	"delta_epoch", "held_since", "loaded_at", "manifest", "manifest_confirmed", "payload_code",
	"totes_on_cart", "undeclared_carrier_at", "uop_remaining", "updated_at",
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

// TestEmptyingStatements_AFullBuiltOnTopsAndBottomsReadsUnknownOnceEmptied: tops
// & bottoms is a cart at 0 totes, and a full built on it carries the totes it
// was built with;
// what stands on it once it is emptied nobody has said, so it reads unknown,
// not 0. Through each of the three emptying statements.
func TestEmptyingStatements_AFullBuiltOnTopsAndBottomsReadsUnknownOnceEmptied(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		empty func(t *testing.T, db *store.DB, svc *BinManifestService, sd *testdb.StandardData, binID int64)
	}{
		{"the clear", func(t *testing.T, _ *store.DB, svc *BinManifestService, _ *testdb.StandardData, binID int64) {
			_, err := svc.ClearForReuse(binID, nil, protocol.DeclaredByLifecycle)
			testutil.MustNoErr(t, err, "ClearForReuse")
		}},
		{"the claim that empties", func(t *testing.T, db *store.DB, svc *BinManifestService, sd *testdb.StandardData, binID int64) {
			order := createTestOrder(t, db, sd.LineNode.ID)
			testdb.ReserveBin(t, db, order.ID, binID)
			testutil.MustNoErr(t, svc.ClearAndClaim(binID, order.ID), "ClearAndClaim")
		}},
		{"a release with nothing left", func(t *testing.T, db *store.DB, svc *BinManifestService, sd *testdb.StandardData, binID int64) {
			order := createTestOrder(t, db, sd.LineNode.ID)
			claimBinForTest(t, db, binID, order.ID)
			zero := 0
			testutil.MustNoErr(t, svc.SyncOrClearForReleased(binID, order.ID, &zero, "", ""), "release empty")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			svc := NewBinManifestService(db, EpochAnnounce{})
			bin := createTestBin(t, db, sd.StorageNode.ID, "BASE-"+strings.ReplaceAll(tc.name, " ", "-"), "", 0)
			_, err := db.Exec(`UPDATE bins SET totes_on_cart=0 WHERE id=$1`, bin.ID)
			testutil.MustNoErr(t, err, "stand the cart as tops & bottoms")
			_, err = svc.SetForProduction(bin.ID, `{"items":[]}`, "PART-A", 100, protocol.DeclaredByLifecycle)
			testutil.MustNoErr(t, err, "build a full on tops & bottoms")
			if got := binRowJSON(t, db, bin.ID)["totes_on_cart"]; got != float64(0) {
				t.Fatalf("the full built on tops & bottoms reads totes_on_cart %v, want 0 (the load leaves the count as it stands)", got)
			}

			tc.empty(t, db, svc, sd, bin.ID)
			if got, ok := binRowJSON(t, db, bin.ID)["totes_on_cart"]; !ok || got != nil {
				t.Errorf("once emptied the cart reads totes_on_cart %v, want unknown (null)", got)
			}
		})
	}
}
