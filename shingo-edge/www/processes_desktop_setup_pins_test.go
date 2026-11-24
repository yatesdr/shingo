package www

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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
	got := runProcessSetupPinGroups(t, nodePath, filepath.Join("testdata", "process-setup-harness", "pins.js"))
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

// processSetupPinGroup is one group of pins.js (`node pins.js --groups`): the
// goldens one pin writes. Reads, when set, is a pattern over golden names:
// the goldens of the other groups the group reads on stdin (page-bugs.txt's
// F22 block reads the refusal goldens).
type processSetupPinGroup struct {
	Name  string `json:"name"`
	Reads string `json:"reads"`
}

// runProcessSetupPinGroups runs every group of pins.js in a node process of
// its own and returns the goldens they print, merged. ONE PROCESS RAN THEM ALL
// IN SERIES until 2026-11-23, on one core (31 s alone, 47-50 s in this
// package); the groups share nothing but the boot preview, which each runs for
// itself, and the refusal goldens, which are handed to their reader here.
//
// At most pinGroupSlots groups run at once. A reading group starts with the rest,
// outside that bound (it blocks on its stdin, not a core, once its own pins are
// done), and is handed what the other groups printed once they have all
// finished, so a reader can never wait on a group that waits for a slot. A
// golden printed by two groups, a group that prints none, and a group that
// fails are each an error naming the group.
// pinGroupSlots is how many node groups run at once. It was GOMAXPROCS (24 on
// the gate laptop): 24 node processes each loading the 582 KB page script land
// as one burst inside the gate's unit step, and on 2026-11-23 four different
// timing-sensitive tests in other packages (Edge engine and cmd/shingoedge,
// Core www) each failed once in ten tier runs of the tip, none in six of the
// base, and each passed 20 of 20 alone. Four keeps the pins at a few seconds of
// wall without that burst.
const pinGroupSlots = 4

func runProcessSetupPinGroups(t *testing.T, nodePath, script string) map[string]string {
	t.Helper()
	var stderr bytes.Buffer
	list := exec.Command(nodePath, script, "--groups")
	list.Stderr = &stderr
	raw, err := list.Output()
	if err != nil {
		t.Fatalf("list the process-setup pin groups: %v\n%s", err, stderr.String())
	}
	var groups []processSetupPinGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		t.Fatalf("decode the pin groups: %v\n%s", err, raw)
	}
	if len(groups) == 0 {
		t.Fatal("pins.js lists no group; the harness has drifted")
	}

	type result struct {
		out    map[string]string
		err    error
		stderr string
	}
	reads := make([]*regexp.Regexp, len(groups))
	for i, g := range groups {
		if g.Reads == "" {
			continue
		}
		re, err := regexp.Compile(g.Reads)
		if err != nil {
			t.Fatalf("pin group %s reads %q: %v", g.Name, g.Reads, err)
		}
		reads[i] = re
	}
	results := make([]result, len(groups))
	// run runs group i. feed, when given, writes the group's stdin and closes
	// it; its error counts only when the group itself succeeded (a group that
	// died first closes the pipe under the feed, which is not the news).
	run := func(i int, feed func(io.Writer) error) {
		cmd := exec.Command(nodePath, script, "--group", groups[i].Name)
		var e bytes.Buffer
		cmd.Stderr = &e
		var fed chan error
		if feed != nil {
			w, err := cmd.StdinPipe()
			if err != nil {
				results[i] = result{err: err}
				return
			}
			fed = make(chan error, 1)
			go func() {
				err := feed(w)
				if cerr := w.Close(); err == nil {
					err = cerr
				}
				fed <- err
			}()
		}
		raw, err := cmd.Output()
		if fed != nil {
			if ferr := <-fed; err == nil && ferr != nil {
				err = fmt.Errorf("hand it the goldens it reads: %w", ferr)
			}
		}
		r := result{err: err, stderr: e.String()}
		if err == nil {
			r.err = json.Unmarshal(raw, &r.out)
		}
		results[i] = r
	}

	slots := make(chan struct{}, pinGroupSlots)
	var writers, readers sync.WaitGroup
	for i := range groups {
		if reads[i] != nil {
			continue
		}
		writers.Add(1)
		go func() {
			defer writers.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			run(i, nil)
		}()
	}
	for i := range groups {
		if reads[i] == nil {
			continue
		}
		readers.Add(1)
		go func() {
			defer readers.Done()
			run(i, func(w io.Writer) error {
				writers.Wait()
				given := map[string]string{}
				for j := range results {
					// A reader's slot is still being written: skip it before reading it.
					if reads[j] != nil {
						continue
					}
					for name, text := range results[j].out {
						if reads[i].MatchString(name) {
							given[name] = text
						}
					}
				}
				return json.NewEncoder(w).Encode(given)
			})
		}()
	}
	writers.Wait()
	readers.Wait()

	got := map[string]string{}
	from := map[string]string{}
	for i, r := range results {
		g := groups[i].Name
		if r.err != nil {
			t.Errorf("pin group %s: %v\n%s", g, r.err, r.stderr)
			continue
		}
		if len(r.out) == 0 {
			t.Errorf("pin group %s printed no golden; the harness has drifted", g)
		}
		for name, text := range r.out {
			if other, ok := from[name]; ok {
				t.Errorf("%s is printed by pin groups %s and %s; a golden has one writer", name, other, g)
			}
			got[name], from[name] = text, g
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return got
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
