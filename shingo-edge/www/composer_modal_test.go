package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestComposerModalJS runs the Node unit test for the station's pick-a-part
// modal in static/operator-station/composer-render.js — which screen each tap
// leads to (two off only with a two-produce recipe, a live style used
// silently, one recipe used, several asked by name, none → an engineer,
// Positions only with the switch on) and the part rows' words (Core's verdict
// pill, the style a part runs as, a CATID). Skipped if `node` is not on PATH
// (matches the other JS test wrappers).
//
// THE SPOTS STEP RUNS ON THE SERVER'S OWN CARD. Its taps look targets up on
// the card, so a hand-written card would pin the JS against a fixture the
// server never sends. The Press 6 fixture (handlers_flow_spots_test.go) is
// read through the station's own composer route and handed to the script as
// a file in the test's temp dir — never beside the script, where two runs of
// this package would race on it.
func TestComposerModalJS(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping JS unit tests")
	}
	f := newPress6(t)
	resp := doRequest(t, f.router, "GET", fmt.Sprintf("/api/operator-stations/%d/composer", f.station), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	var flow map[string]any
	must(t, json.NewDecoder(resp.Body).Decode(&flow), "decode the composer read")
	blob, err := json.Marshal(flow)
	must(t, err, "marshal the composer read")
	press6Path := filepath.Join(t.TempDir(), "press6.json")
	must(t, os.WriteFile(press6Path, blob, 0o644), "write the composer read")
	scriptPath := filepath.Join("static", "operator-station", "composer-modal.test.js")
	out, err := exec.Command(nodePath, scriptPath, press6Path).CombinedOutput()
	if err != nil {
		t.Fatalf("composer modal JS test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("composer modal: %s", out)
}
