package kanban

import (
	"encoding/json"
	"os"
	"testing"

	"shingo/protocol"
)

// kanban_pins_family_test.go — the one truth table of "may this cart be asked for that
// type". The same JSON drives the walk here, the arm (engine), the save (service),
// simcalc and the Edge's CLEAR panels (kanban_pins_push_as.test.js), so no reader can
// keep a rule of its own.

type familyTable struct {
	Types map[string]struct {
		Family    string     `json:"family"`
		Footprint *[]float64 `json:"footprint"`
	} `json:"types"`
	Cases []struct {
		Cart  string `json:"cart"`
		Asked string `json:"asked"`
		Same  bool   `json:"same"`
	} `json:"cases"`
}

func readFamilyTable(t *testing.T) familyTable {
	t.Helper()
	raw, err := os.ReadFile("testdata/same_family.json")
	if err != nil {
		t.Fatal(err)
	}
	var out familyTable
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// pinWalkFamilies is what the walk compares: each type's family, as an id.
func pinWalkFamilies(table familyTable) map[string]int64 {
	ids := map[string]int64{}
	out := map[string]int64{}
	for code, ty := range table.Types {
		if ids[ty.Family] == 0 {
			ids[ty.Family] = int64(len(ids) + 1)
		}
		out[code] = ids[ty.Family]
	}
	return out
}

// TestKanbanPin_TheWalkAsksWithinTheFamily: a cart whose own line is whole, one other
// type short. The walk lights it exactly where the table says the two are one family.
func TestKanbanPin_TheWalkAsksWithinTheFamily(t *testing.T) {
	table := readFamilyTable(t)
	families := pinWalkFamilies(table)
	for _, c := range table.Cases {
		lines := []Line{LineOf(c.Asked, 1, Have{})}
		got := Assign(lines, []protocol.NodeBinsRow{{Node: "W", BinID: 1, Occupied: true, BinTypeCode: c.Cart}}, nil, families)
		if lit := len(got) == 1 && got[0].BinType == c.Asked; lit != c.Same {
			t.Errorf("a %s asked for %s: lit %v, the table says %v", c.Cart, c.Asked, lit, c.Same)
		}
	}
}
