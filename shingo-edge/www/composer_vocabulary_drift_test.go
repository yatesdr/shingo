package www

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"shingoedge/domain/flowspec"
)

// composer_vocabulary_drift_test.go — A FIELD IS CALLED WHAT THE CLAIM CALLS
// IT, and there is ONE table of those words (owner ruling F3, 2026-09-12).
//
// There were five. domain/flowspec's fieldLabels, domain's shapeFields, the
// model's SHAPE_FIELDS, the model's COLUMN_LABEL and the desktop's
// <thead> — so `paired_core_node` was "Paired Core Node" in a refusal, "paired
// position" in a preset diff, "Paired" in a column heading and "Paired" again
// as a chip word, and an engineer reading a refusal had to work out which
// column it was about.
//
// flowspec.Label is the table now; everything else names the FIELD and asks.
// This is the pin that keeps it that way: no surface may spell a claim field's
// word itself.
func TestComposerSurfacesSpellNoFieldWordOfTheirOwn(t *testing.T) {
	t.Parallel()

	// The words F3 removed, and the ones the first fix replaced them with.
	// Both sets are banned as literals: the second lot were better words in a
	// second table, which is the same defect one step on.
	banned := []string{
		"old bins to", "new bins from", "stage new at", "park old at",
		"robot drives via", "third position",
		"paired position", "second paired position", "evacuation destination",
		"evacuation positions",
		"PAIRED BACK POSITION", "STAGE THE NEW BIN AT", "PARK NEW AT", "PARK OLD AT",
		"A/B PARTNER",
	}

	for _, rel := range []string{
		filepath.Join("static", "operator-station", "composer-model.js"),
		filepath.Join("static", "operator-station", "composer-render.js"),
		filepath.Join("static", "js", "pages", "processes-desktop.js"),
	} {
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		// Comments explain what the words USED to be; the rule is about what
		// the page renders.
		src := stripJSComments(string(body))
		for _, w := range banned {
			if strings.Contains(strings.ToLower(src), strings.ToLower(w)) {
				t.Errorf("%s spells %q. A field is called what the claim calls it, and the word "+
					"comes from flowspec.Label — name the field and look it up.", rel, w)
			}
		}
	}
}

// TestFlowspecLabelsReachTheStation: the generated block the model reads
// carries the labels, so fieldWord has something to look up.
//
// Without this the fallback is silent and correct-looking: every word on the
// screen becomes the raw column name, which reads like somebody chose it.
func TestFlowspecLabelsReachTheStation(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("static", "operator-station", "flowspec-data.js"))
	if err != nil {
		t.Fatalf("read flowspec-data.js: %v", err)
	}
	for _, f := range []flowspec.Field{
		flowspec.SwapMode, flowspec.Role, flowspec.PairedCoreNode,
		flowspec.SecondPairedCoreNode, flowspec.InboundStaging, flowspec.OutboundStaging,
		flowspec.InboundSource, flowspec.OutboundDestination,
		flowspec.ChangeoverEvacDestination, flowspec.ChangeoverEvacNodes, flowspec.KeyRoute,
	} {
		want := `"` + string(f) + `": "` + flowspec.Label(f) + `"`
		if !strings.Contains(string(body), want) {
			t.Errorf("flowspec-data.js carries no label for %s (want %s) — every word the composer "+
				"prints for it would fall back to the raw column name.\n"+
				"  regenerate: go test ./www -run TestStationFlowspecFileMatchesGo -update", f, want)
		}
	}
}

// stripJSComments removes // and /* */ so a note about a retired word is not
// read as the word.
//
// TRAILING COMMENTS TOO, not just whole lines. The line version was
// `^\s*//.*$`, which leaves `const X = 1; // was: paired position` intact — so
// a developer recording what a word USED to be, on the line that stopped using
// it, fired this test. That is the same defect the live-predicate scan had
// (store/claim_live_predicate_drift_test.go), and a drift test that fires on
// prose teaches people to stop writing prose.
//
// The `//` inside a string is the one case this gets wrong the other way: it
// would truncate `esc('http://…')` to `esc('http:`. Nothing in the three files
// this scans carries a banned field word after a `//` inside a literal, and a
// scanner that understood JS strings is a JS parser — which is the trade this
// file is not worth. A false PASS here is a coined word that slips through; a
// false FAIL was the one that cost an afternoon.
func stripJSComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, " ")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, " ")
}
