package plantspec

import (
	"testing"
)

// kanban_press5_test.go — plants/kanban-press5.yaml, the Kanban rig at five presses on one
// shared unloader. SHAPE, not tuning: what its runs need the file to have.

func loadKanbanPress5(t *testing.T) *Plant {
	t.Helper()
	p, err := Load("../../plants/kanban-press5.yaml")
	if err != nil {
		t.Fatalf("load plants/kanban-press5.yaml: %v", err)
	}
	return p
}

func TestKanbanPress5Plant_ValidAndCleanAtBirth(t *testing.T) {
	t.Parallel()
	p := loadKanbanPress5(t)
	if err := p.Validate(); err != nil {
		t.Fatalf("plants/kanban-press5.yaml invalid: %v", err)
	}
	if c := p.CensusAtBirth(); !c.Clean() {
		t.Errorf("census: %v", c.Findings())
	}
	if c := p.CarrierPairsAtBirth(); !c.Clean() {
		t.Errorf("carrier pairs: %v", c.Findings())
	}
}

// One shared unloader means one kanban: every window sends to it, it supports all five presses,
// and its pools are the summed minimum with a slot for every cart.
func TestKanbanPress5Plant_OneKanbanOverFivePresses(t *testing.T) {
	t.Parallel()
	p := loadKanbanPress5(t)
	if len(p.Kanbans) != 1 {
		t.Fatalf("kanbans = %d, want one over all five", len(p.Kanbans))
	}
	k := p.Kanbans[0]
	if len(k.Supports) != 5 || k.Pools["45x48"] != 26 || k.Pools["CART-32"] != 22 || len(k.Sources) != 0 {
		t.Errorf("the kanban = %+v, want supports the five presses, pools 26 and 22, no sources", k)
	}
	for _, l := range k.Levels {
		if l.Want != 2 {
			t.Errorf("level of %s = %d, want the mix of 2", l.BinType, l.Want)
		}
	}
	positions := map[string]bool{}
	windows := map[string]bool{}
	for _, c := range p.Claims {
		if c.WindowOf != "" {
			windows[c.CoreNode] = true
			if c.OutboundDestination != k.Group {
				t.Errorf("window %s sends to %q, want the kanban", c.CoreNode, c.OutboundDestination)
			}
			continue
		}
		for _, n := range c.Positions() {
			positions[n] = true
		}
	}
	if len(positions) != 20 {
		t.Errorf("%d positions, want 20", len(positions))
	}
	if len(windows) == 0 {
		t.Error("no windows on the shared unloader")
	}
	room := map[string]int{}
	for _, z := range p.Zones {
		if z.Name != k.Group {
			continue
		}
		for _, s := range z.Positions {
			for _, code := range s.Admits {
				room[code]++
			}
		}
	}
	if room["45x48"] < k.Pools["45x48"] || room["CART-32"] < k.Pools["CART-32"] {
		t.Errorf("room = %v, want at least the pool per family", room)
	}
}
