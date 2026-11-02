package main

import (
	"encoding/json"
	"os"
	"testing"

	"shingocore/plantspec"
)

// kanban_pins_family_test.go — the one truth table at simcalc: a plant file's families,
// never a size it re-measures. Prediction: PINS.md, "BRIEF v5".

// pinFamilyPlant states the table as a plant file can at this tree, with sizes.
func pinFamilyPlant(types map[string]struct {
	Family    string     `json:"family"`
	Footprint *[]float64 `json:"footprint"`
}) *plantspec.Plant {
	p := &plantspec.Plant{CarrierSizes: map[string]plantspec.CarrierSize{}}
	for code, ty := range types {
		p.BinTypes = append(p.BinTypes, code)
		if ty.Footprint != nil {
			p.CarrierSizes[code] = plantspec.CarrierSize{WidthIn: (*ty.Footprint)[0], LengthIn: (*ty.Footprint)[1]}
		}
	}
	return p
}

func TestKanbanPin_SimcalcReadsTheFamilyTable(t *testing.T) {
	raw, err := os.ReadFile("../../kanban/testdata/same_family.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Types map[string]struct {
			Family    string     `json:"family"`
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
	v := newKanbanView(pinFamilyPlant(table.Types))
	for _, c := range table.Cases {
		if same := v.floorOf[c.Cart] == v.floorOf[c.Asked]; same != c.Same {
			t.Errorf("%s and %s: simcalc says one family %v, the table says %v", c.Cart, c.Asked, same, c.Same)
		}
	}
}
