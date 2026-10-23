package domain

import "shingo/protocol"

// Height is the height an empty cart is wanted at, in layers of totes. It is a
// value, comparable, so it can key a map, and its zero value says no height:
//
//   - none (the zero value): no height said — a keeper ask, a part that states
//     no tote_layers. Any stack.
//   - Layers(n): ranked by the ladder (bins preferredEmptyOrder): the exact
//     height first, then the shortest taller, then the tallest shorter; at 0,
//     tops & bottoms first, else the shortest stack.
//   - TopsAndBottomsOnly: tops & bottoms and nothing else (SEND BACK's pull,
//     and a maintained group's tops & bottoms row: its count and its cap).
//
// Tops & bottoms is nobody's ordinary empty: none and a stack never take it,
// Layers(0) and TopsAndBottomsOnly do (Admits; in SQL, bins.HeightArm).
//
// It replaces the *int that every empty finder, the finder's want and the
// set-down read carried, where nil was none.
type Height struct {
	form   heightForm
	layers int
}

type heightForm uint8

const (
	heightNone heightForm = iota
	heightLayers
	heightTopsAndBottomsOnly
)

// TopsAndBottomsOnly is a want of tops & bottoms and nothing else.
var TopsAndBottomsOnly = Height{form: heightTopsAndBottomsOnly}

// Layers is a want of n layers.
func Layers(n int) Height { return Height{form: heightLayers, layers: n} }

// Stated is the layers a Layers want names, and false for any other want.
func (h Height) Stated() (int, bool) { return h.layers, h.form == heightLayers }

// Admits says whether an empty cart in state s answers this want: none and a
// stack take every empty but tops & bottoms, Layers(0) takes any (ranking tops
// & bottoms first), TopsAndBottomsOnly takes tops & bottoms alone. The SQL twin
// is bins.HeightArm, held equal by store/bins TestHeightArm_AgreesWithAdmits.
func (h Height) Admits(s protocol.CarrierState) bool {
	switch {
	case h.form == heightTopsAndBottomsOnly:
		return s == protocol.CarrierTopsAndBottoms
	case h.form == heightLayers && h.layers == 0:
		return true
	}
	return s != protocol.CarrierTopsAndBottoms
}
