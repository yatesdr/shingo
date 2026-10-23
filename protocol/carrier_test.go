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
