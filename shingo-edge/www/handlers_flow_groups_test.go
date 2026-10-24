package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingoedge/domain"
)

// handlers_flow_groups_test.go — a recipe's groups, saved from the desktop's
// own body and read back on both composer reads (recipe groups, 2026-10-24).
//
// SYNTH-round2 §2 is the recipe: Press 6's "Two spot press index" re-saved
// with Left Hand on PLN_006 or PLN_010 (deck PLN_005) and Right Hand on
// PLN_007 or PLN_012 (deck PLN_008, its own reorder point). The cells travel
// as they always have; the groups ride beside them, untouched, and a group's
// Advanced comes back on the desktop's read because the mint reads it.

// engineer is a desktop session on the fixture's own router: the recipe
// routes are admin-gated, and the session is the author.
func (f press6) engineer(t *testing.T) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/login-dummy", nil)
	w := httptest.NewRecorder()
	f.h.sessions.setUser(w, req, "engineer")
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie")
	}
	return cookies[0]
}

// press6Groups is SYNTH-round2 §2's groups on this fixture.
func press6Groups() []map[string]any {
	return []map[string]any{
		{"of": "PLN_006", "name": "Left Hand", "spots": []map[string]any{
			{"core_node_name": "PLN_006", "paired_core_node": []string{"PLN_005"}},
			{"core_node_name": "PLN_010", "paired_core_node": []string{"PLN_005"}}}},
		{"of": "PLN_007", "name": "Right Hand", "spots": []map[string]any{
			{"core_node_name": "PLN_007", "paired_core_node": []string{"PLN_008"}},
			{"core_node_name": "PLN_012", "paired_core_node": []string{"PLN_008"}}},
			"advanced": map[string]any{"reorder_point": 1, "auto_reorder": true}},
	}
}

// saveRecipe posts the desktop's recipe-create body for cells and groups,
// built by desktop-bodies.js flowPresetCreate under node.
func (f press6) saveRecipe(t *testing.T, cookie *http.Cookie, name string, cells []domain.FlowCell, groups any) *http.Response {
	t.Helper()
	body := desktopBuilder(t, "B.flowPresetCreate(f.name, { cells: f.cells, groups: f.groups })",
		map[string]any{"name": name, "cells": cells, "groups": groups})
	return doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/presets", body, cookie)
}

