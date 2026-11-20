package service

import (
	"errors"
	"testing"

	"shingocore/store/payloads"
)

// Pure (no Postgres): the standard-pack rule every template load reads. No
// declaration is the pack; a declared 0 is an empty bin; a declared count above
// the pack is kept; a declared negative is refused (owner, 2026-11-19).
func TestStandardPackUOP(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		name     string
		declared *int
		want     int
	}{
		{"undeclared is the pack", nil, 800},
		{"0 is empty", n(0), 0},
		{"a partial is kept", n(404), 404},
		{"above the pack is kept", n(803), 803},
	} {
		got, err := standardPackUOP(800, c.declared)
		if err != nil || got != c.want {
			t.Errorf("%s: standardPackUOP(800) = %d, %v; want %d", c.name, got, err, c.want)
		}
	}
	if got, err := standardPackUOP(800, n(-5)); !errors.Is(err, ErrNegativeCount) {
		t.Errorf("a declared -5 = %d, %v; want ErrNegativeCount", got, err)
	}
}

// Pure: the payloads above a ratio of 1, by code. A part alone at 8 is 8; a
// part at 1, a kit (several lines, its ratio 1) and a payload with no line are
// absent, so a ratio-1 plant's pages carry an empty map.
func TestUOPRatiosOf(t *testing.T) {
	t.Parallel()
	ps := []*payloads.Payload{{ID: 1, Code: "P-8"}, {ID: 2, Code: "P-1"}, {ID: 3, Code: "KIT"}, {ID: 4, Code: "BARE"}}
	lines := map[int64][]*payloads.ManifestItem{
		1: {{PartNumber: "P-8", PartsPerCycle: 8}},
		2: {{PartNumber: "P-1", PartsPerCycle: 1}},
		3: {{PartNumber: "K-A", PartsPerCycle: 1}, {PartNumber: "K-C", PartsPerCycle: 2}},
	}
	got := uopRatiosOf(ps, lines)
	if len(got) != 1 || got["P-8"] != 8 {
		t.Errorf("uopRatiosOf = %v, want only P-8 at 8", got)
	}
}
