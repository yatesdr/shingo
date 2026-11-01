package kanban

import (
	"reflect"
	"testing"

	"shingo/protocol"
)

// allStates is every carrier state protocol.CarrierStateOf can answer.
var allStates = []protocol.CarrierState{
	protocol.CarrierLoaded, protocol.CarrierBare, protocol.CarrierTopsAndBottoms,
	protocol.CarrierLeftover, protocol.CarrierUnverified, protocol.CarrierEmpty,
}

// TestLine_GapForIsShortForEveryCarrierState is the test that earns the package: the
// divert's gap for a cart and the keeper's Short are one number, whatever stands on the
// cart, on a line built from the keeper's own populations. The two carts that read a
// line of their own are named, and their answers pinned, in the next test.
func TestLine_GapForIsShortForEveryCarrierState(t *testing.T) {
	t.Parallel()
	for _, have := range []Have{
		{},
		{Resident: 1},
		{Resident: 1, Asked: 1},
		{Resident: 1, Asked: 1, Coming: 1},
		{Resident: 3, Asked: 2, Coming: 4}, // past the want: Short goes negative, as the keeper reads it
	} {
		const want = 5
		short := have.Short(want)
		for _, state := range allStates {
			knockdown := LineOf("KD", want, have)
			if got := knockdown.GapFor(Cart{State: state}); got != short {
				t.Errorf("%+v, a %s knockdown: GapFor = %d, Short = %d", have, state, got, short)
			}
			if state == protocol.CarrierTopsAndBottoms {
				continue // its own row: TestLine_GapForReadsAToteCartsOwnLine
			}
			// A tote type with no stack heights wanted, its row or none: Short, too.
			tote := LineOf("TOTE", want, have)
			tote.TotesPerLayer = 8
			tote.Heights = []HeightLine{{Layers: 0, Want: 9, Have: 7}}
			if got := tote.GapFor(Cart{State: state, Tote: true, Layers: 2}); got != short {
				t.Errorf("%+v, a %s tote cart: GapFor = %d, Short = %d", have, state, got, short)
			}
			// Stack heights wanted: only an empty cart has a height to read.
			if state == protocol.CarrierEmpty {
				continue
			}
			tote.Heights = []HeightLine{{Layers: 2, Want: 9, Have: 7}}
			if got := tote.GapFor(Cart{State: state, Tote: true, Layers: 2}); got != short {
				t.Errorf("%+v, a %s tote cart on a line wanting heights: GapFor = %d, Short = %d", have, state, got, short)
			}
		}
	}
}

// TestLine_GapForReadsAToteCartsOwnLine: the two carts GapFor does not answer with
// Short. Tops & bottoms is its own row, never capped by the type's Short; an empty
// tote cart is its own height, held under the type's Short, and 0 at a height not wanted.
func TestLine_GapForReadsAToteCartsOwnLine(t *testing.T) {
	t.Parallel()
	line := Line{Code: "TOTE", Want: 4, Have: 3, TotesPerLayer: 8, // Short = 1
		Heights: []HeightLine{{Layers: 0, Want: 3, Have: 0}, {Layers: 2, Want: 5, Have: 1}, {Layers: 3, Want: 1, Have: 1}}}
	noRow := Line{Code: "TOTE", Want: 4, Have: 3, TotesPerLayer: 8}
	empty := func(layers int) Cart { return Cart{State: protocol.CarrierEmpty, Tote: true, Layers: layers} }
	tb := Cart{State: protocol.CarrierTopsAndBottoms, Tote: true}
	for _, c := range []struct {
		name string
		line Line
		cart Cart
		want int
	}{
		{"tops & bottoms: its row, past the type's Short", line, tb, 3},
		{"tops & bottoms with no row: nothing", noRow, tb, 0},
		{"an empty at a wanted height, held under the type's Short", line, empty(2), 1},
		{"an empty at a height the group has enough of", line, empty(3), 0},
		{"an empty at a height not wanted", line, empty(5), 0},
		{"an empty where no stack height is wanted: the type's Short", noRow, empty(2), 1},
	} {
		if got := c.line.GapFor(c.cart); got != c.want {
			t.Errorf("%s: GapFor = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestRank_LargestGapFirstTiesByName: the divert's order, stable on the name.
func TestRank_LargestGapFirstTiesByName(t *testing.T) {
	t.Parallel()
	type g struct {
		name string
		gap  int
	}
	groups := []g{{"B", 1}, {"C", 3}, {"A", 1}, {"D", 2}}
	Rank(groups, func(x g) string { return x.name }, func(x g) int { return x.gap })
	if want := []g{{"C", 3}, {"D", 2}, {"A", 1}, {"B", 1}}; !reflect.DeepEqual(groups, want) {
		t.Errorf("Rank = %v, want %v", groups, want)
	}
}
