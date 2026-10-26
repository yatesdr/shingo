package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol/testutil"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// handlers_flow_presets_test.go — the three endpoints, and the two rules that
// are the handler's rather than the service's: the session is the author, and
// a body has to name exactly one source for the shape.

func presetProcess(t *testing.T, name string) (int64, int64) {
	t.Helper()
	pid := seedProcess(t, name)
	if _, err := testDB.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "PP_01", Name: "PP_01", Enabled: true,
	}); err != nil {
		t.Fatalf("create position: %v", err)
	}
	if _, err := testDB.UpsertRoutingNode(domain.CoreNodeKinds{}, domain.RoutingNodeInput{
		ProcessID: pid, CoreNodeName: "PP-SRC", Role: domain.RoutingRoleSource, Enabled: true,
	}); err != nil {
		t.Fatalf("routing source: %v", err)
	}
	if _, err := testDB.UpsertRoutingNode(domain.CoreNodeKinds{}, domain.RoutingNodeInput{
		ProcessID: pid, CoreNodeName: "PP-DST", Role: domain.RoutingRoleDestination, Enabled: true,
	}); err != nil {
		t.Fatalf("routing destination: %v", err)
	}
	sid := seedStyle(t, name+"-style", pid)
	if _, err := testDB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "PP_01", Role: "consume", SwapMode: "two_robot",
		PayloadCode: "PP-PART", InboundStaging: "PP_01",
		InboundSource: "PP-SRC", OutboundDestination: "PP-DST",
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	return pid, sid
}

// TestFlowPresets_ARepeatedNameIsAVersionOnlyOfTheSameShape pins owner ruling
// F2 (2026-09-12) as the owner re-keyed it on 2026-10-22 (spots v0, Q2),
// through the real router.
//
// A repeated name is version n+1, which is what versions are FOR — a recipe
// changing, with every member claim's source_preset_version recording which
// one it came from. It is wrong when the recipes are different THINGS: R6's
// rename moves every version of a name, so two lineages sharing one name
// could never be pulled apart afterwards.
//
// WHAT A RECIPE IS, for its name, is its structure: per cell the swap mode,
// the role and which fields are filled, and the slot count. Never its node
// names. A NAMED BEHAVIOUR CHANGE: the same structure on other nodes was
// refused as "already a different shape", so Springfield Press 6's "Two spot
// press index" could not be re-saved with ULN_005 for a deleted ULN_003
// under its own name, and every fix restarted the pre-fill's lineage.
func TestFlowPresets_ARepeatedNameIsAVersionOnlyOfTheSameShape(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid, sameShape := presetProcess(t, "PRESET-NAME")

	// A SECOND STYLE, THE SAME STRUCTURE ON A SECOND POSITION.
	if _, err := testDB.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "PP_02", Name: "PP_02", Enabled: true,
	}); err != nil {
		t.Fatalf("create the second position: %v", err)
	}
	otherNodes := seedStyle(t, "PRESET-NAME-other", pid)
	if _, err := testDB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: otherNodes, CoreNodeName: "PP_02", Role: "consume", SwapMode: "two_robot",
		PayloadCode: "PP-PART-2", InboundStaging: "PP_02",
		InboundSource: "PP-SRC", OutboundDestination: "PP-DST",
	}); err != nil {
		t.Fatalf("seed the second claim: %v", err)
	}
	// A THIRD STYLE OF ANOTHER STRUCTURE: both positions, two slots.
	twoSlots := seedStyle(t, "PRESET-NAME-pair", pid)
	for _, n := range []string{"PP_01", "PP_02"} {
		if _, err := testDB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: twoSlots, CoreNodeName: n, Role: "consume", SwapMode: "two_robot",
			PayloadCode: "PP-PAIR-" + n, InboundStaging: n,
			InboundSource: "PP-SRC", OutboundDestination: "PP-DST",
		}); err != nil {
			t.Fatalf("seed the pair claim %s: %v", n, err)
		}
	}

	create := func(name string, styleID int64) *http.Response {
		return doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/presets",
			map[string]any{"name": name, "from_style_id": styleID}, cookie)
	}
	versions := func() []int {
		t.Helper()
		rows, err := testDB.ListFlowPresets(pid, true)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := make([]int, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Version)
		}
		return out
	}

	assertStatus(t, create("index", sameShape), http.StatusOK)
	// THE SAME RECIPE AGAIN IS v2, as it always was.
	assertStatus(t, create("index", sameShape), http.StatusOK)
	if got := versions(); len(got) != 2 {
		t.Fatalf("the same shape under the same name gave %v, want two versions", got)
	}
	// ITS NODES CHANGED, ITS STRUCTURE NOT: v3, under its own name.
	assertStatus(t, create("index", otherNodes), http.StatusOK)
	if got := versions(); len(got) != 3 || got[0] != 3 {
		t.Fatalf("the same structure on other nodes gave %v, want v3 of the same name", got)
	}

	// A DIFFERENT SLOT SHAPE UNDER THAT NAME IS REFUSED, BY NAME.
	resp := create("index", twoSlots)
	assertStatus(t, resp, http.StatusBadRequest)
	msg, _ := decodeBody(t, resp)["error"].(string)
	for _, want := range []string{"index", "already a different shape", "pick another name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not say %q: %q", want, msg)
		}
	}
	// AND NOTHING WAS WRITTEN. A refused name must not leave a version behind.
	if got := versions(); len(got) != 3 {
		t.Errorf("the refused create left something: %v", got)
	}

	// The other structure under its OWN name is fine — the guard is about the
	// name, not about the press running two recipes.
	assertStatus(t, create("pair", twoSlots), http.StatusOK)
	if got := versions(); len(got) != 4 {
		t.Errorf("the second structure under its own name was refused: %v", got)
	}
}

