package kanban

import (
	"shingo/protocol"
	"shingocore/domain"
)

// line.go — one level type of a group as its line reads it. Its readers are an
// unloader's group line, the surplus arm's count of its carts and the lift divert's
// gap (dispatch.carrierGap), so the card and the divert agree by construction.

// Line is one level type of a group: what the group wants of the type, what it has
// (resident, asked and coming: want less Have.Short), and its wanted heights when
// the type states its totes per layer.
type Line struct {
	Code          string
	Want, Have    int
	TotesPerLayer int // 0: the type states none, and has no heights
	Heights       []HeightLine
}

// HeightLine is one wanted height of a type: how many carts standing layers high the
// group wants, and how many it has (resident and coming at exactly that many totes).
type HeightLine struct {
	Layers     int
	Want, Have int
}

// LineOf is a type's line with no heights yet, from its want and its populations.
func LineOf(code string, want int, have Have) Line {
	return Line{Code: code, Want: want, Have: want - have.Short(want)}
}

// Short is how many more of the type the group wants: want less have. At or below
// zero the group is not short of it.
func (l Line) Short() int { return l.Want - l.Have }

// HeightShort is how many more carts standing layers high the group wants: the wanted
// height's want less have, held under the type's Short (a cart at a height is a cart
// of the type), but for the tops & bottoms row (0 layers), which is a line of its own.
// 0 for a height the group does not want. Its reader is GapFor, which takes a tote
// cart into a group only as its own height.
func (l Line) HeightShort(layers int) int {
	for _, h := range l.Heights {
		if h.Layers != layers {
			continue
		}
		if layers == 0 {
			return h.Want - h.Have
		}
		return min(h.Want-h.Have, l.Short())
	}
	return 0
}

// Cart is what a line asks of the cart it answers for.
type Cart struct {
	State protocol.CarrierState
	// Tote says the cart's type states its totes per layer.
	Tote bool
	// Layers is the cart's height, read only for an empty tote cart, which is at a
	// whole number of layers above 0 by protocol.CarrierStateOf.
	Layers int
}

// CartOf is the bin as its line reads it.
func CartOf(b *domain.Bin) Cart {
	layers, _ := b.Layers()
	return Cart{State: b.CarrierState(), Tote: protocol.IsToteType(b.TotesPerLayer), Layers: layers}
}

// GapFor is how short the line is of this cart, which is Short for every carrier
// state but two, each a line of its own: a tote cart at 0 reads the tops & bottoms
// row, and an empty tote cart reads its own height when the group wants stack heights
// of the type. Pin: TestLine_GapForIsShortForEveryCarrierState.
func (l Line) GapFor(c Cart) int {
	switch {
	case !c.Tote:
		return l.Short()
	case c.State == protocol.CarrierTopsAndBottoms:
		return l.HeightShort(0)
	case c.State == protocol.CarrierEmpty && l.wantsStacks():
		return l.HeightShort(c.Layers)
	}
	return l.Short()
}

// wantsStacks reports whether the line wants any height above tops & bottoms.
func (l Line) wantsStacks() bool {
	for _, h := range l.Heights {
		if h.Layers > 0 {
			return true
		}
	}
	return false
}
