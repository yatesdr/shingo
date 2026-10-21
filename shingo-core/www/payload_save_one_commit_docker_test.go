//go:build docker

package www

import (
	"net/http"
	"testing"
)

// payload_save_one_commit_docker_test.go — a template API save of a payload
// commits its row, its carrier types and its manifest together or not at all.
// A refusal found in the last of the three (a cat-id conflict, which only the
// manifest write can see) used to leave the first two saved: an update was
// reported failed with its row and carriers changed, and a create left a
// payload with no parts whose retry hit a duplicate code.

func TestPayloadSave_UpdateCommitsTogetherOrNotAtAll(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	id, a, b, c := seedDoorPayload(t, h, db, "PLD-ONE")
	before := payloadRowJSON(t, db, id)
	lines := payloadManifestJSON(t, db, id, true)

	// PLD-ONE-P1 is known with cat id 70000001; naming it with another is a
	// question for the operator, refused as a conflict.
	rec := postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update", map[string]any{
		"id": id, "code": "PLD-ONE-RENAMED", "description": "changed", "uop_capacity": 60, "robot_group": "RG-9",
		"tote_layers": 3, "uop_per_tote": 4,
		"bin_type_ids": []int64{c},
		"manifest":     []map[string]any{{"part_number": "PLD-ONE-P1", "catid": "79999999", "parts_per_cycle": 2}},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("update naming a known part with another cat id: status %d, want 409; body=%s",
			rec.Code, rec.Body.String())
	}
	requireSame(t, "row after a refused update", payloadRowJSON(t, db, id), before)
	requireSame(t, "carriers after a refused update", payloadBinTypeIDs(t, db, id), []int64{a, b})
	requireSame(t, "lines after a refused update (ids included)", payloadManifestJSON(t, db, id, true), lines)
}

func TestPayloadSave_CreateCommitsTogetherOrNotAtAll(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	seedDoorPayload(t, h, db, "PLD-TWO")
	bare := mintBareType(t, db, "PLD-TWO-CART")

	for _, tc := range []struct {
		name string
		code string
		body map[string]any
		want int
	}{
		{"a known part under another cat id", "PLD-TWO-NEW", map[string]any{
			"manifest": []map[string]any{{"part_number": "PLD-TWO-P1", "catid": "79999999", "parts_per_cycle": 1}},
		}, http.StatusConflict},
		{"a bare carrier type", "PLD-TWO-BARE", map[string]any{
			"bin_type_ids": []int64{bare.ID},
			"manifest":     []map[string]any{{"part_number": "PLD-TWO-P9", "parts_per_cycle": 1}},
		}, http.StatusBadRequest},
	} {
		body := map[string]any{"code": tc.code, "uop_capacity": 10}
		for k, v := range tc.body {
			body[k] = v
		}
		rec := postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create", body)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d; body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		if p, err := db.GetPayloadByCode(tc.code); err == nil {
			t.Errorf("%s: payload %s (id %d) was created by a refused save", tc.name, tc.code, p.ID)
		}
	}
}
