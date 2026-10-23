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
	testutil.MustNoErr(t, audit.AppendBinUOPOverride(db.DB, binID, 10, 8, "override", "test", nil, "", "operator",
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
