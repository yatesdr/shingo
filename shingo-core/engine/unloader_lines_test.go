package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store"
)

// unloader_lines_test.go — the walk that tells each cart at an unloader's
// windows what to go back as (assignWindows), on lines built by hand. The
// counts that feed it are the keeper's own and are pinned against a database
// in unloader_lines_docker_test.go.

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

// toteCart is a cart of TOTE-1, 8 totes to a layer: a tote cart by its own
// row, whatever the group wants of the type. No number is an unverified cart.
func toteCart(node string, full bool, totes ...int) protocol.NodeBinsRow {
	r := cart(node, "TOTE-1", full, totes...)
	perLayer := 8
	r.TotesPerLayer = &perLayer
	return r
}

// said renders assignments as "node=TYPE" or "node=TYPE·n", in the answer's
// order.
func said(as []protocol.NodeBinsAssignment) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		s := a.Node + "=" + a.BinType
		if a.Layers != nil {
			s += fmt.Sprintf("·%d", *a.Layers)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func requireWalk(t *testing.T, lines []store.TypeLine, windows []protocol.NodeBinsRow, want string) {
	t.Helper()
	if got := said(assignWindows(lines, windows, nil, nil)); got != want {
		t.Errorf("walk = %q, want %q", got, want)
	}
}

// The reference case: the group at 0 of 3 totes and 2 of 3 KD; a full tote
// cart, a full KD cart and an empty KD cart at the windows. The empty KD is
// walked first and sent as it is, taking KD to 3 of 3; both fulls are then
// asked for totes.
func TestAssignWindows_ReferenceCase(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "KD-1", Want: 3, Have: 2},
		{Code: "TOTE-1", Want: 3, Have: 0},
	}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		cart("W1", "TOTE-1", true),
		cart("W2", "KD-1", true),
		cart("W3", "KD-1", false),
	}, "W1=TOTE-1 W2=TOTE-1 W3=KD-1")
}

// A cart goes back as its own type while the group is short of it, even when
// another type is shorter; once the group has its own type, as the shortest.
func TestAssignWindows_OwnFirstThenLargestShortfall(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "KD-1", Want: 1, Have: 0},
		{Code: "TOTE-1", Want: 5, Have: 0},
	}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		cart("W1", "KD-1", true),
		cart("W2", "KD-1", true),
	}, "W1=KD-1 W2=TOTE-1")
}

// Equal shortfalls go to the first level in the levels' order.
func TestAssignWindows_TiesByTheLevelsOrder(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "B-TYPE", Want: 2, Have: 0},
		{Code: "A-TYPE", Want: 2, Have: 0},
	}
	requireWalk(t, lines, []protocol.NodeBinsRow{cart("W1", "OTHER-1", true)}, "W1=B-TYPE")
}

// Empties are walked before fulls, whatever the windows' order: the empty at
// the last window takes the one KD the group is short of, and the full KD
// before it is asked for something else.
func TestAssignWindows_EmptiesFirst(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "KD-1", Want: 1, Have: 0},
		{Code: "TOTE-1", Want: 1, Have: 0},
	}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		cart("W1", "KD-1", true),
		cart("W2", "KD-1", false),
	}, "W1=TOTE-1 W2=KD-1")
}

// A group short of nothing assigns nothing, and an empty window is not walked.
func TestAssignWindows_NothingShortNothingAssigned(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "KD-1", Want: 2, Have: 2},
		{Code: "TOTE-1", Want: 1, Have: 3},
	}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		cart("W1", "KD-1", true),
		{Node: "W2"},
		cart("W3", "TOTE-1", false),
	}, "")
	if as := assignWindows(lines, nil, nil, nil); as == nil || len(as) != 0 {
		t.Errorf("no windows: %v, want an empty list", as)
	}
}

// A type with layer wants: an empty counts at a height only on an exact
// multiple of its type's totes per layer. 24 totes at 8 a layer is 3 high; 26
// is a remainder and no number is unknown, and both count to the type alone.
func TestAssignWindows_ExactMultipleHeights(t *testing.T) {
	t.Parallel()
	tote := func() []store.TypeLine {
		return []store.TypeLine{{Code: "TOTE-1", Want: 6, Have: 0, TotesPerLayer: 8,
			Heights: []store.HeightLine{{Layers: 3, Want: 1}, {Layers: 5, Want: 1}}}}
	}
	// The 3-high cart is sent as 3 high (its own height); the 40-tote cart as
	// 5 high; the remainder and the unknown as the type at any height.
	requireWalk(t, tote(), []protocol.NodeBinsRow{
		toteCart("W1", false, 24),
		toteCart("W2", false, 40),
		toteCart("W3", false, 26),
		toteCart("W4", false),
	}, "W1=TOTE-1·3 W2=TOTE-1·5 W3=TOTE-1 W4=TOTE-1")

	// A second 3-high cart: its height is met, so it is asked for the largest
	// shortfall. The type is short 5; 5 high speaks for 1 of it, any height
	// for the other 4, and any height is asked.
	requireWalk(t, tote(), []protocol.NodeBinsRow{
		toteCart("W1", false, 24),
		toteCart("W2", false, 24),
	}, "W1=TOTE-1·3 W2=TOTE-1")
}

