package www

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestSourceabilityEventsHasAUIConsumer.
//
// `GET /api/sourceability/events` shipped in 4975247c — routed, filterable,
// written to by both of SourceabilityMonitor's recompute paths — and had ZERO UI
// consumers for a whole release. The commit said "No new page" in as many words,
// so nothing was broken; the endpoint simply answered a question nobody could
// ask. That is a failure mode no existing test covers: every drift test here
// checks that a thing REFERENCED exists, and this was the mirror — a thing that
// exists and is referenced by nothing.
//
// This test is narrow on purpose. The general version — "every read-only /api
// GET route has a consumer" — was measured while writing it: 17 of Core's 107
// non-parameterised GET routes have no textual reference under www/static or
// www/templates, and some of those are false positives (a URL built by
// concatenation) while others are real. Notably `/api/parts/mission-duration` is
// among them. That is worth building and worth an owner's attention; it is not
// worth guessing an allowlist for here.
func TestSourceabilityEventsHasAUIConsumer(t *testing.T) {
	const endpoint = "/api/sourceability/events"

	var consumers []string
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".js" {
			return nil
		}
		body, err := fs.ReadFile(staticFS, p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		if strings.Contains(string(body), endpoint) {
			consumers = append(consumers, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk static: %v", err)
	}

	if len(consumers) == 0 {
		t.Errorf("no page script fetches %s, so the sourceability verdict history is unreadable again.\n"+
			"  The table is written by both recompute paths and the endpoint is routed — what goes missing is the only\n"+
			"  way anyone can see it. If the panel is being retired deliberately, retire the endpoint with it.", endpoint)
	}
}

// TestSourcingPaneCarriesAVerdictHistoryContainer.
//
// The panel is filled client-side, keyed off `data-process` — so the container
// has to be emitted per pane, inside the {{range}} over processes, or the JS
// finds nothing and the page silently loses the history with no error anywhere.
// A missing container is invisible: the fetch succeeds, the grouping succeeds,
// and the loop writes to zero elements.
//
// It also pins WHERE the panel lives, which was a decision rather than a
// default. Row 7.2 proposed putting it beside the ledger-exceptions panel on
// /inventory. It is here instead because sourceability_events is keyed by
// (process_key, style_id) — exactly this page's rail-and-pane key — so the pane
// IS the filter and no filter UI is needed; the ledger-exceptions panel is keyed
// by bin/carrier and would make the reader re-supply a process context that page
// does not have.
func TestSourcingPaneCarriesAVerdictHistoryContainer(t *testing.T) {
	body, err := fs.ReadFile(templateFS, "templates/sourcing.html")
	if err != nil {
		t.Fatalf("read sourcing.html: %v", err)
	}
	src := string(body)

	if !strings.Contains(src, `class="src-history" data-process=`) {
		t.Error(`sourcing.html has no <div class="src-history" data-process=…> — the verdict-history panel has nowhere to render.` +
			"\n  It must sit inside the per-process {{range}} so each pane gets its own container keyed by process.")
	}

	// The container must be inside the pane loop, not once on the page. The
	// cheap structural proof: the process variable the panes are keyed on is the
	// same one the container is keyed on.
	if !strings.Contains(src, `class="src-history" data-process="{{$p.ProcessID}}"`) {
		t.Error(`the src-history container is not keyed on {{$p.ProcessID}}, so it is either outside the per-process range` +
			"\n  or keyed on something the JS does not group by. Both render an empty panel with no error.")
	}

	// No chips. The page already has a verdict pill vocabulary (.src-v-*) and a
	// second one on the same screen for the same concept is how U5 started.
	if strings.Contains(src, `"chip`) || strings.Contains(src, ` chip-`) {
		t.Error("sourcing.html references a chip class. This page's verdict vocabulary is .src-v-*; two pill vocabularies " +
			"for one concept on one screen is the U5 shape.")
	}
}
