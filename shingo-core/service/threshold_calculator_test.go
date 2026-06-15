package service

import "testing"

// TestCalculateThresholds_Formula pins the pure unified-formula math (shared
// verbatim with the Edge): ceil(((sum of L1/L2 leads)/cycle)*safety) and
// ceil((market_to_cell/cycle)*safety), the 1.5 default safety, and the
// zero-cycle guard.
func TestCalculateThresholds_Formula(t *testing.T) {
	t.Parallel()
	// cycle 10; L1 leads 5+5+5+5=20 → (20/10)*1.5 = 3.0 → 3.
	// market 30 → (30/10)*1.5 = 4.5 → ceil 5.
	out := CalculateThresholds(ThresholdCalculatorInputs{
		CycleSeconds: 10, L1QueueSeconds: 5, L1TransitSeconds: 5, L2LoadSeconds: 5,
		L2TransitSeconds: 5, MarketToCellSeconds: 30, SafetyFactor: 1.5,
	})
	if out.L1Threshold != 3 || out.CellReorder != 5 || out.SafetyApplied != 1.5 {
		t.Errorf("out = %+v, want L1=3 Cell=5 safety=1.5", out)
	}
	if d := CalculateThresholds(ThresholdCalculatorInputs{CycleSeconds: 10, MarketToCellSeconds: 10}); d.SafetyApplied != 1.5 {
		t.Errorf("default safety = %v, want 1.5 when factor <= 0", d.SafetyApplied)
	}
	if z := CalculateThresholds(ThresholdCalculatorInputs{L1QueueSeconds: 100}); z.L1Threshold != 0 || z.CellReorder != 0 {
		t.Errorf("zero cycle outputs = %+v, want 0/0 (no divide-by-zero)", z)
	}
}

// TestScoreConfidence pins the coverage→label ladder, including the L2-starved
// demotion (rich L1/retrieve data but thin stores must not stamp HIGH because
// L2 timings feed the L1 threshold).
func TestScoreConfidence(t *testing.T) {
	t.Parallel()
	if g := scoreConfidence(14, 20, 20, 20); g != "HIGH" {
		t.Errorf("full coverage = %q, want HIGH", g)
	}
	if g := scoreConfidence(7, 10, 10, 10); g != "MEDIUM" {
		t.Errorf("medium coverage = %q, want MEDIUM", g)
	}
	if g := scoreConfidence(14, 20, 15, 20); g != "MEDIUM" {
		t.Errorf("L2-starved (15<20) = %q, want MEDIUM not HIGH", g)
	}
	if g := scoreConfidence(3, 5, 5, 5); g != "LOW" {
		t.Errorf("thin coverage = %q, want LOW", g)
	}
}
