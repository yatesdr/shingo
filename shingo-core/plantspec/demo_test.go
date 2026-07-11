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

// The demo plant must seed all three loader TYPES so each is exercisable end to end:
// MULTI-WINDOW (a shared_window loader keyed on a synthetic id, ≥2 windows), SINGLE-
// WINDOW (a shared_window loader anchored at its own real node), and DEDICATED-
// POSITIONS (a dedicated_positions loader, ≥2 same-payload positions + a buffer zone).
// Guards the demo against losing that coverage. Assertions are SHAPE-based (each type
// present), not count-based, so tuning the demo (window/position counts) doesn't trip it.
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

	// SINGLE-WINDOW: PLK_X1 is its own loader (no window_of / home_of).
	x1, ok := byNode["PLK_X1"]
	if !ok {
		t.Fatal("missing single-window loader claim PLK_X1")
	}
	if x1.WindowOf != "" || x1.HomeOf != "" {
		t.Fatalf("PLK_X1 must be its own loader (no window_of/home_of), got window_of=%q home_of=%q", x1.WindowOf, x1.HomeOf)
	}
	if !x1.IsManualSwap() || x1.Role != "produce" {
		t.Fatalf("PLK_X1 must be a manual_swap produce loader, got swap=%q role=%q", x1.SwapMode, x1.Role)
	}

	// DEDICATED-POSITIONS: PLK_D1 + PLK_D2, both home_of PLK_DECK, SAME payload, buffer wired.
	var deck []Claim
	for _, c := range p.Claims {
		if c.HomeOf == "PLK_DECK" {
			deck = append(deck, c)
		}
	}
	if len(deck) != 2 {
		t.Fatalf("dedicated PLK_DECK: want 2 positions, got %d", len(deck))
	}
	if deck[0].Payload != deck[1].Payload || deck[0].Payload == "" {
		t.Fatalf("PLK_DECK positions must carry the SAME payload (same-payload-two-position), got %q and %q", deck[0].Payload, deck[1].Payload)
	}
	for _, c := range deck {
		if c.BufferDest == "" {
			t.Fatalf("PLK_DECK position %s must set buffer_dest (step-7 buffer)", c.CoreNode)
		}
	}

	// Dedicated-loader buffer zone present.
	bufFound := false
	for _, z := range p.Zones {
		if z.Name == "SYN_BUF_Deck" {
			bufFound = true
		}
	}
	if !bufFound {
		t.Fatal("missing dedicated-loader buffer zone SYN_BUF_Deck")
	}
}
