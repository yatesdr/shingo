//go:build docker

package bins_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/dispatch/binsource"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// tote_height_preference_docker_test.go — the empty finders' height ladder
// (bins.preferredEmptyOrder), on every finder: a cart wanted at a height is
// the exact one, else the shortest taller, else the tallest shorter, else one
// with no count. It ranks the stacks and filters none of them; what a want
// keeps out is tops & bottoms alone (bins.HeightArm,
// TestEmptyFinders_TopsAndBottomsIsNobodysOrdinaryEmpty).

// toteGroup builds a group with one direct slot per cart and lands an empty
// cart in each, in the order given, so the first has the lowest id. A nil count
// is a cart whose count is unknown. A code with perLayer 0 is a type that says
// no totes per layer. Returns the group id and the bin ids in the order given.
func toteGroup(t *testing.T, db *store.DB, prefix string, carts []toteCart) (int64, []int64) {
	t.Helper()
	sdb := db.DB
	grpID, err := nodes.CreateGroup(sdb, prefix+"-GRP")
	testutil.MustNoErr(t, err, "CreateGroup")
	testdb.MarkGroup(t, db, grpID)
	types := map[string]int64{}
	var ids []int64
	for i, c := range carts {
		btID, ok := types[c.code]
		if !ok {
			var perLayer any
			if c.perLayer > 0 {
				perLayer = c.perLayer
			}
			testutil.MustNoErr(t, sdb.QueryRow(
				`INSERT INTO bin_types (code, totes_per_layer) VALUES ($1, $2) RETURNING id`,
				c.code, perLayer).Scan(&btID), "bin type "+c.code)
			types[c.code] = btID
		}
		slot := &nodes.Node{Name: fmt.Sprintf("%s-SLOT-%d", prefix, i), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, nodes.Create(sdb, slot), "create slot")
		var id int64
		testutil.MustNoErr(t, sdb.QueryRow(
			`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart)
			 VALUES ($1, $2, $3, 'available', $4) RETURNING id`,
			btID, fmt.Sprintf("%s-CART-%d", prefix, i), slot.ID, c.totes).Scan(&id), "cart")
		ids = append(ids, id)
	}
	return grpID, ids
}

type toteCart struct {
	code     string
	perLayer int
	totes    *int
}

func totes(n int) *int { return &n }

// everyEmptyFinder is the six exported empty finders, each asked for the group's
// tote type (or any type) with the given want.
func everyEmptyFinder(db *store.DB, code string, grpID int64) []struct {
	name string
	find func(want bins.Height) (*bins.Bin, error)
} {
	anyone := reservations.Anyone
	return []struct {
		name string
		find func(want bins.Height) (*bins.Bin, error)
	}{
		{"typed, group-scoped", func(w bins.Height) (*bins.Bin, error) {
			return bins.FindEmptyOfTypeInGroup(db.DB, code, grpID, 0, anyone, w)
		}},
		{"type set, group-scoped", func(w bins.Height) (*bins.Bin, error) {
			return bins.FindEmptyOfTypesInGroup(db.DB, []string{code}, grpID, 0, anyone, w)
		}},
		{"untyped, group-scoped", func(w bins.Height) (*bins.Bin, error) {
			return bins.FindEmptyCompatibleInGroup(db.DB, "", grpID, 0, anyone, w)
		}},
		{"typed, plant-wide", func(w bins.Height) (*bins.Bin, error) {
			return bins.FindEmptyOfType(db.DB, code, "", 0, bins.EmptyFence{}, anyone, w)
		}},
		{"type set, plant-wide", func(w bins.Height) (*bins.Bin, error) {
			return bins.FindEmptyOfTypes(db.DB, []string{code}, "", 0, bins.EmptyFence{}, anyone, w)
		}},
		{"untyped, plant-wide", func(w bins.Height) (*bins.Bin, error) {
			return bins.FindEmptyCompatible(db.DB, "", "", 0, bins.EmptyFence{}, anyone, w)
		}},
	}
}

func foundID(t *testing.T, b *bins.Bin, err error) int64 {
	t.Helper()
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	testutil.MustNoErr(t, err, "find")
	return b.ID
}

