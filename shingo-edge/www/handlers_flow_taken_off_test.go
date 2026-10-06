package www

import (
	"net/http"
	"strings"
	"testing"

	"shingo/shared/scenefixtures"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// handlers_flow_taken_off_test.go — a part taken off a saved flow is not a
// lost part, through the real engine and the real router.
//
// The stored flow runs a part on a position. The draft puts a different part
// on that position, so the stored part sits on none. Named in the request's
// taken_off, it is a part the engineer took off this flow: the preview raises
// nothing about it and the save leaves the style without it. Not named, it is
// a part that lost its position by accident, and the preview says so exactly
// as it always has — and the save, which never refused an unplaced part,
// still saves.

// takenOffFixture is a fresh plant with the running style's stored claims and
// a draft that leaves the first claim's part on no position.
type takenOffFixture struct {
	db        *store.DB
	processID int64
	styleID   int64
	stationID int64
	part      string
	cells     []domain.FlowCell
}

func newTakenOffFixture(t *testing.T) takenOffFixture {
	t.Helper()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	if err := db.SetFlowComposerEnabled(seeded.ProcessID, true); err != nil {
		t.Fatalf("open the gate: %v", err)
	}
	process, err := db.GetProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if process.ActiveStyleID == nil {
		t.Fatal("the fixture's press is running nothing")
	}
	styleID := *process.ActiveStyleID
	stored, err := db.ListStyleNodeClaims(styleID)
	if err != nil {
		t.Fatalf("ListStyleNodeClaims: %v", err)
	}
	// THE PART TO LOSE, AND ITS REPLACEMENT: the first stored claim whose part
	// no other claim runs, re-pointed at a part another claim does run. The set
	// of positions is unchanged, so the running style's position gate has
	// nothing to say, and the dropped part is on no position at all.
	runs := map[string]int{}
	for _, c := range stored {
		runs[c.PayloadCode]++
	}
	lose, other := -1, ""
	for i, c := range stored {
		if lose < 0 && c.PayloadCode != "" && runs[c.PayloadCode] == 1 {
			lose = i
		}
	}
	for i, c := range stored {
		if i != lose && c.PayloadCode != "" && lose >= 0 && c.PayloadCode != stored[lose].PayloadCode {
			other = c.PayloadCode
			break
		}
	}
	if lose < 0 || other == "" {
		t.Fatalf("the running style has no part on one position only with another part beside it: %+v", stored)
	}
	var cells []domain.FlowCell
	for i, c := range stored {
		cell := domain.Collapse(c)
		if i == lose {
			cell.PayloadCode = other
		}
		cells = append(cells, cell)
	}
	return takenOffFixture{
		db: db, processID: seeded.ProcessID, styleID: styleID,
		stationID: seeded.Stations["screen-a4"], part: stored[lose].PayloadCode, cells: cells,
	}
}

func (f takenOffFixture) path(verb string) string {
	return "/api/processes/" + itoa(f.processID) + "/flow/" + verb
}

// partFinding is the unplaced-part finding's message, or "" when the preview
// raised none.
func partFinding(t *testing.T, preview map[string]any) string {
	t.Helper()
	list, ok := preview["findings"].([]any)
	if !ok {
		t.Fatalf("the preview carries no findings list: %v", preview)
	}
	for _, raw := range list {
		f, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if msg, ok := f["message"].(string); ok && f["core_node_name"] == "" && strings.Contains(msg, "need") {
			return msg
		}
	}
	return ""
}

func TestFlowPreview_APartTakenOffIsNotLost(t *testing.T) {
	f := newTakenOffFixture(t)
	_, router := realFlowRouter(t, f.db)

	resp := doRequest(t, router, "POST", f.path("preview"),
		map[string]any{"to_style_id": f.styleID, "cells": f.cells, "taken_off": []string{f.part}}, nil)
	assertStatus(t, resp, http.StatusOK)
	if msg := partFinding(t, flowBody(t, resp)); msg != "" {
		t.Errorf("%s was taken off this flow and the preview still says %q", f.part, msg)
	}

	resp = doRequest(t, router, "POST", f.path("preview"),
		map[string]any{"to_style_id": f.styleID, "cells": f.cells}, nil)
	assertStatus(t, resp, http.StatusOK)
	if got, want := partFinding(t, flowBody(t, resp)), "1 part needs a position: "+f.part; got != want {
		t.Errorf("a part dropped without being taken off: the preview says %q, want %q", got, want)
	}
}

func TestFlowSave_APartTakenOffLeavesTheStyle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		takenOff bool
	}{
		{"taken off", true},
		// Unchanged by the ruling: the save never refused an unplaced part.
		{"dropped without being taken off", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTakenOffFixture(t)
			_, router := realFlowRouter(t, f.db)
			preview := map[string]any{"to_style_id": f.styleID, "cells": f.cells}
			save := map[string]any{"to_style_id": f.styleID, "cells": f.cells, "station_id": f.stationID}
			if tc.takenOff {
				preview["taken_off"] = []string{f.part}
				save["taken_off"] = []string{f.part}
			}
			resp := doRequest(t, router, "POST", f.path("preview"), preview, nil)
			assertStatus(t, resp, http.StatusOK)
			fp, ok := flowBody(t, resp)["fingerprint"].(string)
			if !ok || fp == "" {
				t.Fatal("the preview returned no fingerprint")
			}
			save["fingerprint"] = fp

			resp = doRequest(t, router, "POST", f.path("save"), save, nil)
			assertStatus(t, resp, http.StatusOK)

			after, err := f.db.ListStyleNodeClaims(f.styleID)
			if err != nil {
				t.Fatalf("ListStyleNodeClaims after: %v", err)
			}
			if runsPart(after, f.part) {
				t.Errorf("the saved flow still runs %s: %+v", f.part, after)
			}
		})
	}
}

func runsPart(claims []processes.NodeClaim, part string) bool {
	for _, c := range claims {
		if c.PayloadCode == part {
			return true
		}
	}
	return false
}
