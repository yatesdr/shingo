//go:build docker

package audit_test

import (
	"encoding/json"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/audit"
)

// TestListBinUOPByBin_ReadsTheDetailItWasWrittenWith: the bin timeline shows a
// row's detail where the row was written with one (AppendBinUOP's §16
// enrichment: the tote declares and derives write theirs there), and an
// override's metadata as before.
func TestListBinUOPByBin_ReadsTheDetailItWasWrittenWith(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const binID = 902
	testutil.MustNoErr(t, audit.AppendBinUOP(db.DB, binID, nil, 0, audit.OpTotesDeclared, "test", nil, "", "operator",
		audit.BinUOPContext{Detail: json.RawMessage(`{"layers":0,"totes_to":0}`)}), "a row with detail")
	testutil.MustNoErr(t, audit.AppendBinUOPOverride(db.DB, binID, 10, 8, "override", "test", nil, "", "operator", "",
		[]byte(`{"kind":"count"}`)), "a row with metadata")
	rows, err := audit.ListBinUOPByBin(db.DB, binID, 10, 0)
	testutil.MustNoErr(t, err, "the timeline")
	got := map[string]string{}
	for _, r := range rows {
		if r.Metadata != nil {
			got[r.Op] = *r.Metadata
		}
	}
	if len(rows) != 2 || got[audit.OpTotesDeclared] != `{"layers": 0, "totes_to": 0}` || got["override"] != `{"kind": "count"}` {
		t.Errorf("timeline %d rows, blobs %v: want the declare's detail and the override's metadata", len(rows), got)
	}
}

// TestAppendBinUOP_ABoundaryRowCarriesAStationOnlyOnAnEdgeRollback: the
// boundary row a bump writes on the exceptions ledger names a station only on
// an edge_rollback, the one bump whose v184 row held the uid (in actor; v185
// moves it to station_uid). A lift's boundary row names none, as at v184:
// v184 had no column for it, so a station written there would not survive
// `migrate down 185` and back (the served round trip found 278 such rows),
// and nothing reads one. Red if the boundary row takes the lift's station.
func TestAppendBinUOP_ABoundaryRowCarriesAStationOnlyOnAnEdgeRollback(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const lifted, rolledBack = 903, 904
	ctx := audit.BinUOPContext{StationUID: "stn-aaaa1111"}
	testutil.MustNoErr(t, audit.AppendBinUOP(db.DB, lifted, nil, 5, audit.OpLifted, "test", nil, "", "system", ctx), "a lift")
	testutil.MustNoErr(t, audit.AppendBinUOP(db.DB, rolledBack, nil, 5, audit.OpEdgeRollback, "test", nil, "", "", ctx), "an edge rollback")
	station := func(binID int64) string {
		t.Helper()
		var s string
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT station_uid FROM bin_uop_exception WHERE kind = 'boundary' AND bin_id = $1`,
			binID).Scan(&s), "read the boundary row")
		return s
	}
	if got := station(lifted); got != "" {
		t.Errorf("the lift's boundary row names station %q, want none (v184 had no column for it)", got)
	}
	if got := station(rolledBack); got != "stn-aaaa1111" {
		t.Errorf("the edge rollback's boundary row names station %q, want the rollback's uid", got)
	}
}