// TestEmptyFinders_HeightLadder: with 8 totes a layer and a want of 5 layers
// (40 totes), every finder takes 40 (exact), then 48 (taller), then 26
// (shorter), then the cart with no count. The carts are landed lowest id first
// in the reverse of that ladder, so today's terms alone (bin id, every slot
// equally reachable) would take them the other way round; with no want they do.
func TestEmptyFinders_HeightLadder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	tote := func(n *int) toteCart { return toteCart{code: "TH-TOTE", perLayer: 8, totes: n} }
	grpID, ids := toteGroup(t, db, "TH", []toteCart{tote(nil), tote(totes(26)), tote(totes(48)), tote(totes(40))})
	unknown, shorter, taller, exact := ids[0], ids[1], ids[2], ids[3]
	five := bins.Layers(5)

	for _, f := range everyEmptyFinder(db, "TH-TOTE", grpID) {
		t.Run(f.name, func(t *testing.T) {
			b, err := f.find(bins.Height{})
			if got := foundID(t, b, err); got != unknown {
				t.Fatalf("no want took %d, want today's first by id (%d)", got, unknown)
			}
			for _, rung := range []struct {
				name string
				want int64
			}{{"exact", exact}, {"taller", taller}, {"shorter", shorter}, {"unknown", unknown}} {
				b, err := f.find(five)
				if got := foundID(t, b, err); got != rung.want {
					t.Fatalf("rung %s: took %d, want %d (ladder exact %d, taller %d, shorter %d, unknown %d)",
						rung.name, got, rung.want, exact, taller, shorter, unknown)
				}
				_, err = db.Exec(`UPDATE bins SET locked = true WHERE id = $1`, rung.want)
				testutil.MustNoErr(t, err, "take the "+rung.name+" cart out")
			}
			_, err = db.Exec(`UPDATE bins SET locked = false WHERE id = ANY($1)`, ids)
			testutil.MustNoErr(t, err, "put the carts back")
		})
	}
}

// TestEmptyFinders_OnlyShorterOrUnknownStillFound: the want ranks the stacks
// and refuses none. With a want of 5 layers and only a cart of 26 and one with no count,
// every finder delivers the 26; with only the unknown one left, it delivers
// that; a cart whose type says no totes per layer is found too.
func TestEmptyFinders_OnlyShorterOrUnknownStillFound(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grpID, ids := toteGroup(t, db, "TS", []toteCart{
		{code: "TS-TOTE", perLayer: 8, totes: nil},
		{code: "TS-TOTE", perLayer: 8, totes: totes(26)},
	})
	unknown, shorter := ids[0], ids[1]
	five := bins.Layers(5)
	for _, f := range everyEmptyFinder(db, "TS-TOTE", grpID) {
		t.Run(f.name, func(t *testing.T) {
			b, err := f.find(five)
			if got := foundID(t, b, err); got != shorter {
				t.Fatalf("took %d, want the shorter cart %d: a height short of the want is still delivered", got, shorter)
			}
			_, err = db.Exec(`UPDATE bins SET locked = true WHERE id = $1`, shorter)
			testutil.MustNoErr(t, err, "take the shorter cart out")
			b, err = f.find(five)
			if got := foundID(t, b, err); got != unknown {
				t.Fatalf("took %d, want the cart with no count %d", got, unknown)
			}
			_, err = db.Exec(`UPDATE bins SET locked = false WHERE id = ANY($1)`, ids)
			testutil.MustNoErr(t, err, "put the carts back")
		})
	}

	kdGrp, kd := toteGroup(t, db, "TK", []toteCart{{code: "TK-KD", totes: totes(3)}})
	b, err := bins.FindEmptyOfTypeInGroup(db.DB, "TK-KD", kdGrp, 0, reservations.Anyone, five)
	if got := foundID(t, b, err); got != kd[0] {
		t.Errorf("a type with no totes per layer: took %d, want %d", got, kd[0])
	}
}

// TestEmptyFinders_CountAndFindAgreeWithAWant: the level keeper's count and the
// typed group finder share one WHERE, and a want changes only the ORDER BY, so
// with a want a group the count calls stocked still finds a cart and one it
// calls empty finds none.
func TestEmptyFinders_CountAndFindAgreeWithAWant(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	stocked, _ := toteGroup(t, db, "TC", []toteCart{
		{code: "TC-TOTE", perLayer: 8, totes: nil},
		{code: "TC-TOTE", perLayer: 8, totes: totes(16)},
	})
	empty, err := nodes.CreateGroup(db.DB, "TC-EMPTY-GRP")
	testutil.MustNoErr(t, err, "empty group")
	testdb.MarkGroup(t, db, empty)
	for _, want := range []bins.Height{{}, bins.Layers(0), bins.Layers(5)} {
		for _, g := range []int64{stocked, empty} {
			n, err := bins.CountEmptyOfTypeInGroup(db.DB, "TC-TOTE", g, want)
			testutil.MustNoErr(t, err, "count")
			b, err := bins.FindEmptyOfTypeInGroup(db.DB, "TC-TOTE", g, 0, reservations.Anyone, want)
			found := foundID(t, b, err) != 0
			if found != (n > 0) {
				label := "none"
				if n, ok := want.Stated(); ok {
					label = fmt.Sprint(n)
				}
				t.Errorf("group %d, want %s layers: count %d but found=%v", g, label, n, found)
			}
		}
	}
}

