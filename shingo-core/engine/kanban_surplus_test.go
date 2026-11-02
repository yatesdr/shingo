package engine

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"shingocore/internal/lockorder"
)

// kanban_surplus_test.go — the surplus arm's decisions with no database.

func TestPlanSurplus(t *testing.T) {
	t.Parallel()
	const f45, f32 = int64(1), int64(2)
	for _, tc := range []struct {
		name   string
		shorts []surplusShort
		above  []surplusLine
		want   []string
	}{
		{name: "the largest of the family, not the first",
			shorts: []surplusShort{{code: "S", family: f45, need: 1}},
			above:  []surplusLine{{code: "A", family: f45, above: 1}, {code: "B", family: f45, above: 2}},
			want:   []string{"B"}},
		{name: "a tie takes the lines' order",
			shorts: []surplusShort{{code: "S", family: f45, need: 1}},
			above:  []surplusLine{{code: "A", family: f45, above: 2}, {code: "B", family: f45, above: 2}},
			want:   []string{"A"}},
		{name: "the need, not the surplus, bounds the moves",
			shorts: []surplusShort{{code: "S", family: f45, need: 2}},
			above:  []surplusLine{{code: "A", family: f45, above: 5}},
			want:   []string{"A", "A"}},
		{name: "the surplus bounds the moves",
			shorts: []surplusShort{{code: "S", family: f45, need: 2}},
			above:  []surplusLine{{code: "A", family: f45, above: 1}},
			want:   []string{"A"}},
		{name: "another family is never asked",
			shorts: []surplusShort{{code: "S", family: f45, need: 1}},
			above:  []surplusLine{{code: "C", family: f32, above: 3}},
			want:   nil},
		{name: "the largest need goes first",
			shorts: []surplusShort{{code: "S1", family: f45, need: 1}, {code: "S2", family: f32, need: 2}},
			above:  []surplusLine{{code: "A", family: f45, above: 1}, {code: "C", family: f32, above: 2}},
			want:   []string{"C", "C", "A"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := planSurplus(tc.shorts, tc.above); !reflect.DeepEqual(got, tc.want) {
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
			Family string `json:"family"`
		} `json:"types"`
		Cases []struct {
			Cart, Asked string
			Same        bool
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	family := func(code string) int64 {
		f := table.Types[code].Family
		if ids[f] == 0 {
			ids[f] = int64(len(ids) + 1)
		}
		return ids[f]
	}
	for _, c := range table.Cases {
		p := planSurplus([]surplusShort{{code: c.Asked, family: family(c.Asked), need: 1}},
			[]surplusLine{{code: c.Cart, family: family(c.Cart), above: 1}})
		if moved := len(p) == 1; moved != c.Same {
			t.Errorf("a %s above, a %s short: moved %v, the table says one family: %v", c.Cart, c.Asked, moved, c.Same)
		}
	}
}
