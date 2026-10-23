package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
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
	if got := said(assignWindows(lines, windows, nil)); got != want {
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
	if as := assignWindows(lines, nil, nil); as == nil || len(as) != 0 {
		t.Errorf("no windows: %v, want an empty list", as)
	}
}

// A type with layer wants: a cart counts at a height only on an exact multiple
// of its type's totes per layer. 24 totes at 8 a layer is 3 high; 26 is a
// remainder and no number is unknown, and both count to the type alone.
func TestAssignWindows_ExactMultipleHeights(t *testing.T) {
	t.Parallel()
	tote := func() []store.TypeLine {
		return []store.TypeLine{{Code: "TOTE-1", Want: 6, Have: 0, TotesPerLayer: 8,
			Heights: []store.HeightLine{{Layers: 3, Want: 1}, {Layers: 5, Want: 1}}}}
	}
	// The 3-high cart is sent as 3 high (its own height); the 40-tote cart as
	// 5 high; the remainder and the unknown as the type at any height.
	requireWalk(t, tote(), []protocol.NodeBinsRow{
		cart("W1", "TOTE-1", true, 24),
		cart("W2", "TOTE-1", true, 40),
		cart("W3", "TOTE-1", true, 26),
		cart("W4", "TOTE-1", true),
	}, "W1=TOTE-1·3 W2=TOTE-1·5 W3=TOTE-1 W4=TOTE-1")

	// A second 3-high cart: its height is met, so it is asked for the largest
	// shortfall. The type is short 5; 5 high speaks for 1 of it, any height
	// for the other 4, and any height is asked.
	requireWalk(t, tote(), []protocol.NodeBinsRow{
		cart("W1", "TOTE-1", true, 24),
		cart("W2", "TOTE-1", true, 24),
	}, "W1=TOTE-1·3 W2=TOTE-1")
}

// A remainder cart and an unknown cart are not counted at a height: with every
// height met and the type short, both go back as the type.
func TestAssignWindows_RemainderAndUnknownCountToTheType(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{{Code: "TOTE-1", Want: 3, Have: 1, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 3, Want: 1, Have: 1}}}}
	requireWalk(t, lines, []protocol.NodeBinsRow{
		cart("W1", "TOTE-1", false, 26),
		cart("W2", "TOTE-1", false),
		cart("W3", "TOTE-1", false, 24),
	}, "W1=TOTE-1 W2=TOTE-1")

	// 26 totes is 3 layers and 2 over: with 3 high still short, the cart is
	// sent as the type it is, not as 3 high.
	short := []store.TypeLine{{Code: "TOTE-1", Want: 2, Have: 0, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 3, Want: 1}}}}
	requireWalk(t, short, []protocol.NodeBinsRow{cart("W1", "TOTE-1", false, 26)}, "W1=TOTE-1")
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
		cart("W1", "TOTE-1", false),
		cart("W2", "TOTE-1", false, 26),
	}, "W1=TOTE-1·5 W2=TOTE-1·5")
}

// A height is short only while its type is: with the type met, a wanted height
// still under its own want assigns nothing.
func TestAssignWindows_AHeightIsHeldUnderItsType(t *testing.T) {
	t.Parallel()
	lines := []store.TypeLine{{Code: "TOTE-1", Want: 2, Have: 2, TotesPerLayer: 8,
		Heights: []store.HeightLine{{Layers: 3, Want: 2, Have: 0}}}}
	requireWalk(t, lines, []protocol.NodeBinsRow{cart("W1", "TOTE-1", true, 24)}, "")
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
