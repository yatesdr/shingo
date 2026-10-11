package main

import (
	"path/filepath"
	"testing"

	"shingocore/plantspec"
)

// edge2_fixture_test.go — the edge2 plant's demand thresholds against the stock
// its own cells hold.
//
// WHY. Core's threshold monitor compares a demand's threshold with the
// plant-wide total of the payload, cells included. A threshold at or below what
// the active cells can hold between them is met by the cells alone: once the
// market is drawn down, the total freezes above the threshold as the cells stop
// counting, and the loader never refires. Edge2's PART-W did exactly that (20
// against three cells of 20: a stranded swap that nothing was coming for).
//
// It reads plants/edge2.yaml itself, not a fixture copy, for the reason
// seed_demo_mg2_test.go gives: the shipped file is the thing under test.
//
// The seats counted are every consume claim of each process's ACTIVE style, both
// halves of an A/B pair included (both hold a carrier at once). A threshold of 0
// is informational (no loader push) and is skipped.
func TestEdge2Fixture_ThresholdCoversSeatStock(t *testing.T) {
	t.Parallel()

	p, err := plantspec.Load(filepath.Join("..", "..", "..", "plants", "edge2.yaml"))
	if err != nil {
		t.Fatalf("load plants/edge2.yaml: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate plants/edge2.yaml: %v", err)
	}

	active := map[string]bool{}
	for _, proc := range p.Processes {
		active[proc.ActiveStyle] = true
	}
	seatStock := map[string]int64{}
	for _, c := range p.Claims {
		if active[c.Style] && c.Role == "consume" {
			seatStock[c.Payload] += c.UOPCapacity
		}
	}

	for _, d := range p.Demands {
		if d.ReplenishUOPThreshold == nil || *d.ReplenishUOPThreshold == 0 {
			continue
		}
		th := int64(*d.ReplenishUOPThreshold)
		if th <= seatStock[d.Payload] {
			t.Errorf("demand %s at %s: threshold %d is not above the %d its active cells hold; "+
				"the cells alone meet it and the loader never refires",
				d.Payload, d.Node, th, seatStock[d.Payload])
		}
	}
}
