package protocol

import "testing"

func TestToteLayers(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		name          string
		totes, perLyr *int
		want          int
		ok            bool
	}{
		{"two layers", n(16), n(8), 2, true},
		{"a known 0 is a height of 0", n(0), n(8), 0, true},
		{"a remainder is no height", n(13), n(8), 0, false},
		{"a count not known", nil, n(8), 0, false},
		{"a type that states no totes per layer", n(16), nil, 0, false},
		{"a type stating 0 per layer", n(16), n(0), 0, false},
	} {
		got, ok := ToteLayers(c.totes, c.perLyr)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: ToteLayers = %d, %v; want %d, %v", c.name, got, ok, c.want, c.ok)
		}
		row := NodeBinsRow{TotesOnCart: c.totes, TotesPerLayer: c.perLyr}
		if got, ok := row.Layers(); got != c.want || ok != c.ok {
			t.Errorf("%s: NodeBinsRow.Layers = %d, %v; want %d, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestCarrierStateOf pins the state each pair of numbers makes:
// totes_per_layer {NULL, 8} × totes_on_cart {NULL, 0, 8, 13, 16}, plus a part
// on a cart at 0 and a bare type at 0 (13 is the leftover row: a layer of 8 and
// five more). Core's TestCarrierState_GoSQLAgree runs the same rows through
// CarrierStateOf and its SQL twin and requires one answer.
func TestCarrierStateOf(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		name                       string
		part                       string
		bare                       bool
		totesOnCart, totesPerLayer *int
		want                       CarrierState
	}{
		{"no tote numbers, count unknown", "", false, nil, nil, CarrierEmpty},
		{"no tote numbers, count 0", "", false, n(0), nil, CarrierEmpty},
		{"no tote numbers, count 8", "", false, n(8), nil, CarrierEmpty},
		{"no tote numbers, count 13", "", false, n(13), nil, CarrierEmpty},
		{"no tote numbers, count 16", "", false, n(16), nil, CarrierEmpty},
		{"tote type, count unknown", "", false, nil, n(8), CarrierUnverified},
		{"tote type, count 0", "", false, n(0), n(8), CarrierTopsAndBottoms},
		{"tote type, one layer", "", false, n(8), n(8), CarrierEmpty},
		{"tote type, a layer and five", "", false, n(13), n(8), CarrierLeftover},
		{"tote type, two layers", "", false, n(16), n(8), CarrierEmpty},
		{"a part on a tote cart at 0", "PART", false, n(0), n(8), CarrierLoaded},
		{"a bare type at 0", "", true, n(0), n(8), CarrierBare},
		{"a type stating 0 per layer is no tote type", "", false, n(16), n(0), CarrierEmpty},
	} {
		if got := CarrierStateOf(c.part, c.bare, c.totesOnCart, c.totesPerLayer); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
		row := NodeBinsRow{PayloadCode: c.part, Bare: c.bare, TotesOnCart: c.totesOnCart, TotesPerLayer: c.totesPerLayer}
		if got := row.CarrierState(); got != c.want {
			t.Errorf("%s: NodeBinsRow.CarrierState = %s, want %s", c.name, got, c.want)
		}
	}
}
