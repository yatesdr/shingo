package domain

import (
	"fmt"
	"testing"
)

func countText(n *int) string {
	if n == nil {
		return "unknown"
	}
	return fmt.Sprint(*n)
}

func ptr(n int) *int { return &n }

// TestDeriveToteDraw is the derivation's table: 8 totes a layer, 5 high, UOP
// per tote 10, capacity 400. H is the outside cart's totes, k the front's.
func TestDeriveToteDraw(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		k, h           *int
		cycles         int
		outside, front *int
		overdrawn      bool
	}{
		{"a full", ptr(0), ptr(40), 400, ptr(0), ptr(40), false},
		{"coil out", ptr(0), ptr(40), 135, ptr(26), ptr(14), false},
		{"a full on the carried cart", ptr(26), ptr(40), 400, ptr(26), ptr(40), false},
		{"a full on a carried cart of 14", ptr(14), ptr(40), 400, ptr(14), ptr(40), false},
		{"nothing built", ptr(0), ptr(40), 0, ptr(40), ptr(0), false},
		{"unknown front", nil, ptr(40), 135, ptr(26), ptr(14), false},
		{"unknown outside", ptr(0), nil, 135, nil, ptr(14), false},
		{"over-draw", ptr(26), ptr(26), 800, ptr(0), ptr(80), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveToteDraw(ToteDrawInput{Cycles: ptr(tc.cycles), UOPPerTote: ptr(10),
				FrontTotes: tc.k, OutsideTotes: tc.h})
			if countText(got.Outside) != countText(tc.outside) || countText(got.Front) != countText(tc.front) {
				t.Errorf("outside %s front %s, want outside %s front %s",
					countText(got.Outside), countText(got.Front), countText(tc.outside), countText(tc.front))
			}
			if got.Overdrawn != tc.overdrawn {
				t.Errorf("overdrawn %v, want %v", got.Overdrawn, tc.overdrawn)
			}
		})
	}
}

// TestDeriveToteDraw_NoRuleIsUnknown: with no UOP per tote, or no count to
// trust, neither cart gets a number.
func TestDeriveToteDraw_NoRuleIsUnknown(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]ToteDrawInput{
		"no UOP per tote":   {Cycles: ptr(135), FrontTotes: ptr(0), OutsideTotes: ptr(40)},
		"no count to trust": {UOPPerTote: ptr(10), FrontTotes: ptr(0), OutsideTotes: ptr(40)},
		"a count below 0":   {Cycles: ptr(-5), UOPPerTote: ptr(10), FrontTotes: ptr(0), OutsideTotes: ptr(40)},
	} {
		if got := DeriveToteDraw(in); got.Outside != nil || got.Front != nil || got.Overdrawn {
			t.Errorf("%s: outside %s front %s overdrawn %v, want both unknown",
				name, countText(got.Outside), countText(got.Front), got.Overdrawn)
		}
	}
}

// TestDeriveToteDraw_UnknownFrontNeverClaimsMore: for every k the front might
// really have carried, the counts written with k unknown are at most what the
// carts truly hold. The rule this checks is the one STOP condition the
// derivation carries: counting an unknown front as 0 must never claim a tote
// that is not there.
func TestDeriveToteDraw_UnknownFrontNeverClaimsMore(t *testing.T) {
	t.Parallel()
	for _, h := range []int{0, 14, 26, 40} {
		for cycles := 0; cycles <= 800; cycles += 5 {
			written := DeriveToteDraw(ToteDrawInput{Cycles: ptr(cycles), UOPPerTote: ptr(10), OutsideTotes: ptr(h)})
			for k := 0; k <= 40; k++ {
				truth := DeriveToteDraw(ToteDrawInput{Cycles: ptr(cycles), UOPPerTote: ptr(10),
					FrontTotes: ptr(k), OutsideTotes: ptr(h)})
				if *written.Outside > *truth.Outside || *written.Front > *truth.Front {
					t.Fatalf("H %d, cycles %d, a real front of %d: wrote outside %d front %d, "+
						"truly outside %d front %d", h, cycles, k, *written.Outside, *written.Front,
						*truth.Outside, *truth.Front)
				}
			}
		}
	}
}
