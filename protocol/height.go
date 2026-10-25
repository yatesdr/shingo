package protocol

import (
	"fmt"
	"strconv"
)

// height.go — the height an empty cart is wanted at: the value a demand states
// and the rule that reads it against a cart's state (CarrierState).

// Height is the height an empty cart is wanted at, in layers of totes. It is a
// value, comparable, so it can key a map, and its zero value says no height:
//
//   - none (the zero value): no height said — a keeper ask, a part that states
//     no tote_layers. Any stack.
//   - Layers(n): n layers. At 0, tops & bottoms or any stack.
//   - TopsAndBottomsOnly: tops & bottoms and nothing else (SEND BACK's pull,
//     and a maintained group's tops & bottoms row: its count and its cap).
//
// Tops & bottoms is nobody's ordinary empty: none and a stack never take it,
// Layers(0) and TopsAndBottomsOnly do (Admits). Which of the carts a height
// admits a finder takes first is the finder's to rank, not this value's (Core:
// bins preferredEmptyOrder and its Go twin, binsource rank).
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
// stack take every empty but tops & bottoms, Layers(0) takes any, and
// TopsAndBottomsOnly takes tops & bottoms alone. Core's SQL twin is bins
// HeightArm, held equal by store/bins TestHeightArm_AgreesWithAdmits; this
// value's own table is TestHeight_Admits.
func (h Height) Admits(s CarrierState) bool {
	switch {
	case h.form == heightTopsAndBottomsOnly:
		return s == CarrierTopsAndBottoms
	case h.form == heightLayers && h.layers == 0:
		return true
	}
	return s != CarrierTopsAndBottoms
}

// String is the want in one word, its spelling on the wire (OrderRequest.Want)
// and in both modules' order rows: "" for none, the decimal layers for
// Layers(n), "tops_bottoms" (the cart state it admits alone) for
// TopsAndBottomsOnly.
func (h Height) String() string {
	switch h.form {
	case heightLayers:
		return strconv.Itoa(h.layers)
	case heightTopsAndBottomsOnly:
		return string(CarrierTopsAndBottoms)
	}
	return ""
}

// MarshalText is String, so the want rides JSON as one string.
func (h Height) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// UnmarshalText reads String's spelling back: "" is none, "tops_bottoms" is
// TopsAndBottomsOnly, a decimal is Layers(n). Anything else is refused rather
// than read as none: a want a reader cannot spell is not "no want".
func (h *Height) UnmarshalText(b []byte) error {
	switch s := string(b); s {
	case "":
		*h = Height{}
	case string(CarrierTopsAndBottoms):
		*h = TopsAndBottomsOnly
	default:
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("a height is \"\", %q or a number of layers, not %q", CarrierTopsAndBottoms, s)
		}
		*h = Layers(n)
	}
	return nil
}