// A remainder cart and an unknown cart are not counted at a height: with every
// height met and the type short, both go back as the type.
func TestAssignWindows_RemainderAndUnknownCountToTheType(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{{Code: "TOTE-1", Want: 3, Have: 1, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 3, Want: 1, Have: 1}}}}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		toteCart("W1", false, 26),
		toteCart("W2", false),
		toteCart("W3", false, 24),
	}, "W1=TOTE-1 W2=TOTE-1")

	// 26 totes is 3 layers and 2 over: with 3 high still short, the cart is
	// sent as the type it is, not as 3 high.
	short := []store.TypeLine{{Code: "TOTE-1", Want: 2, Have: 0, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 3, Want: 1}}}}
	requireWalk(t, short, []protocol.NodeBinsRow{toteCart("W1", false, 26)}, "W1=TOTE-1")
}

// A cart with no number, or with a remainder, has no height of its own, so
// its own thing is the type at any height: what the wanted heights do not
// speak for. When the heights speak for the whole shortfall, it is asked for
// the largest one instead of going back as the bare type.
func TestAssignWindows_NoHeightOfItsOwnTakesTheLargestHeight(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{{Code: "TOTE-1", Want: 3, Have: 0, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 5, Want: 2}, {Layers: 3, Want: 1}}}}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		toteCart("W1", false),
		toteCart("W2", false, 26),
	}, "W1=TOTE-1·5 W2=TOTE-1·5")
}

// A height is short only while its type is: with the type met, a wanted height
// still under its own want assigns nothing.
func TestAssignWindows_AHeightIsHeldUnderItsType(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{{Code: "TOTE-1", Want: 2, Have: 2, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 3, Want: 2, Have: 0}}}}
	requireWalk(t, lines, []protocol.NodeBinsRow{toteCart("W1", true, 24)}, "")
}

// Heights tie ahead of their type at any height, and the walk's own
// assignments move the running shortfall: a KD cart dressed as a tote stack.
func TestAssignWindows_HeightsTieAheadOfAnyHeight(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "KD-1", Want: 1, Have: 1},
		{Code: "TOTE-1", Want: 2, Have: 0, TotesPerLayer: 8,
			Heights: []store.HeightLine{{Layers: 3, Want: 1}}},
	}
	// TOTE-1 is short 2: 3 high speaks for 1, any height for the other. The
	// first KD ties 3 high against any height and takes 3 high; the second
	// takes the any-height one; the third finds nothing short.
	requireWalk(t, lines, []protocol.NodeBinsRow{
		cart("W1", "KD-1", true),
		cart("W2", "KD-1", true),
		cart("W3", "KD-1", true),
	}, "W1=TOTE-1·3 W2=TOTE-1")
}

// TOPS & BOTTOMS IS A LINE OF ITS OWN. TOTE-1 keeps 2 and has 2 (the type is
// met), and its tops & bottoms row wants 1: a cart at 0 goes back as tops &
// bottoms (0 ships as &0), uncapped by the met type; the second cart at 0 finds
// the row met and nothing else short.
func TestAssignWindows_TopsAndBottomsIsItsOwnLine(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{{Code: "TOTE-1", Want: 2, Have: 2, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 0, Want: 1}, {Layers: 3, Want: 1}}}}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		toteCart("W1", false, 0),
		toteCart("W2", false, 0),
	}, "W1=TOTE-1·0")
}

// The row takes nothing from the type: TOTE-1 short 2 with no stack heights and
// a row short 1. Two unverified carts walked first find the type short 2 and
// both go back as the type (the row's shortfall is not the type's); the cart
// at 0 after them still takes the row.
func TestAssignWindows_TheRowTakesNothingFromTheType(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{{Code: "TOTE-1", Want: 2, Have: 0, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 0, Want: 1}}}}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		toteCart("W1", false),
		toteCart("W2", false),
		toteCart("W3", false, 0),
	}, "W1=TOTE-1 W2=TOTE-1 W3=TOTE-1·0")
}

// A cart at 0 has no own thing without a row, and is never its type at any
// height by its own right: with no row and the type short, it is asked for the
// largest shortfall like any cart without one (here the type), and with the
// row met and a stack height short, for that height.
func TestAssignWindows_ACartAtZeroWithNoRowHasNoOwnThing(t *testing.T) {
	t.Parallel()
	noRow := []store.TypeLine{
		{Code: "KD-1", Want: 3, Have: 0},
		{Code: "TOTE-1", Want: 1, Have: 0, TotesPerLayer: 8},
	}
	requireWalk(t, noRow, []protocol.NodeBinsRow{toteCart("W1", false, 0)}, "W1=KD-1")
	rowMet := []store.TypeLine{{Code: "TOTE-1", Want: 3, Have: 1, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 0, Want: 1, Have: 1}, {Layers: 3, Want: 2}}}}
	requireWalk(t, rowMet, []protocol.NodeBinsRow{toteCart("W1", false, 0)}, "W1=TOTE-1·3")
}