// TestFlowPresets_CreateReadArchive walks the three endpoints as the tab does.
func TestFlowPresets_CreateReadArchive(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid, sid := presetProcess(t, "PRESET-CRUD")

	// The read on a press with no presets still offers its shape.
	resp := doRequest(t, router, "GET", "/api/processes/"+itoa(pid)+"/presets", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	if got, _ := body["presets"].([]any); len(got) != 0 {
		t.Errorf("presets = %v, want none yet", got)
	}
	cands, _ := body["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want the one shape this press runs", len(cands))
	}
	cand, _ := cands[0].(map[string]any)
	key, _ := cand["shape_key"].(string)
	if key == "" {
		t.Fatal("the candidate carries no shape_key, so naming it has nothing to match")
	}
	// THE MODE WORD ALONE (owner ruling R5, 2026-09-12). The positions are not
	// in the name: every card and row draws them off the SHAPE, beside
	// whatever the preset ends up called, so a name carrying them said them
	// twice and said them wrong the moment an engineer typed their own.
	if name, _ := cand["suggested_name"].(string); name != "2‑Robot Swap" {
		t.Errorf("suggested name = %q, want the mode word alone", name)
	}
	if _, ok := cand["shape"]; !ok {
		t.Error("the candidate carries no shape, so the row has no positions to draw beside the name")
	}

	// Create from the candidate. created_by is the SESSION's, never the body's.
	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/presets",
		map[string]any{"name": "The one shape", "from_candidate_shape": key, "created_by": "not me"}, cookie)
	assertStatus(t, resp, http.StatusOK)
	newID := int64(decodeBody(t, resp)["id"].(float64))

	p, err := testDB.GetFlowPreset(newID)
	if err != nil {
		t.Fatalf("GetFlowPreset: %v", err)
	}
	if p.CreatedBy != "testadmin" {
		t.Errorf("created_by = %q, want the session user — a body cannot name the author", p.CreatedBy)
	}
	if p.Version != 1 {
		t.Errorf("version = %d, want 1", p.Version)
	}
	if strings.Contains(p.FlowJSON, "PP-PART") {
		t.Errorf("the stored shape carries the part: %s", p.FlowJSON)
	}

	// The read now lists it, with its member, in step, and offers the shape no
	// longer.
	resp = doRequest(t, router, "GET", "/api/processes/"+itoa(pid)+"/presets", nil, cookie)
	body = decodeBody(t, resp)
	presets, _ := body["presets"].([]any)
	if len(presets) != 1 {
		t.Fatalf("presets = %d after creating one", len(presets))
	}
	row, _ := presets[0].(map[string]any)
	if members, _ := row["members"].([]any); len(members) != 1 {
		t.Errorf("members = %v, want the one style that runs the shape", row["members"])
	}
	if drifted, _ := row["drifted"].([]any); len(drifted) != 0 {
		t.Errorf("drifted = %v on a freshly named shape", drifted)
	}
	if cands, _ := body["candidates"].([]any); len(cands) != 0 {
		t.Errorf("candidates = %v; a named shape is not still on offer", cands)
	}

	// A second preset of the same name is version 2 — a preset is never edited
	// in place, because claims record which version they came from.
	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/presets",
		map[string]any{"name": "The one shape", "from_style_id": sid}, cookie)
	assertStatus(t, resp, http.StatusOK)
	p2, err := testDB.GetFlowPreset(int64(decodeBody(t, resp)["id"].(float64)))
	testutil.MustNoErr(t, err, "testDB.GetFlowPreset")
	if p2.Version != 2 {
		t.Errorf("second preset of that name is v%d, want v2", p2.Version)
	}

	// Archive hides it.
	resp = doRequest(t, router, "POST",
		"/api/processes/"+itoa(pid)+"/presets/"+itoa(newID)+"/archive", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	after, err := testDB.GetFlowPreset(newID)
	testutil.MustNoErr(t, err, "testDB.GetFlowPreset")
	if after.ArchivedAt == nil {
		t.Error("archived_at was not stamped")
	}
	resp = doRequest(t, router, "GET", "/api/processes/"+itoa(pid)+"/presets", nil, cookie)
	for _, p := range decodeBody(t, resp)["presets"].([]any) {
		if int64(p.(map[string]any)["id"].(float64)) == newID {
			t.Error("an archived preset is still listed")
		}
	}
}