// desktopCard is the desktop composer read's card for a recipe.
func (f press6) desktopCard(t *testing.T, cookie *http.Cookie, preset int64) map[string]any {
	t.Helper()
	resp := doRequest(t, f.router, "GET", "/api/processes/"+itoa(f.processID)+"/composer", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var data struct {
		Presets []map[string]any `json:"presets"`
	}
	must(t, json.NewDecoder(resp.Body).Decode(&data), "decode the desktop composer")
	for _, p := range data.Presets {
		if p["id"] == fmt.Sprint(preset) {
			return p
		}
	}
	t.Fatalf("the desktop composer read carries no card for recipe %d", preset)
	return nil
}

var press6TwoSpotCells = []domain.FlowCell{
	cell("PLN_006", "PLN_005", "SMN_BUF_100", "ULN_002"),
	cell("PLN_007", "PLN_008", "SMN_BUF_100", "ULN_005"),
}

// TestFlowPresets_AGroupsAdvancedRoundTrips: the engineer's Set up groups,
// saved under the recipe's own name, is v2 (the cells' structure is the
// version key, and groups leave it alone); the desktop reads the groups back
// with Right Hand's Advanced, the station reads them without it (no reader
// there), and a second edit of the groups alone is v3.
func TestFlowPresets_AGroupsAdvancedRoundTrips(t *testing.T) {
	f := newPress6(t)
	cookie := f.engineer(t)

	resp := f.saveRecipe(t, cookie, "Two spot press index", press6TwoSpotCells, press6Groups())
	assertStatus(t, resp, http.StatusOK)
	id := int64(decodeBody(t, resp)["id"].(float64))
	p, err := f.db.GetFlowPreset(id)
	must(t, err, "the grouped recipe")
	if p.Version != 2 {
		t.Fatalf("the grouped re-save is v%d, want v2 of the same name", p.Version)
	}

	got, err := json.Marshal(f.desktopCard(t, cookie, id)["groups"])
	must(t, err, "marshal the desktop's groups")
	const desktop = `[{"of":"PLN_006","name":"Left Hand","spots":[` +
		`{"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},` +
		`{"core_node_name":"PLN_010","paired_core_node":["PLN_005"]}]},` +
		`{"of":"PLN_007","name":"Right Hand","spots":[` +
		`{"core_node_name":"PLN_007","paired_core_node":["PLN_008"]},` +
		`{"core_node_name":"PLN_012","paired_core_node":["PLN_008"]}],` +
		`"advanced":{"auto_reorder":true,"reorder_point":1}}]`
	if !sameJSON(t, string(got), desktop) {
		t.Errorf("the desktop reads groups %s, want %s", got, desktop)
	}
	got, err = json.Marshal(f.card(t, id)["groups"])
	must(t, err, "marshal the station's groups")
	station := strings.Replace(desktop, `,"advanced":{"auto_reorder":true,"reorder_point":1}`, "", 1)
	if !sameJSON(t, string(got), station) {
		t.Errorf("the station reads groups %s, want %s (no Advanced: nothing on the station reads it)", got, station)
	}

	groups := press6Groups()
	groups[1]["spots"] = []map[string]any{{"core_node_name": "PLN_007", "paired_core_node": []string{"PLN_008"}}}
	resp = f.saveRecipe(t, cookie, "Two spot press index", press6TwoSpotCells, groups)
	assertStatus(t, resp, http.StatusOK)
	p, err = f.db.GetFlowPreset(int64(decodeBody(t, resp)["id"].(float64)))
	must(t, err, "the re-grouped recipe")
	if p.Version != 3 {
		t.Errorf("the groups changed and the cells did not: v%d, want v3", p.Version)
	}
}

// TestFlowPresets_AGroupThatBreaksTheOwnersRuleIsRefusedByName: the save's
// refusal reaches the engineer in the server's words, and nothing is written.
func TestFlowPresets_AGroupThatBreaksTheOwnersRuleIsRefusedByName(t *testing.T) {
	f := newPress6(t)
	cookie := f.engineer(t)
	groups := press6Groups()
	groups[1]["spots"] = []map[string]any{
		{"core_node_name": "PLN_007", "paired_core_node": []string{"PLN_008"}},
		{"core_node_name": "PLN_012", "paired_core_node": []string{"PLN_005"}}}
	resp := f.saveRecipe(t, cookie, "Two spot press index", press6TwoSpotCells, groups)
	const says = "PLN_005 is in both Left Hand and Right Hand — a node serves one group only"
	if msg, _ := decodeBody(t, resp)["error"].(string); resp.StatusCode != http.StatusBadRequest || msg != says {
		t.Errorf("save: %d %q, want 400 %q", resp.StatusCode, msg, says)
	}
	// Groups travel with a recipe's own cells and nowhere else: a style's
	// shape has none to be checked against.
	resp = doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/presets",
		map[string]any{"name": "From the running style", "from_style_id": f.running, "groups": press6Groups()}, cookie)
	if msg, _ := decodeBody(t, resp)["error"].(string); resp.StatusCode != http.StatusBadRequest || msg != "groups are saved with a recipe's cells" {
		t.Errorf("groups on a style's shape: %d %q, want 400", resp.StatusCode, msg)
	}
	rows, err := f.db.ListFlowPresets(f.processID, true)
	must(t, err, "list")
	if len(rows) != 2 {
		t.Errorf("%d recipes after two refused saves, want the fixture's 2", len(rows))
	}
}

// A group naming a node the cell has since lost is stale like a cell's own
// node (spots v0's "refuse and flag"): the card carries the sentence, and the
// mint refuses the recipe by it before anything is placed.
func TestFlowMint_AGroupNamingALostNodeIsRefusedUntilReSaved(t *testing.T) {
	f := newPress6(t)
	cookie := f.engineer(t)
	groups := press6Groups()
	groups[0]["spots"] = append(groups[0]["spots"].([]map[string]any),
		map[string]any{"core_node_name": "PLN_013", "paired_core_node": []string{"PLN_014"}})
	resp := f.saveRecipe(t, cookie, "Two spot press index", press6TwoSpotCells, groups)
	assertStatus(t, resp, http.StatusOK)
	id := int64(decodeBody(t, resp)["id"].(float64))
	must(t, f.db.DeleteProcessNode(f.nodes["PLN_013"]), "the cell loses PLN_013")

	const said = "Two spot press index runs Left Hand on PLN_013, which is no longer a position here — " +
		"an engineer re-saves the recipe on the desktop"
	if got := f.card(t, id)["stale"]; fmt.Sprint(got) != fmt.Sprint([]any{said}) {
		t.Errorf("the card's stale = %v, want [%s]", got, said)
	}
	bodies := mintBodies(t, f.station, map[string]mintArgs{"unmoved": {id, press6Pair, nil}})
	resp = doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/flow/mint", bodies["unmoved"], nil)
	if msg, _ := decodeBody(t, resp)["error"].(string); resp.StatusCode != http.StatusUnprocessableEntity || msg != said {
		t.Errorf("mint: %d %q, want 422 %q", resp.StatusCode, msg, said)
	}
}

func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	must(t, json.Unmarshal([]byte(a), &x), "decode "+a)
	must(t, json.Unmarshal([]byte(b), &y), "decode "+b)
	ra, err := json.Marshal(x)
	must(t, err, "marshal "+a)
	rb, err := json.Marshal(y)
	must(t, err, "marshal "+b)
	return string(ra) == string(rb)
}
