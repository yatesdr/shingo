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

var updateBigLineGolden = flag.Bool("update-bigline-golden", false,
	"rewrite testdata/process-bigline-page/*.txt from the Processes page on the big line")

// TestBigLinePagePins runs the Processes page under node on the big line
// (testdata/process-setup-harness/bigline.js: 23 positions, 108 nodes, 46
// parts, 13 styles, 3 recipes in 12 versions, 2 light curtains) and holds
// what each view asks for, with each answer's size, and what Settings draws
// at that size. Every slice of the one-process-page build moves these on
// purpose, and its report says which line moved and why.
//
// Regenerate with
//
//	go test ./www -run TestBigLinePagePins -update-bigline-golden
//
// and read the diff.
func TestBigLinePagePins(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the big-line page pins")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "process-setup-harness", "bigline.js"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the big-line pins: %v\n%s", err, stderr.String())
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the big-line pins: %v\n%s", err, stderr.String())
	}
	if len(got) == 0 {
		t.Fatal("the big-line pins produced no golden; the harness has drifted")
	}
	// NO VIEW ASKS ONCE PER POSITION (the build brief's Pi rule). bigline.js
	// folds a per-position URL into one "N × …/{id}/…" line, so such a line in
	// any view is the cost growing with the line.
	for _, line := range strings.Split(got["bigline-requests.txt"], "\n") {
		if strings.Contains(line, " × ") && strings.Contains(line, "/{id}/") {
			t.Errorf("a view makes one request per position: %s", strings.TrimSpace(line))
		}
	}
	dir := filepath.Join("testdata", "process-bigline-page")
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if *updateBigLineGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if *updateBigLineGolden {
			if err := os.WriteFile(path, []byte(got[n]), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with -update-bigline-golden and read it", n, err)
			continue
		}
		if line, w, g, ok := firstDiffLine(string(want), got[n]); !ok {
			t.Errorf("%s: the page moved at line %d; regenerate only for a named change\n  golden: %s\n  page:   %s",
				path, line, w, g)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if _, ok := got[e.Name()]; !ok && !e.IsDir() {
			t.Errorf("%s: a golden no pin writes; delete it", e.Name())
		}
	}
}
