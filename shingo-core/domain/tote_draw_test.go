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
// per tote 10, capacity 400. H is the outside cart's totes, k the front's. An
// outside at 0 (tops & bottoms) stays 0 and never overdraws; an overdrawn
// outside is unknown, never 0.
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
		{"over-draw: the outside is unknown, not 0", ptr(26), ptr(26), 800, nil, ptr(80), true},
		{"an outside at 0 stays 0", ptr(0), ptr(0), 135, ptr(0), ptr(14), false},
		{"an outside at 0 with the front short: no overdraw", ptr(0), ptr(0), 400, ptr(0), ptr(40), false},
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
			if got.Overdrawn && got.Outside != nil {
				t.Errorf("overdrawn with an outside of %d: the two are exclusive", *got.Outside)
			}
		})
	}
}

// TestDeriveToteDraw_NoRuleIsUnknown: with no UOP per tote, or no count to
// trust, neither cart gets a number; but an outside known at 0 stays 0.
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
		in.OutsideTotes = ptr(0)
		if got := DeriveToteDraw(in); countText(got.Outside) != "0" || got.Front != nil || got.Overdrawn {
			t.Errorf("%s, an outside at 0: outside %s front %s overdrawn %v, want 0, unknown, no overdraw",
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
				// Unknown claims nothing; a truth that overdraws is not a cart.
				if written.Outside != nil && truth.Outside != nil && *written.Outside > *truth.Outside ||
					*written.Front > *truth.Front {
					t.Fatalf("H %d, cycles %d, a real front of %d: wrote outside %s front %d, "+
						"truly outside %s front %d", h, cycles, k, countText(written.Outside), *written.Front,
						countText(truth.Outside), *truth.Front)
				}
			}
		}
	}
}
