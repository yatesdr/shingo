package shared

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

var updateHelperPinsGolden = flag.Bool("update-helper-pins-golden", false,
	"rewrite testdata/helper-pins/*.txt from the UI helpers as they run today")

// TestHelperPins characterises the UI helpers on each surface as they are
// BEFORE the shared-UI brief's S2 replaces the three copies with one set, so
// the swap behind each name shows up as that surface's golden moving:
//
//   - modal.<surface>.txt: showModal / hideModal — open and close classes,
//     Escape, backdrop click, body scroll lock, focus, fields cleared on hide;
//   - dialog.<surface>.txt: confirm / prompt (Core's uiConfirm, uiPrompt and
//     uiConfirmWithInput) — what each resolves to on OK, Cancel, Escape,
//     backdrop and a second open, the DOM it writes, focus;
//   - toast.<surface>.txt: the DOM, levels, lifetime, stacking, escaping;
//   - api.<surface>.txt: each api's shape, the request every verb sends, and
//     what every answer (200/204/empty/non-JSON, the servers' refusal bodies,
//     plain text, 5xx, no answer) resolves or rejects with;
//   - sse.*.txt: the Edge hub's createSSE, shared createSSE, the onSSE bus,
//     what Core's app.js does with the bus on import, and htmx-sse.js —
//     URL, dispatch, reconnect backoff, watchdog, build change, close;
//   - htmx-errors.edge-admin.txt: who on Edge admin handles an htmx request
//     error (nobody, today);
//   - hubs.txt: which copy each hub exports under each helper name.
//
// The surfaces are core-hub (shingo-core/www/static/app.js), edge-hub
// (shingo-edge/www/static/js/shingoedge.js) and shared (utils.js here). The
// harness and its DOM, clock, fetch and EventSource stubs are in
// testdata/helper-pins-harness (pins.js says what each golden covers; world.js
// how a page is built). Every source file loads under node unmodified.
//
// Regenerate with
//
//	go test . -run TestHelperPins -update-helper-pins-golden
//
// and read the diff: a regenerated golden pins nothing unless someone read it.
func TestHelperPins(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the UI helper pins")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "helper-pins-harness", "pins.js"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the helper pins: %v\n%s", err, stderr.String())
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the pins' output: %v\n%s", err, stderr.String())
	}
	if len(got) == 0 {
		t.Fatal("the pins produced no golden; the harness has drifted")
	}

	// ITS OWN DIRECTORY: the stale check below owns every file in it, so no
	// other pin may write there.
	dir := filepath.Join("testdata", "helper-pins")
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if *updateHelperPinsGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if *updateHelperPinsGolden {
			if err := os.WriteFile(path, []byte(got[n]), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with -update-helper-pins-golden and read it", n, err)
			continue
		}
		if line, w, g, ok := helperPinsFirstDiff(string(want), got[n]); !ok {
			t.Errorf("%s: the helpers moved at line %d; regenerate only for a named change\n  golden:  %s\n  helpers: %s",
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
		if *updateHelperPinsGolden {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("remove stale %s: %v", e.Name(), err)
			}
			continue
		}
		t.Errorf("%s: a golden no pin writes; delete it, or restore the pin that wrote it", filepath.Join(dir, e.Name()))
	}
	t.Logf("helper pins: %s", strings.Join(names, ", "))
}

// helperPinsFirstDiff finds the first line two texts disagree on, 1-based,
// with both sides of it; ok is true when they are equal.
func helperPinsFirstDiff(want, got string) (line int, w, g string, ok bool) {
	if want == got {
		return 0, "", "", true
	}
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		a, b := "(end of file)", "(end of file)"
		if i < len(wl) {
			a = wl[i]
		}
		if i < len(gl) {
			b = gl[i]
		}
		if a != b {
			return i + 1, a, b, false
		}
	}
	return len(wl), "", "", false
}