// A full tote cart has no own thing (the stack leaves with the parts): a full
// cart at 3 high, with 3 high and the type at any height each short 1 and KD-1
// short 3, takes the largest shortfall, KD-1, not its height and not its type.
// A full knockdown keeps its type.
func TestAssignWindows_AFullToteCartTakesTheLargest(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "KD-1", Want: 3, Have: 0},
		{Code: "TOTE-1", Want: 2, Have: 0, TotesPerLayer: 8,
			Heights: []store.HeightLine{{Layers: 3, Want: 1}}},
	}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		toteCart("W1", true, 24),
		cart("W2", "KD-1", true),
	}, "W1=KD-1 W2=KD-1")
}

// Two windows at one wanted height each take it, and each decrements it: the
// third cart at that height finds it met.
func TestAssignWindows_TwoWindowsAtOneHeight(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{{Code: "TOTE-1", Want: 2, Have: 0, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 3, Want: 2}}}}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		toteCart("W1", false, 24),
		toteCart("W2", false, 24),
		toteCart("W3", false, 24),
	}, "W1=TOTE-1·3 W2=TOTE-1·3")
}

// A bare cart at a window is nobody's empty and is assigned nothing, though
// every line is short.
func TestAssignWindows_ABareCartIsAssignedNothing(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{{Code: "TOTE-1", Want: 2, Have: 0}}
	bare := cart("W1", "TOTE-1", false)
	bare.Bare = true
	requireWalk(t, lines, []protocol.NodeBinsRow{bare}, "")
}

// sizes is the floor sizes the walk's tests give their types: three 45 x 48
// types (a knockdown, a tote cart and a basket, the tote entered the other way
// round) and a 36 x 36 knockdown; NODIM has none entered.
func sizes() map[string]domain.FloorSize {
	out := map[string]domain.FloorSize{}
	for code, wl := range map[string][2]float64{"KD-45": {45, 48}, "TOTE-1": {48, 45}, "WB-45": {45, 48}, "KD-36": {36, 36}} {
		fs, _ := domain.BinType{WidthIn: wl[0], LengthIn: wl[1]}.FloorSize()
		out[code] = fs
	}
	return out
}

func requireSizedWalk(t *testing.T, lines []store.TypeLine, windows []protocol.NodeBinsRow, want string) {
	t.Helper()
	if got := said(assignWindows(lines, windows, nil, sizes())); got != want {
		t.Errorf("walk = %q, want %q", got, want)
	}
}

// THE CART'S FLOOR SIZE BOUNDS THE ASK. A 45 x 48 knockdown goes back as its
// own type while the group is short of it; then as the largest shortfall among
// 45 x 48 types (the tote cart, entered 48 x 45), never as the 36 x 36 the
// group is shortest of, and never as a type with no size.
func TestAssignWindows_ACartIsAskedWithinItsFloorSize(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "KD-36", Want: 9, Have: 0},
		{Code: "NODIM", Want: 8, Have: 0},
		{Code: "KD-45", Want: 1, Have: 0},
		{Code: "TOTE-1", Want: 2, Have: 0},
		{Code: "WB-45", Want: 1, Have: 0},
	}
	requireSizedWalk(t, lines, []protocol.NodeBinsRow{
		cart("W1", "KD-45", true),
		cart("W2", "KD-45", true),
		cart("W3", "KD-45", true),
		cart("W4", "KD-45", true),
		cart("W5", "KD-45", true),
	}, "W1=KD-45 W2=TOTE-1 W3=TOTE-1 W4=WB-45")

	// The reference case with sizes: the full tote cart (48 x 45) is asked for
	// totes, its own size; the full 36 x 36 knockdown is never asked for totes
	// and, its own type met, is asked for nothing.
	ref := []store.TypeLine{
		{Code: "KD-36", Want: 3, Have: 2},
		{Code: "TOTE-1", Want: 3, Have: 0},
	}
	requireSizedWalk(t, ref, []protocol.NodeBinsRow{
		cart("W1", "TOTE-1", true),
		cart("W2", "KD-36", true),
		cart("W3", "KD-36", false),
	}, "W1=TOTE-1 W3=KD-36")
}

// A cart whose type has no size is asked as before: the largest shortfall of
// any type, sized or not.
func TestAssignWindows_ACartWithNoSizeAsToday(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{
		{Code: "KD-36", Want: 9, Have: 0},
		{Code: "NODIM", Want: 1, Have: 1},
	}
	requireSizedWalk(t, lines, []protocol.NodeBinsRow{cart("W1", "NODIM", true)}, "W1=KD-36")
}
