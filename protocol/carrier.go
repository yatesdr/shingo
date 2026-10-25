package protocol

// carrier.go — what stands on a cart, as both modules read it from the same
// two numbers: the totes on the cart (bins.totes_on_cart) and its type's totes
// per layer (bin_types.totes_per_layer).

// ToteLayers is a cart's height in layers: its totes on the cart, when that is
// an exact multiple of its type's totes per layer. ok is false for a count not
// known, a type that states no totes per layer, or a remainder (a cart that
// is no whole number of layers). A known 0 is a height of 0.
//
// One spelling for every reader: the Edge's empties line and the unloader
// card's height (NodeBinsRow.Layers), Core's unloader walk and the keeper's
// heights (domain.Bin.Layers). Pinned by TestToteLayers.
func ToteLayers(totesOnCart, totesPerLayer *int) (int, bool) {
	if totesOnCart == nil || !IsToteType(totesPerLayer) || *totesOnCart%*totesPerLayer != 0 {
		return 0, false
	}
	return *totesOnCart / *totesPerLayer, true
}

// IsToteType reports whether a carrier type is a tote type: it states its
// totes per layer (bin_types.totes_per_layer). The one spelling of the fact on
// both modules; in SQL it is the null test, `totes_per_layer IS NOT NULL`
// (TestRule7_ToteTypeHasOneSpelling). A stated value is never 0 or below: the
// column's CHECK (totes_per_layer > 0) refuses it, and the Edge reads the value
// from Core's feed, so the arithmetic here and in CarrierStateOf divides by it.
func IsToteType(totesPerLayer *int) bool { return totesPerLayer != nil }

// Layers is the row's cart height (ToteLayers).
func (r NodeBinsRow) Layers() (int, bool) { return ToteLayers(r.TotesOnCart, r.TotesPerLayer) }

// CarrierState is what stands on a cart, one value: Core's finders, counts,
// pool, bind, unloader walk, drop choice and drain read it, and so do the
// Edge's board empties line and SEND BACK line (its chain readiness asks the
// row's part and Stocked instead, wrongCarrier). The values are the strings
// Core's SQL twin (store/internal/helpers CarrierStateSQL) renders, pinned equal by its
// agreement table.
type CarrierState string

const (
	// CarrierLoaded: the cart holds a part (its payload code is set).
	CarrierLoaded CarrierState = "loaded"
	// CarrierBare: the marker — a bare carrier type, nothing on it.
	CarrierBare CarrierState = "bare"
	// CarrierTopsAndBottoms: a tote type at a known 0 totes, no part — a tote's
	// top and bottom on the cart, which a tote press runs on.
	CarrierTopsAndBottoms CarrierState = "tops_bottoms"
	// CarrierLeftover: a tote type whose known count is above 0 and not a
	// whole number of layers ("3 high + 2").
	CarrierLeftover CarrierState = "leftover"
	// CarrierUnverified: a tote type whose count is not known.
	CarrierUnverified CarrierState = "unverified"
	// CarrierEmpty: every other part-less cart — a type that states no totes
	// per layer, or a tote cart at a whole number of layers above 0.
	CarrierEmpty CarrierState = "empty"
)

// CarrierStateOf is a cart's state from its part, whether its type is the bare
// marker, its totes on the cart and its type's totes per layer (nil: unknown,
// and a type that states none is not a tote type). The marker is judged first:
// it is never stock, whatever it is stamped with. An ordinary empty is empty,
// leftover or unverified, never tops & bottoms and never bare.
func CarrierStateOf(part string, bare bool, totesOnCart, totesPerLayer *int) CarrierState {
	switch {
	case bare:
		return CarrierBare
	case part != "":
		return CarrierLoaded
	case !IsToteType(totesPerLayer):
		return CarrierEmpty
	case totesOnCart == nil:
		return CarrierUnverified
	case *totesOnCart == 0:
		return CarrierTopsAndBottoms
	case *totesOnCart%*totesPerLayer != 0:
		return CarrierLeftover
	}
	return CarrierEmpty
}

// CarrierState is the row's cart state (CarrierStateOf).
func (r NodeBinsRow) CarrierState() CarrierState {
	return CarrierStateOf(r.PayloadCode, r.Bare, r.TotesOnCart, r.TotesPerLayer)
}