// TestFlowPresets_CreateRefusals: the body has to name exactly one source, and
// a name is required. Both are the handler's, not the service's.
func TestFlowPresets_CreateRefusals(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid, sid := presetProcess(t, "PRESET-REFUSE")

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"no name", map[string]any{"from_style_id": sid}},
		{"neither source", map[string]any{"name": "x"}},
		{"both sources", map[string]any{"name": "x", "from_style_id": sid, "from_candidate_shape": "k"}},
	} {
		resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/presets", tc.body, cookie)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, resp.StatusCode)
		}
	}

	// A style with no flow has no shape to name — a 400 that says so, not a
	// preset with no cells.
	empty := seedStyle(t, "PRESET-REFUSE-empty", pid)
	resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/presets",
		map[string]any{"name": "nothing", "from_style_id": empty}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)

	// A candidate key nobody runs any more: the offer was read before somebody
	// else edited the flow. Refused by name so the tab can say "re-read".
	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/presets",
		map[string]any{"name": "stale", "from_candidate_shape": "not-a-shape"}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
}

// TestFlowPresets_ThereIsNoApplyEndpoint. The one architectural property of
// this feature: a preset cannot change a style except through flow/save, which
// cannot run without a preview's fingerprint. An apply endpoint would be a
// second way to write claims, and the one that skipped the diff.
func TestFlowPresets_ThereIsNoApplyEndpoint(t *testing.T) {
	_, router := newAdminRouter(t)
	h2, _ := newAdminRouter(t)
	cookie := authCookie(t, h2)
	pid, _ := presetProcess(t, "PRESET-NOAPPLY")
	for _, path := range []string{
		"/api/processes/" + itoa(pid) + "/presets/1/apply",
		"/api/processes/" + itoa(pid) + "/presets/apply",
	} {
		resp := doRequest(t, router, "POST", path, map[string]any{"style_ids": []int64{1}}, cookie)
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d — there is no apply endpoint, and a preset applies through flow/save", path, resp.StatusCode)
		}
	}
}

