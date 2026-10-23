//go:build docker

package bins_test

import (
	"database/sql"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/internal/helpers"
)

// TestCarrierState_GoSQLAgree is the carrier state's agreement table, the
// TestSourceableStatus_GoSQLAgree precedent: every pair of numbers a cart can
// carry — totes_per_layer {NULL, 8} × totes_on_cart {NULL, 0, 8, 13, 16}, plus
// a part on a cart at 0 and a bare type at 0 (13 is the leftover row) — stored
// as a real bin, judged by helpers.CarrierStateSQL over the row and by
// protocol.CarrierStateOf over the same numbers read back through the bin
// (domain.Bin.CarrierState), and the two must be one answer and the expected
// one. A drift between the Go and the SQL is a finder, a count or a drain that
// disagrees with the walk and the card about what a cart is.
func TestCarrierState_GoSQLAgree(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	var plain, tote int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('CS-PLAIN') RETURNING id`).Scan(&plain), "plain type")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ('CS-TOTE', 8) RETURNING id`).Scan(&tote), "tote type")
	tx, err := db.Begin()
	testutil.MustNoErr(t, err, "begin")
	marker, err := bins.EnsureBareMarkerTx(tx, tote)
	testutil.MustNoErr(t, err, "the tote's bare marker")
	testutil.MustNoErr(t, tx.Commit(), "commit")

	n := func(v int) *int { return &v }
	for i, c := range []struct {
		name        string
		typeID      int64
		part        string
		totesOnCart *int
		want        protocol.CarrierState
	}{
		{"no tote numbers, count unknown", plain, "", nil, protocol.CarrierEmpty},
		{"no tote numbers, count 0", plain, "", n(0), protocol.CarrierEmpty},
		{"no tote numbers, count 8", plain, "", n(8), protocol.CarrierEmpty},
		{"no tote numbers, count 13", plain, "", n(13), protocol.CarrierEmpty},
		{"no tote numbers, count 16", plain, "", n(16), protocol.CarrierEmpty},
		{"tote type, count unknown", tote, "", nil, protocol.CarrierUnverified},
		{"tote type, count 0", tote, "", n(0), protocol.CarrierTopsAndBottoms},
		{"tote type, one layer", tote, "", n(8), protocol.CarrierEmpty},
		{"tote type, a layer and five", tote, "", n(13), protocol.CarrierLeftover},
		{"tote type, two layers", tote, "", n(16), protocol.CarrierEmpty},
		{"a part on a tote cart at 0", tote, sd.Payload.Code, n(0), protocol.CarrierLoaded},
		{"a bare type at 0", marker, "", n(0), protocol.CarrierBare},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := &bins.Bin{BinTypeID: c.typeID, Label: fmt.Sprintf("CS-%d", i), Status: "available", NodeID: &sd.StorageNode.ID}
			testutil.MustNoErr(t, db.CreateBin(b), "bin")
			if c.part != "" {
				testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, c.part, 10), "part")
			}
			var totes sql.NullInt64
			if c.totesOnCart != nil {
				totes = sql.NullInt64{Int64: int64(*c.totesOnCart), Valid: true}
			}
			_, err := db.Exec(`UPDATE bins SET totes_on_cart = $1 WHERE id = $2`, totes, b.ID)
			testutil.MustNoErr(t, err, "totes")

			var fromSQL protocol.CarrierState
			testutil.MustNoErr(t, db.QueryRow(`SELECT `+helpers.CarrierStateSQL("b", "bt")+`
				FROM bins b JOIN bin_types bt ON bt.id = b.bin_type_id WHERE b.id = $1`, b.ID).Scan(&fromSQL), "the SQL")
			got, err := db.GetBin(b.ID)
			testutil.MustNoErr(t, err, "read back")
			fromGo := got.CarrierState()
			if fromSQL != c.want || fromGo != c.want {
				t.Errorf("SQL %s, Go %s, want %s: the two spellings of one state have drifted", fromSQL, fromGo, c.want)
			}
		})
	}
}
