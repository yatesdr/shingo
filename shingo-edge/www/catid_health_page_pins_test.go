package www

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var updateCatidHealthPageGolden = flag.Bool("update-catid-health-page-golden", false,
	"rewrite testdata/catid-health-page/*.txt from the Processes page's health strip")

// TestCatidHealthPagePins characterises the CATID health items on the
// Processes page (processes-desktop.js refreshCatidHealth and healthSection,
// catid-health.js): S4 took the strip off the list, so
//
//   - requests-list.txt: the requests a boot of the list and each 10 s tick
//     send with 0, 1 and 3 processes that have a red or amber item (the
//     summary alone), focus and visibility (none), and a refused summary
//     (the counts are left as they were);
//   - strip.txt: each list row's one count from that answer, no strip, and
//     item 5's two styles whose names differ by a trailing space;
//   - requests-process.txt: a process open reads its items on the Settings
//     tab only, never on the tick, and draws every item, worst first, with
//     its verbs, as its Health section;
//   - sse-hint.txt: the page opens no /events stream (the hint is gone).
//
// The answers are TestCatidHealthBodiesPins' goldens (testdata/catid-health/),
// so regenerate that one first. The harness is testdata/catid-health-harness
// (pins.js), on U0's page harness, unmodified. Regenerate with
//
//	go test ./www -run TestCatidHealthPagePins -update-catid-health-page-golden
//
// and read the diff.
func TestCatidHealthPagePins(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the health strip page pins")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "catid-health-harness", "pins.js"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the health strip pins: %v\n%s", err, stderr.String())
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the pins' output: %v\n%s", err, stderr.String())
	}
	if len(got) == 0 {
		t.Fatal("the pins produced no golden; the harness has drifted")
	}
	checkGoldenDir(t, filepath.Join("testdata", "catid-health-page"), got, *updateCatidHealthPageGolden,
		"-update-catid-health-page-golden")
}

// TestCatidHealthHint_NoEdgePageListens: the "catid-health" SSE hint is gone
// (the engine sends no such frame; the Processes list's 10 s refresh is its
// clock, owner 2026-11-11: "we are good with 10s of delay"), so a page that
// listens for it waits for nothing. No Edge page or template names it: a
// createSSE handler key, an onSSE or station type string, an htmx trigger.
func TestCatidHealthHint_NoEdgePageListens(t *testing.T) {
	t.Parallel()
	// The ways an Edge page subscribes to an SSE type: createSSE's handler
	// key, onSSE's or a station's type string, an htmx trigger. A fetch URL
	// (/api/catid-health) is none of them.
	listen := regexp.MustCompile(`\bonCatidHealth\b|sse:catid-health|['"]catid-health['"]`)
	var found []string
	for _, root := range []string{"static", "templates"} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if ext := filepath.Ext(p); ext != ".js" && ext != ".html" {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				if listen.MatchString(line) {
					found = append(found, filepath.ToSlash(p)+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	sort.Strings(found)
	if len(found) != 0 {
		t.Errorf("a page now listens for the catid-health hint; update this pin and sse-hint.txt:\n  %s",
			strings.Join(found, "\n  "))
	}
}
