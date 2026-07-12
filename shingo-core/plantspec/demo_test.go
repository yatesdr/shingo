package plantspec

import "testing"

// The shipped demo plant must always load + validate — guards plants/demo.yaml
// against drift (a dangling ref or missing staging would break `make dev-seed`).
func TestShippedDemoPlantValid(t *testing.T) {
	p, err := Load("../../plants/demo.yaml")
	if err != nil {
		t.Fatalf("load plants/demo.yaml: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("plants/demo.yaml invalid: %v", err)
	}
}

// The demo plant must seed both shared loader TYPES so each is exercisable end to end:
// MULTI-WINDOW (a shared_window loader keyed on a synthetic id, ≥2 windows) and SINGLE-
// WINDOW (a shared_window loader anchored at its own real node — the demo has ≥1). The
// dedicated-positions loader is covered by the stable seed fixture
// (cmd/seeddev/testdata/seed-fixture.yaml), not the demo. Guards the demo against losing
// loader coverage. Assertions are SHAPE-based (each type present), not count-based, so
// tuning the demo (window counts, adding/dropping a leg) doesn't trip it.
func TestShippedDemoPlantLoaderTypes(t *testing.T) {
	p, err := Load("../../plants/demo.yaml")
	if err != nil {
		t.Fatalf("load plants/demo.yaml: %v", err)
	}

	byNode := map[string]Claim{}
	for _, c := range p.Claims {
		byNode[c.CoreNode] = c
	}

	// MULTI-WINDOW: a synthetic shared_window loader with ≥2 window_of claims.
	var multiLoader string
	windows := 0
	for _, c := range p.Claims {
		if c.WindowOf != "" {
			multiLoader = c.WindowOf
			windows++
		}
	}
	if windows < 2 {
		t.Fatalf("multi-window loader %q: want ≥2 windows, got %d", multiLoader, windows)
	}

	// SINGLE-WINDOW: ≥1 manual_swap produce loader that is its own anchor (no window_of /
	// home_of). The demo may carry several (one per single-payload component leg); assert
	// at least one exists.
	var singles []Claim
	for _, c := range p.Claims {
		if c.IsManualSwap() && c.Role == "produce" && c.WindowOf == "" && c.HomeOf == "" {
			singles = append(singles, c)
		}
	}
	if len(singles) < 1 {
		t.Fatalf("single-window loaders: want ≥1 (a manual_swap produce loader with no window_of/home_of), got %d", len(singles))
	}
}
