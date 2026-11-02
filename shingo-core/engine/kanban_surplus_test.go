package engine

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"shingocore/domain"
	"shingocore/internal/lockorder"
)

// kanban_surplus_test.go — the surplus arm's decisions with no database.

func TestPlanSurplus(t *testing.T) {
	t.Parallel()
	f45, _ := domain.BinType{WidthIn: 45, LengthIn: 48}.FloorSize()
	f32, _ := domain.BinType{WidthIn: 32, LengthIn: 30}.FloorSize()
	for _, tc := range []struct {
		name   string
		shorts []surplusShort
		above  []surplusLine
		room   map[string]int
		want   surplusPlan
	}{
		{name: "the largest on the floor, not the first",
			shorts: []surplusShort{{code: "S", floor: f45, sized: true, need: 1}},
			above:  []surplusLine{{code: "A", floor: f45, sized: true, above: 1}, {code: "B", floor: f45, sized: true, above: 2}},
			want:   surplusPlan{toWindows: []string{"B"}}},
		{name: "a tie takes the lines' order",
			shorts: []surplusShort{{code: "S", floor: f45, sized: true, need: 1}},
			above:  []surplusLine{{code: "A", floor: f45, sized: true, above: 2}, {code: "B", floor: f45, sized: true, above: 2}},
			want:   surplusPlan{toWindows: []string{"A"}}},
		{name: "the need, not the surplus, bounds the moves",
			shorts: []surplusShort{{code: "S", floor: f45, sized: true, need: 2}},
			above:  []surplusLine{{code: "A", floor: f45, sized: true, above: 5}},
			want:   surplusPlan{toWindows: []string{"A", "A"}}},
		{name: "the floor first, then a source for the rest",
			shorts: []surplusShort{{code: "S", floor: f45, sized: true, need: 2}},
			above:  []surplusLine{{code: "A", floor: f45, sized: true, above: 1}, {code: "C", floor: f32, sized: true, above: 3}},
			room:   map[string]int{"C": 1},
			want:   surplusPlan{toWindows: []string{"A"}, toSources: []string{"C"}}},
		{name: "another floor with no room waits",
			shorts: []surplusShort{{code: "S", floor: f45, sized: true, need: 1}},
			above:  []surplusLine{{code: "C", floor: f32, sized: true, above: 3}},
			want:   surplusPlan{noRoom: []string{"C"}}},
		{name: "the type with room, though another has more surplus",
			shorts: []surplusShort{{code: "S", floor: f45, sized: true, need: 1}},
			above:  []surplusLine{{code: "C", floor: f32, sized: true, above: 3}, {code: "D", floor: f32, sized: true, above: 1}},
			room:   map[string]int{"D": 1},
			want:   surplusPlan{toSources: []string{"D"}}},
		{name: "a type with no floor shares none",
			shorts: []surplusShort{{code: "S", need: 1}},
			above:  []surplusLine{{code: "A", above: 1}},
			room:   map[string]int{"A": 1},
			want:   surplusPlan{toSources: []string{"A"}}},
		{name: "a cart with no floor shares none with a sized short type",
			shorts: []surplusShort{{code: "S", floor: f45, sized: true, need: 1}},
			above:  []surplusLine{{code: "A", above: 1}},
			want:   surplusPlan{noRoom: []string{"A"}}},
		{name: "the largest need goes first",
			shorts: []surplusShort{{code: "S1", floor: f45, sized: true, need: 1}, {code: "S2", floor: f32, sized: true, need: 2}},
			above:  []surplusLine{{code: "A", floor: f45, sized: true, above: 1}, {code: "C", floor: f32, sized: true, above: 2}},
			want:   surplusPlan{toWindows: []string{"C", "C", "A"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := planSurplus(tc.shorts, tc.above, tc.room); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("planSurplus = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestKanbanSurplus_TheLoaderLockOutranksTheKeepersOwn: the arm takes a loader's lock
// from the keeper's tick and acquires under it. The order is the file's own: the loader's
// before the scan's, and both before the keeper's snapshot lock, which is why the tick
// never holds that one across the arm. A lockcheck build of the docker tests fails loudly
// on a take that breaks it (-tags docker,lockcheck, TestKanbanSurplus_*).
func TestKanbanSurplus_TheLoaderLockOutranksTheKeepersOwn(t *testing.T) {
	t.Parallel()
	loader, scan, keeper := lockorder.LoaderBudget{}.Rank(), lockorder.Scan{}.Rank(), lockorder.KanbanKeeper{}.Rank()
	if loader >= scan || scan >= keeper {
		t.Errorf("ranks loader %d, scan %d, keeper %d: want them in that order", loader, scan, keeper)
	}
}

// pinArmLine is one type as the arm's plan reads it. At the tree, a footprint.
func pinArmLine(sizes map[string]*[]float64, code string) (domain.FloorSize, bool) {
	s := sizes[code]
	if s == nil {
		return domain.FloorSize{}, false
	}
	return domain.BinType{WidthIn: (*s)[0], LengthIn: (*s)[1]}.FloorSize()
}

// TestKanbanPin_TheArmReadsTheFamilyTable: the one truth table at the arm. A surplus of the
// cart's type and a shortage of the asked type: it goes to a window exactly where the table
// says the two are one family.
func TestKanbanPin_TheArmReadsTheFamilyTable(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../kanban/testdata/same_family.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Types map[string]struct {
			Footprint *[]float64 `json:"footprint"`
		} `json:"types"`
		Cases []struct {
			Cart, Asked string
			Same        bool
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	sizes := map[string]*[]float64{}
	for code, ty := range table.Types {
		sizes[code] = ty.Footprint
	}
	for _, c := range table.Cases {
		sf, ss := pinArmLine(sizes, c.Asked)
		cf, cs := pinArmLine(sizes, c.Cart)
		p := planSurplus([]surplusShort{{code: c.Asked, floor: sf, sized: ss, need: 1}},
			[]surplusLine{{code: c.Cart, floor: cf, sized: cs, above: 1}}, nil)
		if moved := len(p.toWindows) == 1; moved != c.Same {
			t.Errorf("a %s above, a %s short: moved %v, the table says one family: %v", c.Cart, c.Asked, moved, c.Same)
		}
	}
}
