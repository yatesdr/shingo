package main

import (
	"testing"

	"shingocore/plantspec"
)

func boolp(b bool) *bool { return &b }

// fixturePlant: one press making PANEL-LH, two welds consuming it (WELD-1, WELD-3),
// each also producing ASSY-X — and WELD-3's ASSY-X output is an A/B pair. The A/B
// pair is the trap: the parked side (active_pull=false) must NOT add a second
// produce rate, or ASSY-X reads 18/min instead of 12.
func fixturePlant() *plantspec.Plant {
	return &plantspec.Plant{
		Payloads: []plantspec.Payload{
			{Code: "PANEL-LH", UOPCapacity: 30},
			{Code: "ASSY-X", UOPCapacity: 20},
		},
		Processes: []plantspec.Process{
			{Name: "PRESS-1", ActiveStyle: "P1"},
			{Name: "WELD-1", ActiveStyle: "W1"},
			{Name: "WELD-3", ActiveStyle: "W3"},
		},
		Styles: []plantspec.Style{
			{Name: "P1", Process: "PRESS-1", Payload: "PANEL-LH"},
			{Name: "W1", Process: "WELD-1", Payload: "ASSY-X"},
			{Name: "W3", Process: "WELD-3", Payload: "ASSY-X"},
		},
		Claims: []plantspec.Claim{
			{CoreNode: "PLN1", Style: "P1", Role: "produce", Payload: "PANEL-LH"},
			{CoreNode: "ALN1", Style: "W1", Role: "consume", Payload: "PANEL-LH"},
			{CoreNode: "ALN2", Style: "W1", Role: "produce", Payload: "ASSY-X"},
			{CoreNode: "ALN6", Style: "W3", Role: "consume", Payload: "PANEL-LH"},
			// A/B produce pair: ALN7 active (default), ALN8 parked.
			{CoreNode: "ALN7", Style: "W3", Role: "produce", Payload: "ASSY-X", PairedCoreNode: "ALN8"},
			{CoreNode: "ALN8", Style: "W3", Role: "produce", Payload: "ASSY-X", PairedCoreNode: "ALN7", ActivePull: boolp(false)},
		},
	}
}

func TestWalkClaims_RatesAndABCountedOnce(t *testing.T) {
	rate := map[string]float64{"PRESS-1": 12, "WELD-1": 6, "WELD-3": 6}
	flows := walkClaims(fixturePlant(), rate, nil)

	// PANEL-LH: 1 press @12 vs 2 welds @6 each = balanced.
	if got := flows["PANEL-LH"].produce; got != 12 {
		t.Errorf("PANEL-LH produce = %v, want 12", got)
	}
	if got := flows["PANEL-LH"].consume; got != 12 {
		t.Errorf("PANEL-LH consume = %v, want 12 (WELD-1 + WELD-3)", got)
	}
	// ASSY-X: WELD-1 produce 6 + WELD-3 A/B produce counted ONCE (6), not twice (12).
	if got := flows["ASSY-X"].produce; got != 12 {
		t.Errorf("ASSY-X produce = %v, want 12 (A/B parked side must not double-count)", got)
	}
}

func TestSolveDerivesPressFromLineRate(t *testing.T) {
	rate := solveRates(fixturePlant(), 6.0)
	// PRESS-1 feeds two 6/min consumers → must be derived to 12/min.
	if got := rate["PRESS-1"]; got != 12 {
		t.Errorf("solved PRESS-1 = %v/min, want 12 (2 consumers × 6)", got)
	}
	// The welds stay anchored at the line rate.
	if got := rate["WELD-1"]; got != 6 {
		t.Errorf("solved WELD-1 = %v/min, want 6 (anchor)", got)
	}
}

func TestVerdicts(t *testing.T) {
	cases := []struct {
		name string
		f    flow
		want string
	}{
		{"balanced", flow{produce: 12, consume: 12, bufferUOP: 120}, "BALANCED"},
		{"starve", flow{produce: 6, consume: 12, bufferUOP: 90}, "STARVE"},
		{"overfill", flow{produce: 12, consume: 6}, "OVERFILL"},
		{"no-source", flow{consume: 6}, "NO-SOURCE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, _, _ := c.f.verdict(0, 0); got != c.want {
				t.Errorf("verdict = %q, want %q", got, c.want)
			}
		})
	}
}
