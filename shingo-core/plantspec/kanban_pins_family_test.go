package plantspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kanban_pins_family_test.go — what a plant file can say about a family: its members, a pool
// per family, and a slot's types, by type or by family. Prediction: PINS.md, "BRIEF v5".

const pinFamilySpec = `
bin_types: [SMALL, BIG, CART32]
carrier_families:
  - {code: F45, types: [SMALL, BIG]}
zones:
  - name: K
    positions:
      - {name: S1, depth: 1, admits: [F45]}
      - {name: S2, depth: 1, admits: [CART32]}
`

func pinLoadSpec(t *testing.T, spec string) (*Plant, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plant.yaml")
	if err := os.WriteFile(path, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// TestKanbanPin_APlantFileStatesFamiliesAndAdmits: the file loads.
func TestKanbanPin_APlantFileStatesFamiliesAndAdmits(t *testing.T) {
	t.Parallel()
	if _, err := pinLoadSpec(t, pinFamilySpec); err != nil {
		t.Errorf("the spec does not load: %v", err)
	}
}

// TestKanbanPin_AnAdmitsCodeThatIsBothIsRefused: a code naming both a type and a family says
// nothing a reader can trust.
func TestKanbanPin_AnAdmitsCodeThatIsBothIsRefused(t *testing.T) {
	t.Parallel()
	p, err := pinLoadSpec(t, strings.ReplaceAll(pinFamilySpec, "code: F45", "code: CART32"))
	if err != nil {
		t.Fatalf("the spec does not load: %v", err)
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("Validate = %v, want it to refuse a code that is both a type and a family", err)
	}
}
