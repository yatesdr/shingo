package www

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

var updateProcessSetupPageGolden = flag.Bool("update-process-setup-page-golden", false,
	"rewrite testdata/process-setup-page/*.txt from the current Processes page")

// TestProcessSetupPagePins characterises what the Processes page
// (processes-desktop.js) does on top of the composer model, at the tree
// BEFORE the process-setup rework, so every later diff to the page shows up
// as the cell, option or request that moved:
//
//   - picker-options.txt: optionsFor(node, kind) for every kind the
//     positions table draws a control for, on every row;
//   - positions-table.txt: the positions table as a text grid, one line per
//     row, for an all-default style and an all-set one;
//   - advanced-shows.txt / advanced-count.txt: which Advanced fields the
//     sheet draws per mode × role × scope (one position, a recipe group) with
//     strict_pack off and on, and the row's "N set" badge beside the sheet's
//     own count;
//   - requests-*.txt: the requests each setup act sends, in order — method,
//     url and body — driven through the page's own click handlers: U0's ten
//     acts, then the rest of the page's writes and reads (the health strip,
//     the style menu, station counters, positions, Recipes, the tab reads),
//     so that every fetch( site in the page is reached by one of them;
//   - refusals-*.txt: every write of every act, refused one at a time — the
//     handler's own 4xx shape, a 500 with no body, no answer at all, and a
//     lapsed session's login page — with what the page then sent, showed,
//     left open and left unhandled;
//   - sheet-stack.txt: openSheet, runSheet, closeSheet, the Escape rule,
//     the discard question and the Advanced sheet, with scroll lock and focus;
//   - page-bugs.txt: the page's S4 bugs, one block each, "ok" or "BUG" per
//     thing that must hold (s4-page-bugs.js).
//
// The harness and the synthetic plant are in testdata/process-setup-harness
// (pins.js and p0-page.js say what each pin covers and how each act is
// driven). The page runs under node, unmodified: nothing here or there
// changes a source file.
//
// Regenerate with
//
//	go test ./www -run TestProcessSetupPagePins -update-process-setup-page-golden
//
// and read the diff: a regenerated golden pins nothing unless someone read it.
func TestProcessSetupPagePins(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the process-setup page pins")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "process-setup-harness", "pins.js"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the process-setup pins: %v\n%s", err, stderr.String())
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the pins' output: %v\n%s", err, stderr.String())
	}
	if len(got) == 0 {
		t.Fatal("the pins produced no golden; the harness has drifted")
	}

	// ITS OWN DIRECTORY, because the stale check below owns every file in it:
	// the composer-model grid's golden sat beside these in process-setup/ and
	// was reported as a golden no pin writes.
	dir := filepath.Join("testdata", "process-setup-page")
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if *updateProcessSetupPageGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if *updateProcessSetupPageGolden {
			if err := os.WriteFile(path, []byte(got[n]), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with -update-process-setup-page-golden and read it", n, err)
			continue
		}
		if line, w, g, ok := firstDiffLine(string(want), got[n]); !ok {
			t.Errorf("%s: the page moved at line %d; regenerate only for a named change\n  golden: %s\n  page:   %s",
				path, line, w, g)
		}
	}

	// A golden no pin writes any more pins nothing and reads as if it did.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if _, ok := got[e.Name()]; ok || e.IsDir() {
			continue
		}
		if *updateProcessSetupPageGolden {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("remove stale %s: %v", e.Name(), err)
			}
			continue
		}
		t.Errorf("%s: a golden no pin writes; delete it, or restore the pin that wrote it", filepath.Join(dir, e.Name()))
	}
	t.Logf("process-setup pins: %s", strings.Join(names, ", "))
}

// firstDiffLine finds the first line two texts disagree on, 1-based, with
// both sides of it; ok is true when they are equal.
func firstDiffLine(want, got string) (line int, w, g string, ok bool) {
	if want == got {
		return 0, "", "", true
	}
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var a, b string
		if i < len(wl) {
			a = wl[i]
		} else {
			a = "(end of file)"
		}
		if i < len(gl) {
			b = gl[i]
		} else {
			b = "(end of file)"
		}
		if a != b {
			return i + 1, a, b, false
		}
	}
	return len(wl), "", "", false
}
