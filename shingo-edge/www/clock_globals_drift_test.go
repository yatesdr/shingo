package www

import (
	"testing"

	"shingo/shared/clockglobals"
)

// TestEveryClockPageCarriesGlobals is Edge's half of the shared rule: a full
// page whose scripts reach a clock function in shared/utils.js must carry
// window.PLANT_TZ and window.SHINGO_CLOCK.
//
// The audit lives in shingo/shared/clockglobals because the contract does —
// utils.js is shipped from that module and read by both trees, and one rule
// implemented twice is how the halves drift apart. See that package for what
// this catches and why nothing failed while it was broken.
//
// Verified red by deleting the partial include from operator-display.html:
// "templates/operator-display.html loads /static/operator-station/operator.js,
// which reads the plant clock, but the page carries no clock globals."
func TestEveryClockPageCarriesGlobals(t *testing.T) {
	// Edge spells inclusion two ways: the partial directly (the operator
	// kiosk, which has its own header) or the shared header that carries it.
	carriers := []string{
		`{{template "clock-globals"`,
		`{{template "header"`,
		"window.PLANT_TZ",
	}

	findings, subject, err := clockglobals.Audit(templatesFS, StaticFS(), carriers)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, f := range findings {
		t.Error(clockglobals.Explain(f))
	}

	// A guard that silently matches nothing is worse than no guard: if the
	// script resolver or the import walk breaks, every page trivially passes.
	if subject == 0 {
		t.Fatal("no Edge page was found to load a clock function — the script/import walk is " +
			"broken, not the templates (operator-display.html and header.html both qualify)")
	}
}
