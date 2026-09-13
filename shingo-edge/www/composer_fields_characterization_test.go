package www

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"shingoedge/domain/flowspec"
)

// TestComposerFieldsCharacterization runs the Node-based characterization for
// the composer's field rules — the suite that REPLACED the claim editor's.
//
// The old one walked every (role, swap_mode) cell through processes.js and
// asserted which of the claim modal's twenty-seven controls were on screen and
// what saveClaim POSTed. The modal retired in U9d; the questions it answered
// did not, and the answers moved to the desktop composer. The replacement
// walks the same matrix against the model and refuses to run if it pins fewer
// facts than the suite it replaces (528, printed by the last green run before
// the deletion) — a replacement that checks less is not one.
//
// Self-contained: no npm, skipped when node is absent.
func TestComposerFieldsCharacterization(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the composer field characterization")
	}
	scriptPath := filepath.Join("static", "js", "pages", "composer-fields.characterization.test.js")
	out, err := exec.Command(nodePath, scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("composer field characterization failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("composer fields: %s", out)
}

// -update regenerates the station's flowspec copy.
var updateFlowspec = flag.Bool("update", false, "regenerate the station's flowspec copy")

// TestStationFlowspecFileMatchesGo — the operator station's copy of the same
// tables, and the same rule: flowspec.ExportJSON, byte for byte.
//
// WHY A FILE AND NOT AN ENDPOINT. The composer's model clears the fields a
// choreography forbids when the operator changes it, and the flowspec is the
// oracle for which those are — so its correctness cannot sit behind a round
// trip on a screen whose whole point is that a scan does not wait.
//
// ONE FILE, BOTH SURFACES. The desktop's copy used to live inside processes.js
// and this note used to say the bytes existed twice. That page is gone and
// processes.html loads this same file (processes.html:59), so there is exactly
// one rendering of the tables in the tree, Go is its only author, and the
// flowspec.json golden that sat beside the second copy went with it.
//
// The comment kept both halves of that history and contradicted itself — "one
// rendering" in one paragraph and "the bytes exist twice" in the next, crediting
// a regeneration to the Node test above, which regenerates nothing. One file,
// one author, one -update flag: this test.
//
//	go test ./www -run TestStationFlowspecFileMatchesGo -update
func TestStationFlowspecFileMatchesGo(t *testing.T) {
	path := filepath.Join("static", "operator-station", "flowspec-data.js")
	want, err := flowspec.ExportJSON()
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte(stationFlowspecHeader), want...)
	body = append(bytes.TrimSuffix(body, []byte("\n")), []byte(";\n")...)
	if *updateFlowspec {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s is missing; regenerate: go test ./www -run TestStationFlowspecFileMatchesGo -update", path)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("%s is stale against domain/flowspec.\n"+
			"  regenerate: go test ./www -run TestStationFlowspecFileMatchesGo -update", path)
	}
	if bytes.Contains(got, []byte("\r")) {
		t.Errorf("%s carries CR bytes; the repo is LF", path)
	}
}

// The station file's preamble. A classic script so composer-model.js can be
// loaded the same way by the browser and by its Node test.
const stationFlowspecHeader = `// flowspec-data.js — GENERATED from domain/flowspec by
// TestStationFlowspecFileMatchesGo. Do not edit: regenerate with
//   go test ./www -run TestStationFlowspecFileMatchesGo -update
// The Flow Composer reads window.FLOWSPEC to know which fields a choreography
// forbids, so a stale copy here is a cell the operator can still fill and the
// server then refuses.
window.FLOWSPEC = `
