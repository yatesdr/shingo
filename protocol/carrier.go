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
	if totesOnCart == nil || totesPerLayer == nil || *totesPerLayer <= 0 || *totesOnCart%*totesPerLayer != 0 {
		return 0, false
	}
	return *totesOnCart / *totesPerLayer, true
}

// Layers is the row's cart height (ToteLayers).
func (r NodeBinsRow) Layers() (int, bool) { return ToteLayers(r.TotesOnCart, r.TotesPerLayer) }
