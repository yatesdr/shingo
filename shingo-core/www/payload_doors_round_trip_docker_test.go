//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
)

// payload_doors_round_trip_docker_test.go — a payload update through each door
// that writes one, read back column by column.
//
// THE ROW IS READ WHOLE (to_jsonb), NOT BY A COLUMN LIST, so a column added to
// payloads later is in every comparison: on an update the expected row is the
// row before the save with exactly the door's own fields laid over it, so a
// door that does not carry a new column must leave it standing with no edit
// here. The importer's created row is spelled out literally, so a new column
// fails it until its value from the importer is written down beside the rest.
//
// The doors: the payloads page's form (handlePayloadUpdate), the template API
// (apiUpdatePayloadTemplate) and the importer (importPayloadGroups), which only
// creates and skips a code that exists.
//
// THE TOTE NUMBERS (tote_layers, uop_per_tote) are seeded away from NULL, so a
// door that does not carry them is seen leaving them standing; the doors that
// carry them are run setting one and blanking the other. A save that does not
// send a tote field leaves it standing; a blank box or a null is unknown.

func payloadRowJSON(t *testing.T, db *store.DB, id int64) map[string]any {
	t.Helper()
	var raw []byte
	testutil.MustNoErr(t, db.QueryRow(
		`SELECT to_jsonb(p) - 'updated_at' FROM payloads p WHERE id=$1`, id).Scan(&raw), "read payload row")
	var row map[string]any
	testutil.MustNoErr(t, json.Unmarshal(raw, &row), "decode payload row")
	return row
}

