//go:build docker

package engine

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// unloader_lines_docker_test.go — an unloader's group line read from a real
// database: the keeper's counts feed the walk, a cart counts at a height only
// on an exact multiple, and a group with no levels costs nothing.

// linesFixture is a flat group with six positions, two carrier types (TOTE-L
// at 8 totes a layer, KD-L with none) and an engine whose database counts its
// statements.
type linesFixture struct {
	eng     *Engine
	db      *store.DB
	counter *store.QueryCounter
	grp     *nodes.Node
	kd      int64
	tote    int64
	slots   []int64
}

func newLinesFixture(t *testing.T, name string) linesFixture {
	t.Helper()
	db, cfg := testdb.OpenWithConfig(t)
	grpID, err := nodes.CreateGroup(db.DB, name)
	testutil.MustNoErr(t, err, "CreateGroup")
	testdb.MarkGroup(t, db, grpID)
	f := linesFixture{db: db}
	for i := 0; i < 6; i++ {
		n := &nodes.Node{Name: name + "-P" + string(rune('A'+i)), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, db.CreateNode(n), "create position")
		f.slots = append(f.slots, n.ID)
	}
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('KD-L') RETURNING id`).Scan(&f.kd), "KD type")
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code, totes_per_layer) VALUES ('TOTE-L', 8) RETURNING id`).Scan(&f.tote), "tote type")
	pinFamilyOf(t, db, "LINES-F", f.kd, f.tote)
	f.grp, err = db.GetNode(grpID)
	testutil.MustNoErr(t, err, "read group")
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	f.eng, f.counter = &Engine{db: cdb}, counter
	return f
}

// empty stands an empty carrier of a type, with totes (nil: unknown), on a
// position of the group.
func (f linesFixture) empty(t *testing.T, btID int64, slot int, label string, totes *int) {
	t.Helper()
	id := landAt(t, f.db, btID, f.slots[slot], label)
	_, err := f.db.Exec(`UPDATE bins SET totes_on_cart = $1 WHERE id = $2`, totes, id)
	testutil.MustNoErr(t, err, "set totes on "+label)
}

func (f linesFixture) lines(t *testing.T, levels []store.KanbanLevel, windows []protocol.NodeBinsRow) *protocol.NodeBinsGroup {
	t.Helper()
	f.counter.Reset()
	got, err := f.eng.UnloaderGroupLines([]store.GroupLineAsk{{Group: f.grp, Levels: levels, Windows: windows}})
	testutil.MustNoErr(t, err, "UnloaderGroupLines")
	if len(got) == 0 {
		return nil
	}
	return &got[0]
}

func (f linesFixture) levels(t *testing.T, wants map[int64]int, layers map[int64][]store.KanbanLayerWant) []store.KanbanLevel {
	t.Helper()
	for bt, want := range wants {
		testutil.MustNoErr(t, f.db.SetKanbanLevelAndLayerWants(store.KanbanLevel{
			GroupNodeID: f.grp.ID, BinTypeID: bt, Want: want,
		}, layers[bt]), "declare level")
	}
	lv, err := f.db.ListKanbanLevels(f.grp.ID)
	testutil.MustNoErr(t, err, "ListKanbanLevels")
	return lv
}

func intp(n int) *int { return &n }

// cart is a window's row holding a cart of the type, full or empty, with its totes when said.
func cart(node, binType string, full bool, totes ...int) protocol.NodeBinsRow {
	r := protocol.NodeBinsRow{Node: node, Occupied: true, BinTypeCode: binType}
	if full {
		r.PayloadCode = "PART-A"
	}
	if len(totes) == 1 {
		n := totes[0]
		r.TotesOnCart = &n
	}
	return r
}

// toteL states a window's row as a cart of TOTE-L, 8 totes to a layer, as the
// bins feed does: a tote cart by its own row.
func toteL(r protocol.NodeBinsRow) protocol.NodeBinsRow {
	perLayer := 8
	r.TotesPerLayer = &perLayer
	return r
}

