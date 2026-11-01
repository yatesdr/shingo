package service

import (
	"strings"
	"testing"

	"shingocore/store"
)

// TestRoomWarnings_TheRowCountsBesideTheLevel: a group keeping three of a type
// and a tops & bottoms row of two stands five carts, whatever heights of 1 or
// more it also wants (those are the level said by height). Past its enabled
// positions and at them the save is warned and never refused, and the sentence
// says how many of the carts are tops & bottoms.
func TestRoomWarnings_TheRowCountsBesideTheLevel(t *testing.T) {
	t.Parallel()
	cfg := KanbanConfig{
		Levels: []store.KanbanLevel{{BinTypeID: 7, Want: 3}},
		LayerWants: []store.KanbanLayerWant{
			{BinTypeID: 7, Layers: 0, Want: 2},
			{BinTypeID: 7, Layers: 2, Want: 3},
		}}
	for _, c := range []struct {
		positions, warnings int
		says                string
	}{{4, 1, "1 cannot be covered"}, {5, 1, "nothing is left free"}, {6, 0, ""}} {
		warnings := roomWarnings(cfg, "TB-GRP", c.positions)
		if len(warnings) != c.warnings {
			t.Errorf("%d positions: warnings %v; want %d", c.positions, warnings, c.warnings)
		}
		for _, m := range warnings {
			if !strings.Contains(m, c.says) {
				t.Errorf("%d positions: %q does not say %q", c.positions, m, c.says)
			}
			if !strings.Contains(m, "5 carriers (2 of them tops & bottoms)") {
				t.Errorf("%d positions: %q does not count the row beside the level", c.positions, m)
			}
		}
	}
}
