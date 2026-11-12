package www

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// The reorder-point stamp left the claim, the reads and the bodies; an Edge
// and a page on either side of a deploy still talk.
//
//   - An older page (a cached desktop build) still sends it: on the
//     replenishment page's PUT as "source", and in a flow save's Advanced
//     block as "reorder_point_source". Both bodies are taken and write the
//     point and Auto reorder they carry.
//   - An older page reading this Edge gets the reorder point and Auto reorder
//     it draws, and nothing it needs is missing (its own defaults stand in
//     for an absent key).
//   - Core never had it: the claim Edge sends Core names the reorder point
//     and no stamp, so a Core of either age reads the same message.
func TestReorderPoint_APeerEitherSideOfTheDeployStillTalks(t *testing.T) {
	f := newPress6(t)
	cookie := f.engineer(t)

	styles, err := f.db.ListStylesByProcess(f.processID)
	must(t, err, "styles")
	var idle int64
	for _, s := range styles {
		if s.Name == "SYN-ONE" {
			idle = s.ID
		}
	}
	if idle == 0 {
		t.Fatal("press 6 has no SYN-ONE style")
	}
	stored, err := f.db.GetStyleNodeClaimByNode(idle, "PLN_009")
	must(t, err, "SYN-ONE's claim")

	t.Run("an older replenishment page's Apply", func(t *testing.T) {
		resp := doRequest(t, f.router, "PUT", "/api/replenishment/cell-reorder", map[string]any{
			"claim_id": stored.ID, "reorder_point": 7, "source": "legacy", "auto_reorder": true,
		}, cookie)
		assertStatus(t, resp, http.StatusNoContent)
		c, err := f.db.GetStyleNodeClaim(stored.ID)
		must(t, err, "reread")
		if c.ReorderPoint != 7 || !c.AutoReorder {
			t.Errorf("after an older page's Apply: point %d auto %v, want 7 true", c.ReorderPoint, c.AutoReorder)
		}
	})

	t.Run("an older desktop's flow save", func(t *testing.T) {
		pv := doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/flow/preview",
			map[string]any{"to_style_id": idle}, cookie)
		fingerprint, _ := flowBody(t, pv)["fingerprint"].(string)
		if fingerprint == "" {
			t.Fatal("the preview returned no fingerprint")
		}
		c, err := f.db.GetStyleNodeClaim(stored.ID)
		must(t, err, "reread")
		raw, err := json.Marshal(domain.Collapse(*c))
		must(t, err, "marshal cell")
		var cellMap map[string]any
		must(t, json.Unmarshal(raw, &cellMap), "cell map")
		raw, err = json.Marshal(domain.AdvancedOf(c))
		must(t, err, "marshal advanced")
		var adv map[string]any
		must(t, json.Unmarshal(raw, &adv), "advanced map")
		adv["reorder_point"], adv["auto_reorder"], adv["reorder_point_source"] = 40, false, "manual"
		cellMap["advanced"] = adv
		resp := doRequest(t, f.router, "POST", "/api/processes/"+itoa(f.processID)+"/flow/save", map[string]any{
			"to_style_id": idle, "cells": []any{cellMap}, "fingerprint": fingerprint, "station_id": f.station,
		}, cookie)
		assertStatus(t, resp, http.StatusOK)
		after, err := f.db.GetStyleNodeClaimByNode(idle, "PLN_009")
		must(t, err, "reread")
		if after.ReorderPoint != 40 || after.AutoReorder {
			t.Errorf("after an older desktop's save: point %d auto %v, want 40 false", after.ReorderPoint, after.AutoReorder)
		}
	})

	t.Run("an older page reading this Edge", func(t *testing.T) {
		resp := doRequest(t, f.router, "GET", "/api/processes/"+itoa(f.processID)+"/composer", nil, cookie)
		assertStatus(t, resp, http.StatusOK)
		var data struct {
			Styles []struct {
				ID       int64                     `json:"id"`
				Advanced map[string]map[string]any `json:"advanced"`
			} `json:"styles"`
		}
		must(t, json.NewDecoder(resp.Body).Decode(&data), "decode the desktop composer")
		var adv map[string]any
		for _, s := range data.Styles {
			if s.ID == idle {
				adv = s.Advanced["PLN_009"]
			}
		}
		if adv == nil {
			t.Fatalf("the desktop read carries no Advanced block for SYN-ONE's PLN_009")
		}
		if adv["reorder_point"] != float64(40) {
			t.Errorf("the read's reorder point = %v, want 40", adv["reorder_point"])
		}
		if v, has := adv["auto_reorder"]; has && v != false {
			t.Errorf("the read's Auto reorder = %v, want off (absent or false)", v)
		}
	})

	t.Run("Core's copy of a claim", func(t *testing.T) {
		raw, err := json.Marshal(protocol.PlantClaim{ReorderPoint: 40})
		must(t, err, "marshal")
		if s := string(raw); !strings.Contains(s, `"reorder_point":40`) || strings.Contains(s, "reorder_point_source") {
			t.Errorf("the claim Edge sends Core reads %s; want the reorder point and no stamp", s)
		}
	})
}
