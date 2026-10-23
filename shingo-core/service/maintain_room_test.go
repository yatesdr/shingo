package service

import (
	"strings"
	"testing"

	"shingocore/store"
)

// TestRoomRefusals_TheRowCountsBesideTheLevel: a group keeping three of a type
// and a tops & bottoms row of two stands five carts, whatever heights of 1 or
// more it also wants (those are the level said by height). Past its enabled
// positions the save is refused, at them it is warned, and the sentence says
// how many of the carts are tops & bottoms.
func TestRoomRefusals_TheRowCountsBesideTheLevel(t *testing.T) {
	t.Parallel()
	cfg := MaintainedGroupConfig{Enabled: true,
		Levels: []store.MaintainLevel{{BinTypeID: 7, Want: 3}},
		LayerWants: []store.MaintainLayerWant{
			{BinTypeID: 7, Layers: 0, Want: 2},
			{BinTypeID: 7, Layers: 2, Want: 3},
		}}
	for _, c := range []struct {
		positions          int
		refusals, warnings int
	}{{4, 1, 0}, {5, 0, 1}, {6, 0, 0}} {
		refusals, warnings := roomRefusals(cfg, "TB-GRP", c.positions)
		if len(refusals) != c.refusals || len(warnings) != c.warnings {
			t.Errorf("%d positions: refusals %v, warnings %v; want %d and %d", c.positions, refusals, warnings,
				c.refusals, c.warnings)
		}
		for _, m := range append(refusals, warnings...) {
			if !strings.Contains(m, "5 carriers (2 of them tops & bottoms)") {
				t.Errorf("%d positions: %q does not count the row beside the level", c.positions, m)
			}
		}
	}
}