// desktopBuilder runs one expression over desktop-bodies.js (bound as B) and a
// fixture (bound as f) under node and returns what it built — the body the
// page sends, never a hand-built one (pick-a-part brief §8, rows 11 and 12).
func desktopBuilder(t *testing.T, expr string, fixture map[string]any) map[string]any {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping a pin written from the desktop's builder")
	}
	blob, err := json.Marshal(fixture)
	testutil.MustNoErr(t, err, "marshal fixture")
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixture.json")
	testutil.MustNoErr(t, os.WriteFile(fixturePath, blob, 0o644), "write fixture")
	modulePath, err := filepath.Abs(filepath.Join("static", "js", "pages", "desktop-bodies.js"))
	testutil.MustNoErr(t, err, "resolve desktop-bodies.js")
	harness := fmt.Sprintf("'use strict';\nconst B = require(%q);\nconst f = require(%q);\nprocess.stdout.write(JSON.stringify(%s));\n",
		filepath.ToSlash(modulePath), filepath.ToSlash(fixturePath), expr)
	harnessPath := filepath.Join(dir, "emit.js")
	testutil.MustNoErr(t, os.WriteFile(harnessPath, []byte(harness), 0o644), "write harness")
	raw, err := exec.Command(nodePath, harnessPath).Output()
	if err != nil {
		t.Fatalf("run desktop-bodies.js: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode body (%s): %v", raw, err)
	}
	return out
}

// TestFlowPresets_AHandBuiltDraftIsNamedFromThePage (pick-a-part U2): a
// recipe built by hand in the positions table is named through the desktop's
// own create body — built by the page's builder — and stored as drawn,
// authored by the session.
func TestFlowPresets_AHandBuiltDraftIsNamedFromThePage(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid, _ := presetProcess(t, "PRESET-NEW")
	if _, err := testDB.UpsertRoutingNode(domain.CoreNodeKinds{}, domain.RoutingNodeInput{
		ProcessID: pid, CoreNodeName: "PP-STG", Role: domain.RoutingRoleStaging, Enabled: true,
	}); err != nil {
		t.Fatalf("routing staging: %v", err)
	}
	shape := []domain.FlowCell{{CoreNodeName: "PP_01", Role: "consume", SwapMode: "two_robot",
		InboundStaging: "PP-STG", OutboundDestination: "PP-DST"}}
	var view struct {
		Presets []struct {
			Name      string            `json:"name"`
			CreatedBy string            `json:"created_by"`
			Shape     []domain.FlowCell `json:"shape"`
		} `json:"presets"`
	}

	body := desktopBuilder(t, "B.flowPresetCreate('Standard', { cells: f.cells })", map[string]any{"cells": shape})
	resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/presets", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	resp = doRequest(t, router, "GET", "/api/processes/"+itoa(pid)+"/presets", nil, cookie)
	decodeJSON(t, resp, &view)
	if len(view.Presets) != 1 || view.Presets[0].Name != "Standard" || view.Presets[0].CreatedBy != "testadmin" {
		t.Fatalf("presets = %+v, want one named Standard by the session user", view.Presets)
	}
	if domain.FlowShapeKey(view.Presets[0].Shape) != domain.FlowShapeKey(shape) {
		t.Errorf("stored shape %+v, want the drawn one %+v", view.Presets[0].Shape, shape)
	}
}
