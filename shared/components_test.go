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

var updateComponentsGolden = flag.Bool("update-components-golden", false,
	"rewrite testdata/components/golden/*.txt from the shared components as they run")

// TestSharedComponentSpecs runs the specs of the four shared UI components:
// layer.js (the one overlay stack), notice.js (toast and banner), http.js (the
// HTTP door) and picker.js (the picker's shell). Each spec is a node script in
// testdata/components that loads its component unmodified in the helper pins'
// page stubs (testdata/helper-pins-harness/world.js); every case names the
// behaviour it holds, and a "cannot happen:" case names the P0 bug it rules
// out. Skipped if node is not on PATH, as the other JS wrappers are.
func TestSharedComponentSpecs(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the component specs")
	}
	for _, name := range []string{"layer", "notice", "http", "picker", "node-picker"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command(nodePath, filepath.Join("testdata", "components", name+".test.js")).CombinedOutput()
			if err != nil {
				t.Fatalf("%s.test.js failed:\n%s\nerror: %v", name, out, err)
			}
			t.Logf("%s", out)
		})
	}
}

// TestSharedComponentPins runs the helper pins' case lists for modal,
// confirm / prompt, toast and api (TestHelperPins, testdata/helper-pins) on
// the components and holds the answers in testdata/components/golden. The
// cases are the same ones, so a golden here diffs line for line against a
// hub's: when wave C swaps a hub's names onto the components, that hub's
// golden moves to read as these do, header aside.
//
// Regenerate with
//
//	go test . -run TestSharedComponentPins -update-components-golden
//
// and read the diff.
func TestSharedComponentPins(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the component pins")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "components", "cases.js"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the component case lists: %v\n%s", err, stderr.String())
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the case lists' output: %v\n%s", err, stderr.String())
	}
	if len(got) == 0 {
		t.Fatal("the case lists produced no golden; the harness has drifted")
	}

	// ITS OWN DIRECTORY: the stale check below owns every file in it.
	dir := filepath.Join("testdata", "components", "golden")
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		path := filepath.Join(dir, n)
		if *updateComponentsGolden {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
			if err := os.WriteFile(path, []byte(got[n]), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with -update-components-golden and read it", n, err)
			continue
		}
		if line, w, g, ok := helperPinsFirstDiff(string(want), got[n]); !ok {
			t.Errorf("%s: the components moved at line %d; regenerate only for a named change\n  golden:     %s\n  components: %s",
				path, line, w, g)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if _, ok := got[e.Name()]; ok || e.IsDir() {
			continue
		}
		if *updateComponentsGolden {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("remove stale %s: %v", e.Name(), err)
			}
			continue
		}
		t.Errorf("%s: a golden no case writes; delete it, or restore the case that wrote it", filepath.Join(dir, e.Name()))
	}
	t.Logf("component pins: %s", strings.Join(names, ", "))
}
