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
