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
)

// tops_and_bottoms_docker_test.go — tops & bottoms (a tote cart at a known 0,
// no part) is nobody's ordinary empty: every empty finder takes it only for a
// want of Layers(0) (first, ahead of a stack) or TopsAndBottomsOnly (and only
// it), never for no height said or a stack.

// TestEmptyFinders_TopsAndBottomsIsNobodysOrdinaryEmpty runs every empty
// finder over a group holding only tops & bottoms, then with a two-high stack
// beside it.
func TestEmptyFinders_TopsAndBottomsIsNobodysOrdinaryEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grpID, ids := toteGroup(t, db, "TB", []toteCart{{code: "TB-TOTE", perLayer: 8, totes: totes(0)}})
	tb := ids[0]
	for _, f := range everyEmptyFinder(db, "TB-TOTE", grpID) {
		for _, c := range []struct {
			name string
			want bins.Height
			got  int64
		}{
			{"no height said", bins.Height{}, 0},
			{"a stack", bins.Layers(2), 0},
			{"a want of 0", bins.Layers(0), tb},
			{"tops & bottoms only", bins.TopsAndBottomsOnly, tb},
		} {
			b, err := f.find(c.want)
			if got := foundID(t, b, err); got != c.got {
				t.Errorf("%s, only tops & bottoms in the group, %s: took %d, want %d", f.name, c.name, got, c.got)
			}
		}
	}

	stack := toteGroupAdd(t, db, grpID, "TB-TOTE", "TB-STACK", 16)
	for _, f := range everyEmptyFinder(db, "TB-TOTE", grpID) {
		for _, c := range []struct {
			name string
			want bins.Height
			got  int64
		}{
			{"no height said", bins.Height{}, stack},
			{"a stack", bins.Layers(2), stack},
			{"a want of 0: tops & bottoms first", bins.Layers(0), tb},
			{"tops & bottoms only", bins.TopsAndBottomsOnly, tb},
		} {
			b, err := f.find(c.want)
			if got := foundID(t, b, err); got != c.got {
				t.Errorf("%s, tops & bottoms beside a two-high, %s: took %d, want %d", f.name, c.name, got, c.got)
			}
		}
		_, err := db.Exec(`UPDATE bins SET locked = true WHERE id = $1`, tb)
		testutil.MustNoErr(t, err, "take tops & bottoms out")
		if b, err := f.find(bins.TopsAndBottomsOnly); foundID(t, b, err) != 0 {
			t.Errorf("%s: tops & bottoms only took the two-high once tops & bottoms was gone", f.name)
		}
		_, err = db.Exec(`UPDATE bins SET locked = false WHERE id = $1`, tb)
		testutil.MustNoErr(t, err, "put it back")
	}
}

// toteGroupAdd stands one more cart of an existing type in a new slot of the
// group, at totes.
func toteGroupAdd(t *testing.T, db interface {
	QueryRow(string, ...any) *sql.Row
}, grpID int64, code, label string, totesOnCart int) int64 {
	t.Helper()
	var slot, id int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO nodes (name, is_synthetic, enabled, parent_id) VALUES ($1, false, true, $2) RETURNING id`,
		label+"-SLOT", grpID).Scan(&slot), "slot")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart)
		SELECT id, $2, $3, 'available', $4 FROM bin_types WHERE code = $1 RETURNING id`,
		code, label, slot, totesOnCart).Scan(&id), "cart")
	return id
}

// TestHeightArm_AgreesWithAdmits holds the height arm's two spellings equal:
// for every carrier state a cart can be in (the agreement table's numbers) and
// every height a caller passes, a cart answers the SQL arm exactly when
// domain.Height.Admits takes its state.
func TestHeightArm_AgreesWithAdmits(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	var plain, tote int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('HA-PLAIN') RETURNING id`).Scan(&plain), "plain")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ('HA-TOTE', 8) RETURNING id`).Scan(&tote), "tote")
	n := func(v int) *int { return &v }
	for i, c := range []struct {
		typeID int64
		totes  *int
	}{{plain, nil}, {plain, n(0)}, {tote, nil}, {tote, n(0)}, {tote, n(8)}, {tote, n(13)}, {tote, n(16)}} {
		b := &bins.Bin{BinTypeID: c.typeID, Label: fmt.Sprintf("HA-%d", i), Status: "available", NodeID: &sd.StorageNode.ID}
		testutil.MustNoErr(t, db.CreateBin(b), "bin")
		var totesArg any
		if c.totes != nil {
			totesArg = *c.totes
		}
		_, err := db.Exec(`UPDATE bins SET totes_on_cart = $1 WHERE id = $2`, totesArg, b.ID)
		testutil.MustNoErr(t, err, "totes")
		got, err := db.GetBin(b.ID)
		testutil.MustNoErr(t, err, "read back")
		state := got.CarrierState()
		for _, h := range []bins.Height{{}, bins.Layers(3), bins.Layers(0), bins.TopsAndBottomsOnly} {
			var inSQL bool
			testutil.MustNoErr(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM bins b JOIN bin_types bt ON bt.id = b.bin_type_id
				WHERE b.id = $1`+bins.HeightArm(h)+`)`, b.ID).Scan(&inSQL), "the arm")
			if inSQL != h.Admits(state) {
				t.Errorf("a %s cart, height %+v: the SQL arm says %v, Admits says %v", state, h, inSQL, h.Admits(state))
			}
		}
		if state == protocol.CarrierTopsAndBottoms && (bins.Height{}).Admits(state) {
			t.Error("no height said admits tops & bottoms")
		}
	}
}