func payloadBinTypeIDs(t *testing.T, db *store.DB, id int64) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT bin_type_id FROM payload_bin_types WHERE payload_id=$1 ORDER BY bin_type_id`, id)
	testutil.MustNoErr(t, err, "read payload bin types")
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var bt int64
		testutil.MustNoErr(t, rows.Scan(&bt), "scan bin type")
		out = append(out, bt)
	}
	testutil.MustNoErr(t, rows.Err(), "payload bin types")
	return out
}

// payloadManifestJSON reads the manifest lines whole; withIDs keeps each line's
// id and created_at, which say whether the line was rewritten or left alone.
func payloadManifestJSON(t *testing.T, db *store.DB, id int64, withIDs bool) []map[string]any {
	t.Helper()
	sel := `to_jsonb(m) - 'payload_id'`
	if !withIDs {
		sel += ` - 'id' - 'created_at'`
	}
	rows, err := db.Query(`SELECT `+sel+` FROM payload_manifest m WHERE payload_id=$1 ORDER BY id`, id)
	testutil.MustNoErr(t, err, "read manifest")
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw []byte
		testutil.MustNoErr(t, rows.Scan(&raw), "scan manifest line")
		var line map[string]any
		testutil.MustNoErr(t, json.Unmarshal(raw, &line), "decode manifest line")
		out = append(out, line)
	}
	testutil.MustNoErr(t, rows.Err(), "manifest")
	return out
}

func overlay(row map[string]any, fields map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		out[k] = v
	}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

func requireSame(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s =\n  %v\nwant\n  %v", what, got, want)
	}
}

// seedDoorPayload creates a payload with every column away from its default,
// two carrier types and two manifest lines, through the template API's create.
func seedDoorPayload(t *testing.T, h *Handlers, db *store.DB, code string) (id int64, a, b, c int64) {
	t.Helper()
	a = mintBinType(t, db, code+"-A").ID
	b = mintBinType(t, db, code+"-B").ID
	c = mintBinType(t, db, code+"-C").ID
	rec := postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create", map[string]any{
		"code": code, "description": "seeded", "uop_capacity": 40, "robot_group": "RG-1",
		"near_empty_enabled": true, "near_empty_robot_group": "RG-NE", "near_empty_threshold_pct": 20,
		"tote_layers": 5, "uop_per_tote": 10,
		"bin_type_ids": []int64{a, b},
		"manifest": []map[string]any{
			{"part_number": code + "-P1", "catid": "70000001", "parts_per_cycle": 2},
			{"part_number": code + "-P2", "parts_per_cycle": 1},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed %s: status %d; body=%s", code, rec.Code, rec.Body.String())
	}
	p, err := db.GetPayloadByCode(code)
	testutil.MustNoErr(t, err, "read seeded payload")
	if p.ToteLayers == nil || *p.ToteLayers != 5 || p.UOPPerTote == nil || *p.UOPPerTote != 10 {
		t.Fatalf("seed %s: tote numbers %v / %v, want 5 / 10", code, p.ToteLayers, p.UOPPerTote)
	}
	return p.ID, a, b, c
}

func TestPayloadDoors_UpdateRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("the form writes its eight fields and leaves carriers and lines standing", func(t *testing.T) {
		t.Parallel()
		h, db := testHandlers(t)
		id, a, b, _ := seedDoorPayload(t, h, db, "PLD-FORM")
		before := payloadRowJSON(t, db, id)
		lines := payloadManifestJSON(t, db, id, true)

		f := url.Values{}
		f.Set("id", fmt.Sprint(id))
		f.Set("code", "PLD-FORM-2")
		f.Set("description", "form edit")
		f.Set("uop_capacity", "50")
		f.Set("robot_group", "RG-2")
		f.Set("near_empty_robot_group", "RG-NE2")
		f.Set("near_empty_threshold_pct", "30")
		// near_empty_enabled absent: an unticked box, which the form reads as off.
		rec := postFormPL(t, h.handlePayloadUpdate, "/payloads/update", f)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("form update: status %d; body=%s", rec.Code, rec.Body.String())
		}

		requireSame(t, "row after the form", payloadRowJSON(t, db, id), overlay(before, map[string]any{
			"code": "PLD-FORM-2", "description": "form edit", "uop_capacity": float64(50),
			"robot_group": "RG-2", "near_empty_enabled": false, "near_empty_robot_group": "RG-NE2",
			"near_empty_threshold_pct": float64(30), "advanced_load_sequence": "",
		}))
		requireSame(t, "carriers after the form", payloadBinTypeIDs(t, db, id), []int64{a, b})
		requireSame(t, "lines after the form (ids included)", payloadManifestJSON(t, db, id, true), lines)
	})

	t.Run("the form writes the tote numbers it carries: a number, and a blank as unknown", func(t *testing.T) {
		t.Parallel()
		h, db := testHandlers(t)
		id, a, b, _ := seedDoorPayload(t, h, db, "PLD-FTOTE")
		before := payloadRowJSON(t, db, id)

		f := url.Values{}
		f.Set("id", fmt.Sprint(id))
		f.Set("code", "PLD-FTOTE")
		f.Set("description", "seeded")
		f.Set("uop_capacity", "40")
		f.Set("robot_group", "RG-1")
		f.Set("near_empty_enabled", "1")
		f.Set("near_empty_robot_group", "RG-NE")
		f.Set("near_empty_threshold_pct", "20")
		f.Set("tote_layers", "6")
		f.Set("uop_per_tote", "")
		rec := postFormPL(t, h.handlePayloadUpdate, "/payloads/update", f)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("form update: status %d; body=%s", rec.Code, rec.Body.String())
		}
		requireSame(t, "row after the form", payloadRowJSON(t, db, id), overlay(before, map[string]any{
			"tote_layers": float64(6), "uop_per_tote": nil,
		}))
		requireSame(t, "carriers after the form", payloadBinTypeIDs(t, db, id), []int64{a, b})

		f.Set("tote_layers", "0")
		rec = postFormPL(t, h.handlePayloadUpdate, "/payloads/update", f)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("a layer count of 0: status %d, want 400", rec.Code)
		}
	})

	t.Run("the template API writes the tote numbers it carries: a number, and null as unknown", func(t *testing.T) {
		t.Parallel()
		h, db := testHandlers(t)
		id, a, _, _ := seedDoorPayload(t, h, db, "PLD-ATOTE")
		before := payloadRowJSON(t, db, id)

		body := map[string]any{
			"id": id, "code": "PLD-ATOTE", "description": "seeded", "uop_capacity": 40, "robot_group": "RG-1",
			"near_empty_enabled": true, "near_empty_robot_group": "RG-NE", "near_empty_threshold_pct": 20,
			"tote_layers": nil, "uop_per_tote": 12,
			"bin_type_ids": []int64{a},
			"manifest":     []map[string]any{{"part_number": "PLD-ATOTE-P1", "catid": "70000001", "parts_per_cycle": 2}},
		}
		rec := postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("api update: status %d; body=%s", rec.Code, rec.Body.String())
		}
		requireSame(t, "row after the API", payloadRowJSON(t, db, id), overlay(before, map[string]any{
			"tote_layers": nil, "uop_per_tote": float64(12),
		}))

		body["uop_per_tote"] = -1
		rec = postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("a UOP per tote of -1: status %d, want 400", rec.Code)
		}
	})

	t.Run("the template API writes its fields, its carriers and its lines", func(t *testing.T) {
		t.Parallel()
		h, db := testHandlers(t)
		id, _, _, c := seedDoorPayload(t, h, db, "PLD-API")
		before := payloadRowJSON(t, db, id)
		var partID any
		for _, l := range payloadManifestJSON(t, db, id, false) {
			if l["part_number"] == "PLD-API-P1" {
				partID = l["part_id"]
			}
		}

		rec := postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update", map[string]any{
			"id": id, "code": "PLD-API", "description": "api edit", "uop_capacity": 60, "robot_group": "RG-3",
			"near_empty_enabled": true, "near_empty_robot_group": "RG-NE3", "near_empty_threshold_pct": 40,
			"bin_type_ids": []int64{c},
			"manifest":     []map[string]any{{"part_number": "PLD-API-P1", "catid": "70000001", "parts_per_cycle": 3}},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("api update: status %d; body=%s", rec.Code, rec.Body.String())
		}
		requireSame(t, "api answer", rec.Body.String(), "{\"status\":\"ok\"}\n")
		requireSame(t, "row after the API", payloadRowJSON(t, db, id), overlay(before, map[string]any{
			"description": "api edit", "uop_capacity": float64(60), "robot_group": "RG-3",
			"near_empty_enabled": true, "near_empty_robot_group": "RG-NE3",
			"near_empty_threshold_pct": float64(40), "advanced_load_sequence": "",
		}))
		requireSame(t, "carriers after the API", payloadBinTypeIDs(t, db, id), []int64{c})
		requireSame(t, "lines after the API", payloadManifestJSON(t, db, id, false), []map[string]any{
			{"part_number": "PLD-API-P1", "parts_per_cycle": float64(3), "description": "", "part_id": partID},
		})
	})

	// TODAY, AND A DEFECT: a save that sends neither list replaces both with
	// nothing (the edit form sends [] when its lists never loaded). The change
	// that makes an absent list leave the stored one standing flips the last
	// two assertions; this records what it changes.
	t.Run("the template API with both lists absent clears both today", func(t *testing.T) {
		t.Parallel()
		h, db := testHandlers(t)
		id, _, _, _ := seedDoorPayload(t, h, db, "PLD-BARE")
		before := payloadRowJSON(t, db, id)

		rec := postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update", map[string]any{
			"id": id, "code": "PLD-BARE", "description": "seeded", "uop_capacity": 40, "robot_group": "RG-1",
			"near_empty_enabled": true, "near_empty_robot_group": "RG-NE", "near_empty_threshold_pct": 20,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("api update: status %d; body=%s", rec.Code, rec.Body.String())
		}
		requireSame(t, "row after the API", payloadRowJSON(t, db, id), before)
		requireSame(t, "carriers after a save that sent none", payloadBinTypeIDs(t, db, id), []int64{})
		requireSame(t, "lines after a save that sent none", payloadManifestJSON(t, db, id, false), []map[string]any{})
	})

	t.Run("the importer skips a code that exists and writes nothing on it", func(t *testing.T) {
		t.Parallel()
		h, db := testHandlers(t)
		id, a, b, _ := seedDoorPayload(t, h, db, "PLD-IMP")
		before := payloadRowJSON(t, db, id)
		lines := payloadManifestJSON(t, db, id, true)

		rep := h.importPayloadGroups(importRows(
			[]string{"PLD-IMP", "90", "70000099", "5"},
			[]string{"PLD-IMP-NEW", "25", "70000098", "2"},
		))
		mustEq(t, rep.Summary.Created, 1, "created")

		requireSame(t, "existing row after import", payloadRowJSON(t, db, id), before)
		requireSame(t, "existing carriers after import", payloadBinTypeIDs(t, db, id), []int64{a, b})
		requireSame(t, "existing lines after import (ids included)", payloadManifestJSON(t, db, id, true), lines)

		// The created row is the store's create with only a code and a
		// capacity: every other column is its default, and the importer writes
		// no carriers.
		created, err := db.GetPayloadByCode("PLD-IMP-NEW")
		testutil.MustNoErr(t, err, "read the imported payload")
		got := payloadRowJSON(t, db, created.ID)
		requireSame(t, "imported row", overlay(got, map[string]any{"id": nil, "created_at": nil}),
			map[string]any{
				"id": nil, "created_at": nil, "code": "PLD-IMP-NEW", "description": "", "uop_capacity": float64(25),
				"robot_group": "", "advanced_load_sequence": "", "near_empty_enabled": false,
				"near_empty_robot_group": "", "near_empty_threshold_pct": float64(0),
				"tote_layers": nil, "uop_per_tote": nil,
			})
		requireSame(t, "imported carriers", payloadBinTypeIDs(t, db, created.ID), []int64{})
		if n := len(payloadManifestJSON(t, db, created.ID, false)); n != 1 {
			t.Errorf("imported lines = %d, want 1", n)
		}
	})
}
