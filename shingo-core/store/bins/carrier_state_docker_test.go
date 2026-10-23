//go:build docker

package bins_test

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/internal/helpers"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
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

// TestDrainableInGroup_EachBoxPullsItsStateOnly runs the agreement table's rows
// through an unloader's drain population (bins.ListDrainableInGroup, W14): each
// row a cart on its own slot of one group, and each of the four boxes ticked
// alone must list exactly the rows of its state — the leftover box the 13 row
// and nothing else — while all four list every row but the loaded cart and the
// bare marker, which no box ever drains (the marker is stage 2's).
//
// MUTATION: drop the state arm, or compare with <> ALL, and every one-box row
// lists the wrong carts. The marker is left out twice, by the empty finders'
// `AND NOT bt.bare` and by its state (bare, which no box ticks), so dropping
// either alone keeps the four-box row green.
func TestDrainableInGroup_EachBoxPullsItsStateOnly(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	var plain, tote int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('DRN-PLAIN') RETURNING id`).Scan(&plain), "plain type")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ('DRN-TOTE', 8) RETURNING id`).Scan(&tote), "tote type")
	tx, err := db.Begin()
	testutil.MustNoErr(t, err, "begin")
	marker, err := bins.EnsureBareMarkerTx(tx, tote)
	testutil.MustNoErr(t, err, "the tote's bare marker")
	testutil.MustNoErr(t, tx.Commit(), "commit")
	grp, err := db.CreateNodeGroup("DRN-PRESS-OUT")
	testutil.MustNoErr(t, err, "the source group")
	testdb.MarkGroup(t, db, grp)

	n := func(v int) *int { return &v }
	byState := map[protocol.CarrierState][]int64{}
	for i, c := range []struct {
		typeID      int64
		part        string
		totesOnCart *int
		state       protocol.CarrierState
	}{
		{plain, "", nil, protocol.CarrierEmpty},
		{plain, "", n(0), protocol.CarrierEmpty},
		{plain, "", n(8), protocol.CarrierEmpty},
		{plain, "", n(13), protocol.CarrierEmpty},
		{plain, "", n(16), protocol.CarrierEmpty},
		{tote, "", nil, protocol.CarrierUnverified},
		{tote, "", n(0), protocol.CarrierTopsAndBottoms},
		{tote, "", n(8), protocol.CarrierEmpty},
		{tote, "", n(13), protocol.CarrierLeftover},
		{tote, "", n(16), protocol.CarrierEmpty},
		{tote, sd.Payload.Code, n(0), protocol.CarrierLoaded},
		{marker, "", n(0), protocol.CarrierBare},
	} {
		slot := &nodes.Node{Name: fmt.Sprintf("DRN-PRESS-OUT-%02d", i), Enabled: true, ParentID: &grp}
		testutil.MustNoErr(t, db.CreateNode(slot), "slot")
		b := &bins.Bin{BinTypeID: c.typeID, Label: fmt.Sprintf("DRN-%02d", i), Status: "available", NodeID: &slot.ID}
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
		byState[c.state] = append(byState[c.state], b.ID)
	}

	listed := func(ticked ...protocol.CarrierState) []int64 {
		t.Helper()
		got, err := db.ListDrainableCarts(grp, loaders.DrainStates(ticked).Ticked())
		testutil.MustNoErr(t, err, "the drain population")
		ids := make([]int64, len(got))
		for i, b := range got {
			ids[i] = b.ID
		}
		slices.Sort(ids)
		return ids
	}
	for _, s := range loaders.Drainable {
		want := slices.Clone(byState[s])
		slices.Sort(want)
		if got := listed(s); !slices.Equal(got, want) {
			t.Errorf("the %s box alone lists %v, want %v: a box pulls its state and only its state", s, got, want)
		}
	}
	var every []int64
	for _, s := range loaders.Drainable {
		every = append(every, byState[s]...)
	}
	slices.Sort(every)
	if got := listed(loaders.Drainable[:]...); !slices.Equal(got, every) {
		t.Errorf("all four boxes list %v, want %v: every part-less cart but the bare marker, never the loaded one", got, every)
	}
	if got := listed(); len(got) != 0 {
		t.Errorf("no box ticked lists %v, want nothing", got)
	}
}
