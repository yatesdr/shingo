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
// The seats counted are every consume claim of each style a process can change
// over to, the most any one of its styles holds of the payload, both halves of an
// A/B pair included (both hold a carrier at once). The seed's active style alone
// is not enough: PART-X is claimed only by L2-B, so it counted no seats and its
// threshold of 20 passed while L2-B's three cells held 60, and its loader was
// asked only once the cells were nearly dry. A threshold of 0 is informational
// (no loader push) and is skipped.
func TestEdge2Fixture_ThresholdCoversSeatStock(t *testing.T) {
	t.Parallel()

	p, err := plantspec.Load(filepath.Join("..", "..", "..", "plants", "edge2.yaml"))
	if err != nil {
		t.Fatalf("load plants/edge2.yaml: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate plants/edge2.yaml: %v", err)
	}

	processOf := map[string]string{}
	for _, s := range p.Styles {
		processOf[s.Name] = s.Process
	}
	type styleSeat struct{ style, payload string }
	byStyle := map[styleSeat]int64{}
	for _, c := range p.Claims {
		if c.Role == "consume" {
			byStyle[styleSeat{c.Style, c.Payload}] += c.UOPCapacity
		}
	}
	type processSeat struct{ process, payload string }
	most := map[processSeat]int64{}
	for k, n := range byStyle {
		ps := processSeat{processOf[k.style], k.payload}
		most[ps] = max(most[ps], n)
	}
	seatStock := map[string]int64{}
	for k, n := range most {
		seatStock[k.payload] += n
	}

	for _, d := range p.Demands {
		if d.ReplenishUOPThreshold == nil || *d.ReplenishUOPThreshold == 0 {
			continue
		}
		th := int64(*d.ReplenishUOPThreshold)
		if th <= seatStock[d.Payload] {
			t.Errorf("demand %s at %s: threshold %d is not above the %d its cells hold under the style claiming most of it; "+
				"the cells alone meet it and the loader never refires",
				d.Payload, d.Node, th, seatStock[d.Payload])
		}
	}
}

// TestEdge2Fixture_LineTwoCarriesItsOwnCarrierType: line 2's loop is closed only
// if no other plant on the same Core draws its carriers. Core's empties keeper
// sources plant-wide by carrier type (by design), so while both plants named
// their carrier STANDARD, line 1's press-empties keeper took line 2's empty
// (TRACE-line2-empties §3). RED while
// edge2.yaml names STANDARD.
func TestEdge2Fixture_LineTwoCarriesItsOwnCarrierType(t *testing.T) {
	t.Parallel()
	load := func(name string) *plantspec.Plant {
		t.Helper()
		p, err := plantspec.Load(filepath.Join("..", "..", "..", "plants", name))
		if err != nil {
			t.Fatalf("load plants/%s: %v", name, err)
		}
		return p
	}
	demo := map[string]bool{}
	for _, bt := range load("demo-sim.yaml").BinTypes {
		demo[bt] = true
	}
	for _, bt := range load("edge2.yaml").BinTypes {
		if demo[bt] {
			t.Errorf("edge2.yaml's carrier type %q is also demo-sim.yaml's: on one Core the two lines' carriers are one pool", bt)
		}
	}
}

// TestEdge2Fixture_ProduceClaimsAskAhead: a produce claim with no reorder point
// asks only at full, and the line then waits out the whole delivery leg of its
// empty with the bin full, every bin. The fixture's
// weld asks ahead: every produce claim that is not a loader sets a reorder point
// below its capacity, so the empty is staged before the bin is full
// (RULINGS-D2-run2-and-the-pair §2.6). RED while WLD_010's claims set none.
func TestEdge2Fixture_ProduceClaimsAskAhead(t *testing.T) {
	t.Parallel()
	p, err := plantspec.Load(filepath.Join("..", "..", "..", "plants", "edge2.yaml"))
	if err != nil {
		t.Fatalf("load plants/edge2.yaml: %v", err)
	}
	capacity := map[string]int64{}
	for _, pl := range p.Payloads {
		capacity[pl.Code] = pl.UOPCapacity
	}
	for _, c := range p.Claims {
		if c.Role != "produce" || c.IsLoader() {
			continue
		}
		limit := c.UOPCapacity
		if limit == 0 {
			limit = capacity[c.Payload]
		}
		if c.ReorderPoint <= 0 || c.ReorderPoint >= limit {
			t.Errorf("produce claim %s/%s: reorder point %d against capacity %d; want one below capacity, so its empty is asked for ahead",
				c.CoreNode, c.Style, c.ReorderPoint, limit)
		}
	}
}
