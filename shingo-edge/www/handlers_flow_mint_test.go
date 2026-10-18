package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store"
)

// handlers_flow_mint_test.go — the gate, pinned from the station's own bodies.
//
// WHY THE BODY COMES FROM composer-model.js. The R3 carve-out (09-17) was
// pinned only by Go tests that hand-built the request, and the station's save
// never carried the field the carve-out needs — so the one door the floor had
// on a gated-off cell refused every request it was ever sent, for a month, with
// every test green. A hand-written body is the correct body by construction;
// the station's is the one that ships. So these run the station's builders
// under node and post what they return (pick-a-part brief §8, rows 11 and 12).
//
// Skipped without node, like every other JS-backed pin in this package.

// stationBodies runs composer-model.js's body builders over a fixture written
// from Go and returns what they built, keyed by name.
func stationBodies(t *testing.T, fixture map[string]any) map[string]map[string]any {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the station body pins")
	}
	blob, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(fixturePath, blob, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	modulePath, err := filepath.Abs(filepath.Join("static", "operator-station", "composer-model.js"))
	if err != nil {
		t.Fatalf("resolve composer-model.js: %v", err)
	}
	harness := fmt.Sprintf(`
'use strict';
const M = require(%q);
const f = require(%q);
process.stdout.write(JSON.stringify({
  'save': M.flowSaveBody(f.styleID, f.cells, f.fingerprint, f.stationID, []),
  'mint': M.mintBody(f.presetID, f.codes, f.stationID),
}));
`, filepath.ToSlash(modulePath), filepath.ToSlash(fixturePath))
	harnessPath := filepath.Join(dir, "emit.js")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o644); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	raw, err := exec.Command(nodePath, harnessPath).Output()
	if err != nil {
		t.Fatalf("run composer-model.js: %v", err)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode bodies (%s): %v", raw, err)
	}
	return out
}

// gatedOffPlant seeds plant A's press with its routing set and the gate SHUT,
// which is what every process Add-process makes now looks like (brief U0).
func gatedOffPlant(t *testing.T) (*store.DB, testdb.SeededPlant) {
	t.Helper()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	if err := db.SetFlowComposerEnabled(seeded.ProcessID, false); err != nil {
		t.Fatalf("shut the gate: %v", err)
	}
	return db, seeded
}

// TestFlowSave_GatedOffStationBodyIsRefused is the statement of today: the
// station's save body, edited flow and all, is a 403 on a cell whose gate is
// shut. The floor does not author flows there, and nothing the station sends
// says otherwise.
func TestFlowSave_GatedOffStationBodyIsRefused(t *testing.T) {
	db, seeded := gatedOffPlant(t)
	h, router := realFlowRouter(t, db)
	_ = h

	process, err := db.GetProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	// A style that is NOT running and HAS a flow: the edit the gate exists to
	// refuse. (The running style would be refused for moving, a different 409.)
	var styleID int64
	for _, id := range seeded.Styles {
		if process.ActiveStyleID != nil && *process.ActiveStyleID == id {
			continue
		}
		claims, err := db.ListStyleNodeClaims(id)
		if err != nil {
			t.Fatalf("ListStyleNodeClaims: %v", err)
		}
		if len(claims) > 0 {
			styleID = id
			break
		}
	}
	if styleID == 0 {
		t.Fatal("plant A has no idle style with a flow — this test has nothing to edit")
	}
	stored, err := db.ListStyleNodeClaims(styleID)
	if err != nil {
		t.Fatalf("ListStyleNodeClaims: %v", err)
	}
	var cells []domain.FlowCell
	for _, c := range stored {
		cells = append(cells, domain.Collapse(c))
	}
	pid := itoa(seeded.ProcessID)
	pv := doRequest(t, router, "POST", "/api/processes/"+pid+"/flow/preview",
		map[string]any{"to_style_id": styleID}, nil)
	fingerprint, _ := flowBody(t, pv)["fingerprint"].(string)
	if fingerprint == "" {
		t.Fatal("the preview returned no fingerprint; the save would be refused as stale, not gated")
	}

	bodies := stationBodies(t, map[string]any{
		"styleID": styleID, "cells": cells, "fingerprint": fingerprint,
		"stationID": seeded.Stations["screen-a4"],
	})
	resp := doRequest(t, router, "POST", "/api/processes/"+pid+"/flow/save", bodies["save"], nil)
	assertStatus(t, resp, http.StatusForbidden)
	if _, has := bodies["save"]["source_preset_id"]; has {
		t.Error("the station's save body names a preset; the gate's carve-out reading is no longer the statement of today")
	}
}

// TestFlowMint_GatedOffStationBodyPassesTheGate is the door U3 builds and the
// reason U0 leaves engine.gateRefusal alone (brief O-10, error row 12): the
// station's mint body — a recipe and the parts the operator picked, no cells,
// no moves — is the floor's one write on a gated-off cell. It must not be a
// 403. Red until U3 lands POST /flow/mint.
func TestFlowMint_GatedOffStationBodyPassesTheGate(t *testing.T) {
	t.Skip("red until U3 lands POST /processes/{id}/flow/mint (brief O-10); U3 removes this skip")
	db, seeded := gatedOffPlant(t)
	_, router := realFlowRouter(t, db)
	presetID, code := firstRecipeAndPart(t, db, seeded)
	bodies := stationBodies(t, map[string]any{
		"presetID": presetID, "codes": []string{code},
		"stationID": seeded.Stations["screen-a4"],
	})
	if _, has := bodies["mint"]["cells"]; has {
		t.Fatal("the station's mint body carries cells; the mint is its own request type (brief O-7)")
	}
	resp := doRequest(t, router, "POST", "/api/processes/"+itoa(seeded.ProcessID)+"/flow/mint", bodies["mint"], nil)
	assertStatus(t, resp, http.StatusOK)
}

// firstRecipeAndPart names plant A's offered shapes as recipes and returns the
// first one with a part the press already runs on it.
func firstRecipeAndPart(t *testing.T, db *store.DB, seeded testdb.SeededPlant) (int64, string) {
	t.Helper()
	t.Fatal("written in U3, beside the mint")
	return 0, ""
}
