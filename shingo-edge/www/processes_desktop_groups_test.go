package www

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"shingoedge/domain"
)

// processes_desktop_groups_test.go — the deploy note's engineer steps, clicked
// on the Processes page over the server's own reads (recipe groups,
// 2026-10-24).
//
// THE NOTE, AS WRITTEN: right after the Edge comes up, an engineer opens "Two
// spot press index" on the desktop, presses Set up groups, adds PLN_010 to
// Left Hand and PLN_012 to Right Hand (PLN_005 and PLN_008 pre-fill under
// them), sets Right Hand's Out to ULN_005, and saves under the same name, which
// is v2. The page runs under node (processes-desktop.popover-acts.test.js,
// `--walk`) on the Press 6 fixture's desktop composer read, records the body it
// posts, and the body goes to the real router; then the saved version's read
// is handed back (`--floor`) and the page's "On the floor" block is read off
// the server's own card.

// desktopRead is the desktop composer read, written to a file for the script.
func (f press6) desktopRead(t *testing.T, cookie *http.Cookie, name string) string {
	t.Helper()
	resp := doRequest(t, f.router, "GET", "/api/processes/"+itoa(f.processID)+"/composer", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	blob, err := io.ReadAll(resp.Body)
	must(t, err, "read the desktop composer")
	p := filepath.Join(t.TempDir(), name)
	must(t, os.WriteFile(p, blob, 0o644), "write the desktop composer")
	return p
}

// desktopPage runs the Processes page's script in one of its modes over a
// composer read and returns what it wrote.
func desktopPage(t *testing.T, mode, read string) map[string]any {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the deploy note's walk")
	}
	out := filepath.Join(t.TempDir(), "page-out.json")
	script := filepath.Join("static", "js", "pages", "processes-desktop.popover-acts.test.js")
	if b, err := exec.Command(nodePath, script, mode, read, out).CombinedOutput(); err != nil {
		t.Fatalf("the page under node (%s) failed:\n%s\nerror: %v", mode, b, err)
	}
	raw, err := os.ReadFile(out)
	must(t, err, "read what the page wrote")
	var got map[string]any
	must(t, json.Unmarshal(raw, &got), "decode what the page wrote")
	return got
}

func TestDesktopRecipeGroups_TheDeployNoteWalks(t *testing.T) {
	f := newPress6(t)
	cookie := f.engineer(t)
	// Where the plant starts: the recipe sends both spots' old bins to
	// ULN_002, so the note's Out to is a change the card makes. The two
	// unloader windows are positions of the process's other station, never
	// routing rows (processes.ErrRoutingNodeIsPosition), and the routing set
	// holds no destination at all.
	shape, err := domain.PresetShapeJSON([]domain.FlowCell{
		cell("PLN_006", "PLN_005", "SMN_BUF_100", "ULN_002"),
		cell("PLN_007", "PLN_008", "SMN_BUF_100", "ULN_002"),
	})
	must(t, err, "the plant's recipe")
	_, err = f.db.Exec(`UPDATE flow_presets SET flow_json = ? WHERE id = ?`, shape, f.twoSpot)
	must(t, err, "store the plant's recipe")

	walked := desktopPage(t, "--walk", f.desktopRead(t, cookie, "v1.json"))
	body, _ := walked["body"].(map[string]any)
	if body == nil {
		t.Fatalf("the walk posted no recipe; the cards read %q", walked["groups"])
	}
	resp := doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/presets", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	id := int64(decodeBody(t, resp)["id"].(float64))
	p, err := f.db.GetFlowPreset(id)
	must(t, err, "the saved recipe")
	if p.Name != "Two spot press index" || p.Version != 2 {
		t.Fatalf("saved %q v%d, want Two spot press index v2", p.Name, p.Version)
	}
	cells, groups, err := domain.ParseRecipe(p.FlowJSON)
	must(t, err, "parse the saved recipe")
	outs := map[string]string{}
	for _, c := range cells {
		outs[c.CoreNodeName] = c.PairedCoreNode + " → " + c.OutboundDestination
	}
	if outs["PLN_006"] != "PLN_005 → ULN_002" || outs["PLN_007"] != "PLN_008 → ULN_005" || len(outs) != 2 {
		t.Errorf("saved cells %v, want PLN_006 ← PLN_005 to ULN_002 and PLN_007 ← PLN_008 to ULN_005", outs)
	}
	got, err := json.Marshal(groups)
	must(t, err, "marshal the saved groups")
	want := press6Groups()
	delete(want[1], "advanced")
	wantJSON, err := json.Marshal(want)
	must(t, err, "marshal SYNTH §2's groups")
	if !sameJSON(t, string(got), string(wantJSON)) {
		t.Errorf("saved groups %s\nwant %s", got, wantJSON)
	}

	floor := desktopPage(t, "--floor", f.desktopRead(t, cookie, "v2.json"))
	const says = "On the floor Left Hand runs on PLN_006 or PLN_010, on deck PLN_005 · " +
		"Right Hand on PLN_007 or PLN_012, on deck PLN_008 · nothing else is offered."
	if floor["floor"] != says || floor["cards"] != float64(2) {
		t.Errorf("v2 opens with %v cards and the block %q\nwant 2 cards and %q", floor["cards"], floor["floor"], says)
	}
}