// TestHeightLadder_ThePoolAgreesWithTheFinders is the ladder's agreement table,
// the Go/SQL twin held equal: for each want, the order the empty finders take a
// group's carts in (preferredEmptyOrder, after HeightArm) is the order a
// dedicated loader's pool takes the same carts in (binsource.Source, whose
// ranker composes the Go ladder, after Height.Admits). The carts are landed in
// an order no rung follows, ties included. Every slot is a direct child of the
// group, so the finders' accessibility terms tie and both sides fall to the bin
// id: the pool ranks no accessibility (its members are a loader's positions),
// and that is not a rung.
func TestHeightLadder_ThePoolAgreesWithTheFinders(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	tote := func(n *int) toteCart { return toteCart{code: "HL-TOTE", perLayer: 8, totes: n} }
	labels := []string{"unknown", "no-totes-per-layer", "13", "48", "24", "16a", "8", "16b", "tb", "40"}
	grpID, ids := toteGroup(t, db, "HL", []toteCart{tote(nil), {code: "HL-KD", totes: totes(3)},
		tote(totes(13)), tote(totes(48)), tote(totes(24)), tote(totes(16)), tote(totes(8)), tote(totes(16)),
		tote(totes(0)), tote(totes(40))})
	label := map[int64]string{}
	for i, id := range ids {
		label[id] = labels[i]
	}
	known := []string{"unknown", "no-totes-per-layer"}
	for _, c := range []struct {
		name  string
		want  bins.Height
		order []string
	}{
		{"no height said", bins.Height{}, []string{"unknown", "no-totes-per-layer", "13", "48", "24", "16a", "8", "16b", "40"}},
		{"tops & bottoms only", bins.TopsAndBottomsOnly, []string{"tb"}},
		{"0: tops & bottoms, then the shortest", bins.Layers(0), append([]string{"tb", "8", "13", "16a", "16b", "24", "40", "48"}, known...)},
		{"2 (16 totes): exact, taller nearest first, then shorter", bins.Layers(2), append([]string{"16a", "16b", "24", "40", "48", "13", "8"}, known...)},
		{"5 (40 totes)", bins.Layers(5), append([]string{"40", "48", "24", "16a", "16b", "13", "8"}, known...)},
		{"9 (72 totes): every cart shorter, the tallest first", bins.Layers(9), append([]string{"48", "40", "24", "16a", "16b", "13", "8"}, known...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			finders := ladderOrder(t, label, func() (*bins.Bin, error) {
				return bins.FindEmptyCompatibleInGroup(db.DB, "", grpID, 0, reservations.Anyone, c.want)
			}, func(id int64) {
				_, err := db.Exec(`UPDATE bins SET locked = true WHERE id = $1`, id)
				testutil.MustNoErr(t, err, "take the cart out")
			})
			_, err := db.Exec(`UPDATE bins SET locked = false WHERE id = ANY($1)`, ids)
			testutil.MustNoErr(t, err, "put the carts back")

			cands := make([]binsource.Cand, 0, len(ids))
			for _, id := range ids {
				b, err := db.GetBin(id)
				testutil.MustNoErr(t, err, "read the cart")
				cands = append(cands, binsource.Cand{BinID: b.ID, BinTypeID: b.BinTypeID, Payload: b.PayloadCode,
					CreatedAt: b.CreatedAt, Status: b.Status, TotesOnCart: b.TotesOnCart,
					TotesPerLayer: b.TotesPerLayer, State: b.CarrierState()})
			}
			pool := ladderOrder(t, label, func() (*bins.Bin, error) {
				best, ok := binsource.Source(cands, binsource.Want{Intent: binsource.Fill, Height: c.want})
				if !ok {
					return nil, sql.ErrNoRows
				}
				return &bins.Bin{ID: best.BinID}, nil
			}, func(id int64) {
				for i := range cands {
					if cands[i].BinID == id {
						cands[i].Locked = true
					}
				}
			})

			if !slices.Equal(finders, c.order) {
				t.Errorf("the finders took %v, want the ladder's %v", finders, c.order)
			}
			if !slices.Equal(pool, finders) {
				t.Errorf("the pool took %v, the finders %v: the Go ladder and the SQL ladder disagree", pool, finders)
			}
		})
	}
}

// ladderOrder takes carts one at a time with take, taking each out with out
// before the next, and returns their labels in the order taken.
func ladderOrder(t *testing.T, label map[int64]string, take func() (*bins.Bin, error), out func(int64)) []string {
	t.Helper()
	var order []string
	for range label {
		b, err := take()
		id := foundID(t, b, err)
		if id == 0 {
			break
		}
		order = append(order, label[id])
		out(id)
	}
	return order
}
