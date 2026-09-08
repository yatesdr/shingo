package www

import (
	"io/fs"
	"testing"

	"shingo/shared/clockglobals"
)

// TestEveryClockPageCarriesGlobals is Core's half of the shared rule: a full
// page whose scripts reach a clock function in shared/utils.js must carry
// window.PLANT_TZ and window.SHINGO_CLOCK.
//
// The audit lives in shingo/shared/clockglobals because the contract does —
// utils.js is shipped from that module and read by both trees, and one rule
// implemented twice is how the halves drift apart. See that package for what
// this catches and why nothing failed while it was broken.
//
// Core had four pages in that state: dashboard-display, dashboard-map,
// dashboard-node-report and heartbeat are standalone full pages that do not
// render through layout.html, so they never picked up the inlines that lived
// in it — the big display board included.
func TestEveryClockPageCarriesGlobals(t *testing.T) {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("sub static: %v", err)
	}

	// Core's full pages are layout.html and the standalone dashboards; all of
	// them now pull the partial, so the partial (or a literal inline) is the
	// only marker that counts here.
	carriers := []string{
		`{{template "clock-globals"`,
		"window.PLANT_TZ",
	}

	findings, subject, err := clockglobals.Audit(templateFS, static, carriers)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, f := range findings {
		t.Error(clockglobals.Explain(f))
	}

	// A guard that silently matches nothing is worse than no guard: if the
	// script resolver or the import walk breaks, every page trivially passes.
	if subject == 0 {
		t.Fatal("no Core page was found to load a clock function — the script/import walk is " +
			"broken, not the templates (layout.html and the dashboards both qualify)")
	}
}
