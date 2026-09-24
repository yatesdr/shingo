package www

import (
	"io/fs"
	"strings"
	"testing"
)

// The report-divergence panel sits in the ledger row on /inventory, beside the
// delta-integrity panel: the owner ruled the placement. The two are read as a
// pair — one says which counts never landed, the other which carriers the Edge
// and Core still disagree about — so they share the row and the fetch.
// Verify-red at the base: there is no container for it.
func TestInventoryPageHasTheReportDivergencePanel(t *testing.T) {
	body, err := fs.ReadFile(templateFS, "templates/inventory.html")
	if err != nil {
		t.Fatalf("read inventory.html: %v", err)
	}
	src := string(body)
	row := strings.Index(src, `<div class="ledger-row">`)
	panel := strings.Index(src, `<div id="rh-report-divergence"></div>`)
	integrity := strings.Index(src, `<div id="rh-delta-integrity"></div>`)
	if panel < 0 {
		t.Fatal(`inventory.html has no <div id="rh-report-divergence"></div>; the panel has nowhere to render`)
	}
	if row < 0 || integrity < 0 || panel < row || panel < integrity {
		t.Errorf("the report-divergence container is not in the ledger row after the delta-integrity panel "+
			"(row at %d, integrity at %d, panel at %d)", row, integrity, panel)
	}
}