// The reference case on a database: 2 KD empties resident, nothing of the
// totes. The empty KD at the last window takes KD to 3 of 3; both fulls are
// asked for totes. Four statements, whatever the types: the open episodes, the
// types in play, the comings and the residents.
func TestUnloaderGroupLines_ReferenceCase(t *testing.T) {
	t.Parallel()
	f := newLinesFixture(t, "LINES-REF")
	f.empty(t, f.kd, 0, "LINES-REF-KD1", nil)
	f.empty(t, f.kd, 1, "LINES-REF-KD2", nil)
	levels := f.levels(t, map[int64]int{f.kd: 3, f.tote: 3}, nil)

	got := f.lines(t, levels, []protocol.NodeBinsRow{
		toteL(cart("W1", "TOTE-L", true)),
		cart("W2", "KD-L", true),
		cart("W3", "KD-L", false),
	})
	want := &protocol.NodeBinsGroup{
		Group: "LINES-REF",
		Wants: []protocol.NodeBinsWant{{BinType: "KD-L", Want: 3}, {BinType: "TOTE-L", Want: 3}},
		Assignments: []protocol.NodeBinsAssignment{
			{Node: "W1", BinType: "TOTE-L"}, {Node: "W2", BinType: "TOTE-L"}, {Node: "W3", BinType: "KD-L"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("line =\n  %+v\nwant\n  %+v", got, want)
	}
	if n := f.counter.Count(); n != 3 {
		t.Errorf("statements = %d, want 3 (episodes, types in play, the loops):\n%v", n, f.counter.Statements())
	}
}

// Heights from the database: residents at 24 totes (3 high at 8 a layer), 26
// (a remainder) and unknown count toward the type; only the 24 counts at 3
// high. A cart coming in at 40 (5 high) counts at 5 high. The group wants 6
// totes, 3 high x2 and 5 high x1: short 2 of the type, 1 at 3 high, none at
// 5 high. A full 5-high cart at a window has no height of its own (its stack
// leaves with the parts), so it is asked for the largest shortfall, 3 high,
// and the next for the type at any height. No more statements than the plain case:
// the residents read carries the totes, and the comings at a height are read off
// the keeper's own coming rows.
func TestUnloaderGroupLines_HeightsCountOnAnExactMultiple(t *testing.T) {
	t.Parallel()
	f := newLinesFixture(t, "LINES-H")
	f.empty(t, f.tote, 0, "LINES-H-24", intp(24))
	f.empty(t, f.tote, 1, "LINES-H-26", intp(26))
	f.empty(t, f.tote, 2, "LINES-H-NIL", nil)
	coming := landAt(t, f.db, f.tote, f.slots[5], "LINES-H-40")
	_, err := f.db.Exec(`UPDATE bins SET totes_on_cart = 40, node_id = NULL WHERE id = $1`, coming)
	testutil.MustNoErr(t, err, "put the 40-tote cart in transit")
	testdb.CreateOrder(t, f.db, func(o *orders.Order) {
		o.OrderType = "move"
		o.DeliveryNode = "LINES-H"
		o.NamedBinID = &coming
	})
	levels := f.levels(t, map[int64]int{f.tote: 6}, map[int64][]store.KanbanLayerWant{
		f.tote: {{Layers: 3, Want: 2}, {Layers: 5, Want: 1}},
	})

	got := f.lines(t, levels, []protocol.NodeBinsRow{
		toteL(cart("W1", "TOTE-L", true, 40)),
		toteL(cart("W2", "TOTE-L", true, 40)),
		toteL(cart("W3", "TOTE-L", true, 40)),
	})
	want := &protocol.NodeBinsGroup{
		Group: "LINES-H",
		Wants: []protocol.NodeBinsWant{
			{BinType: "TOTE-L", Want: 6},
			{BinType: "TOTE-L", Layers: intp(3), Want: 2},
			{BinType: "TOTE-L", Layers: intp(5), Want: 1},
		},
		Assignments: []protocol.NodeBinsAssignment{
			{Node: "W1", BinType: "TOTE-L", Layers: intp(3)}, {Node: "W2", BinType: "TOTE-L"},
			{Node: "W3", NoneShort: true},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("line =\n  %+v\nwant\n  %+v", got, want)
	}
	if n := f.counter.Count(); n != 3 {
		t.Errorf("statements = %d, want 3 (episodes, types in play, the loops):\n%v",
			n, f.counter.Statements())
	}
}

// A cart CLEARed at a window whose empty-out is live is already coming (the
// empty-out names it), so it is counted once: in the keeper's coming, and not
// again by the walk. The group wants 2 KD and 1 tote and holds none; the
// CLEARed empty KD at W1 is coming, so the full KD at W2 is the group's
// second KD. Walking W1 as well would send W2 back as a tote.
func TestUnloaderGroupLines_AClearedCartIsCountedOnce(t *testing.T) {
	t.Parallel()
	f := newLinesFixture(t, "LINES-ONCE")
	w1 := &nodes.Node{Name: "LINES-ONCE-W1", Enabled: true}
	testutil.MustNoErr(t, f.db.CreateNode(w1), "create window")
	cleared := landAt(t, f.db, f.kd, w1.ID, "LINES-ONCE-KD")
	testdb.CreateOrder(t, f.db, func(o *orders.Order) {
		o.OrderType = "move"
		o.SourceNode = w1.Name
		o.DeliveryNode = "LINES-ONCE"
		o.NamedBinID = &cleared
	})
	levels := f.levels(t, map[int64]int{f.kd: 2, f.tote: 1}, nil)

	w1Row := cart("LINES-ONCE-W1", "KD-L", false)
	w1Row.BinID = cleared
	got := f.lines(t, levels, []protocol.NodeBinsRow{w1Row, cart("LINES-ONCE-W2", "KD-L", true)})
	want := []protocol.NodeBinsAssignment{{Node: "LINES-ONCE-W2", BinType: "KD-L"}}
	if !reflect.DeepEqual(got.Assignments, want) {
		t.Errorf("assignments = %+v, want %+v", got.Assignments, want)
	}
	if n := f.counter.Count(); n != 3 {
		t.Errorf("statements = %d, want 3 (episodes, types in play, the loops):\n%v", n, f.counter.Statements())
	}
}

// A group with no levels gets no line and costs no statement.
func TestUnloaderGroupLines_NoLevelsNothing(t *testing.T) {
	t.Parallel()
	f := newLinesFixture(t, "LINES-NONE")
	f.empty(t, f.kd, 0, "LINES-NONE-KD", nil)
	if got := f.lines(t, nil, []protocol.NodeBinsRow{cart("W1", "KD-L", true)}); got != nil {
		t.Errorf("line for a group with no levels = %+v, want nil", got)
	}
	if n := f.counter.Count(); n != 0 {
		t.Errorf("statements = %d, want 0", n)
	}
}

// The line an Edge is sent reads the effective want through the feed's own read: a
// process running the type lifts the want past the base, and a cart comes back as it.
func TestUnloaderGroupLines_TheEffectiveWant(t *testing.T) {
	t.Parallel()
	f := newLinesFixture(t, "LINES-COVER")
	f.levels(t, map[int64]int{f.kd: 1}, nil)
	f.empty(t, f.kd, 0, "LINES-COVER-KD", nil)
	testdb.KanbanKeeper(t, f.db, f.grp.ID, "edge.1")
	coverPart(t, f.db, "LINES-COVER-P", f.kd)
	coverProcess(t, f.db, "LINES-COVER-PRESS", coverStyle{"S", true, [][2]string{{"LINES-COVER-SUPPORT", "LINES-COVER-P"}}})

	levels, err := f.db.ListKanbanLevels(f.grp.ID)
	testutil.MustNoErr(t, err, "levels")
	got := f.lines(t, levels, []protocol.NodeBinsRow{cart("W1", "KD-L", false)})
	want := []protocol.NodeBinsWant{{BinType: "KD-L", Want: 2}}
	if !reflect.DeepEqual(got.Wants, want) {
		t.Errorf("wants = %+v, want the effective %+v", got.Wants, want)
	}
	if assigned := []protocol.NodeBinsAssignment{{Node: "W1", BinType: "KD-L"}}; !reflect.DeepEqual(got.Assignments, assigned) {
		t.Errorf("assignments = %+v, want %+v: one resident of 3 wanted is short", got.Assignments, assigned)
	}
}

// The line's counts are the keeper's: with the keeper's own episode open and
// an ask live, the line's type is as short as the keeper's gap says.
func TestUnloaderGroupLines_CountsAreTheKeepers(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, btID := mntFixture(t, db, "LINES-KEEP", 4, "LINES-KEEP-T", 2)
	var other int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ('OTHER-T') RETURNING id`).Scan(&other), "the window cart's type")
	pinFamilyOf(t, db, "LINES-KEEP-F", btID, other)
	mntOutsideEmpty(t, db, btID, "LINES-KEEP-MARKET")
	eng.KanbanKeeper().Tick()
	eng.KanbanKeeper().Tick()
	st := mntState(t, eng.KanbanKeeper(), "LINES-KEEP", "LINES-KEEP-T")
	if st.Asked == 0 {
		t.Fatalf("the keeper has no live ask, so the asked term is not exercised: %+v", st)
	}

	grp, err := db.GetNode(grpID)
	testutil.MustNoErr(t, err, "read group")
	levels, err := db.ListKanbanLevels(grpID)
	testutil.MustNoErr(t, err, "levels")
	// One full cart of another type at a window: assigned the keeper's type
	// only while the keeper's gap says the group is short.
	got, err := eng.UnloaderGroupLines([]store.GroupLineAsk{{Group: grp, Levels: levels,
		Windows: []protocol.NodeBinsRow{cart("W1", "OTHER-T", true)}}})
	testutil.MustNoErr(t, err, "UnloaderGroupLines")
	assigned := len(got[0].Assignments) == 1 && !got[0].Assignments[0].NoneShort
	if assigned != (st.Gap > 0) {
		t.Errorf("keeper gap %d (resident %d asked %d coming %d), line assigned %v: the two disagree",
			st.Gap, st.Resident, st.Asked, st.Coming, got[0].Assignments)
	}
}

// A window cart whose type is no level of the group still reads its floor
// size, in the same one types read: a full 45 x 48 cart of a type the group
// keeps no level of is asked for the 45 x 48 knockdown the group is short of,
// never the 36 x 36 it is shortest of. Three statements, as the reference case.
func TestUnloaderGroupLines_AWindowCartReadsItsSizeThoughNoLevel(t *testing.T) {
	t.Parallel()
	f := newLinesFixture(t, "LINES-SIZE")
	var small, win int64
	testutil.MustNoErr(t, f.db.QueryRow(`INSERT INTO bin_types (code) VALUES ('SMALL-L') RETURNING id`).Scan(&small), "another family")
	testutil.MustNoErr(t, f.db.QueryRow(`INSERT INTO bin_types (code) VALUES ('WIN-L') RETURNING id`).Scan(&win), "the window cart's type")
	pinFamilyOf(t, f.db, "LINES-45", f.kd, win)
	levels := f.levels(t, map[int64]int{small: 5, f.kd: 1}, nil)

	got := f.lines(t, levels, []protocol.NodeBinsRow{cart("W1", "WIN-L", true)})
	want := []protocol.NodeBinsAssignment{{Node: "W1", BinType: "KD-L"}}
	if !reflect.DeepEqual(got.Assignments, want) {
		t.Errorf("assignments = %+v, want %+v", got.Assignments, want)
	}
	if n := f.counter.Count(); n != 3 {
		t.Errorf("statements = %d, want 3 (episodes, types in play, the loops):\n%v", n, f.counter.Statements())
	}
}
