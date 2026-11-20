//go:build docker

package www

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"

	"shingocore/service"
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

// payload_capacity_doors_test.go — a payload's UoP capacity is required at
// every door that writes it (I1).
//
// uop_capacity is the standard pack: how many pieces a full bin of a part holds
// (a kit's: cycles). A zero there is not "unknown", it is a bin that holds
// nothing — the standard-pack fallback hands the line a carrier with 0 in it,
// and the near-empty and over-capacity arithmetic divide by it. The form doors
// used to discard the Atoi error, so a blank or mistyped box saved 0 with no
// word to the person typing.
//
// EXISTING rows are untouched until edited (the parts_per_cycle precedent):
// nothing here rewrites a stored 0, it refuses to write a new one.
//
// NOT covered: the bulk importer. It creates through PayloadService.Create and
// still admits 0 with a warning, and PayloadService.Create does not carry the
// check for that reason; see the lane report.
//
// The code is held the same way (TestPayloadCodeIsRequiredAtEveryDoor): the
// service trims it and refuses it blank at every door but the importer, which
// trims and refuses its own.

func TestPayloadCapacityIsRequiredAtEveryDoor(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	existing := &payloads.Payload{Code: "CAP-EXISTING", Description: "d", UOPCapacity: 7}
	if err := db.CreatePayload(existing); err != nil {
		t.Fatalf("seed payload: %v", err)
	}

	formCreate := func(uop string, set bool) int {
		f := url.Values{}
		f.Set("code", fmt.Sprintf("CAP-FORM-%s-%v", uop, set))
		if set {
			f.Set("uop_capacity", uop)
		}
		return postFormPL(t, h.handlePayloadCreate, "/payloads/create", f).Code
	}
	formUpdate := func(uop string, set bool) int {
		f := url.Values{}
		f.Set("id", fmt.Sprint(existing.ID))
		f.Set("code", existing.Code)
		if set {
			f.Set("uop_capacity", uop)
		}
		return postFormPL(t, h.handlePayloadUpdate, "/payloads/update", f).Code
	}
	jsonCreate := func(uop int) int {
		return postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create",
			map[string]any{"code": fmt.Sprintf("CAP-JSON-%d", uop), "uop_capacity": uop}).Code
	}
	jsonUpdate := func(uop int) int {
		return postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update",
			map[string]any{"id": existing.ID, "code": existing.Code, "uop_capacity": uop}).Code
	}

	cases := []struct {
		door string
		got  int
		want int
	}{
		{"form create, blank", formCreate("", false), http.StatusBadRequest},
		{"form create, not a number", formCreate("abc", true), http.StatusBadRequest},
		{"form create, zero", formCreate("0", true), http.StatusBadRequest},
		{"form create, negative", formCreate("-3", true), http.StatusBadRequest},
		{"form create, 5", formCreate("5", true), http.StatusSeeOther},
		{"form update, blank", formUpdate("", false), http.StatusBadRequest},
		{"form update, not a number", formUpdate("abc", true), http.StatusBadRequest},
		{"form update, zero", formUpdate("0", true), http.StatusBadRequest},
		{"form update, 9", formUpdate("9", true), http.StatusSeeOther},
		{"json create, zero", jsonCreate(0), http.StatusBadRequest},
		{"json create, 4", jsonCreate(4), http.StatusOK},
		{"json update, zero", jsonUpdate(0), http.StatusBadRequest},
		{"json update, 11", jsonUpdate(11), http.StatusOK},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: status %d, want %d", c.door, c.got, c.want)
		}
	}

	// A refused create writes nothing.
	for _, code := range []string{"CAP-FORM--false", "CAP-FORM-0-true", "CAP-JSON-0"} {
		if p, err := db.GetPayloadByCode(code); err == nil && p != nil {
			t.Errorf("refused create %q left a payload row: %+v", code, p)
		}
	}

	// The service door itself refuses, with a sentinel a handler can classify.
	p, err := db.GetPayload(existing.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	p.UOPCapacity = 0
	if err := h.engine.PayloadService().Update(p); !errors.Is(err, service.ErrPayloadCapacity) {
		t.Errorf("PayloadService.Update(capacity 0) = %v, want ErrPayloadCapacity", err)
	}
	if got, _ := db.GetPayload(existing.ID); got.UOPCapacity != 11 {
		t.Errorf("capacity after refused update = %d, want 11 (the last accepted value)", got.UOPCapacity)
	}
}

// TestPayloadCodeIsRequiredAtEveryDoor: a blank or all-space code is refused
// at both desktop creates and both desktop updates, writing nothing, and a
// padded code is stored trimmed. The template API's create took a blank code
// before the check moved into the service: only the browser's required box
// stood in the way.
func TestPayloadCodeIsRequiredAtEveryDoor(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	existing := &payloads.Payload{Code: "SYN-CODE-KEEP", Description: "d", UOPCapacity: 7}
	if err := db.CreatePayload(existing); err != nil {
		t.Fatalf("seed payload: %v", err)
	}
	count := func() int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM payloads`).Scan(&n); err != nil {
			t.Fatalf("count payloads: %v", err)
		}
		return n
	}
	before := count()

	formCreate := func(code string) int {
		return postFormPL(t, h.handlePayloadCreate, "/payloads/create",
			url.Values{"code": {code}, "uop_capacity": {"5"}}).Code
	}
	formUpdate := func(code string) int {
		return postFormPL(t, h.handlePayloadUpdate, "/payloads/update",
			url.Values{"id": {fmt.Sprint(existing.ID)}, "code": {code}, "uop_capacity": {"7"}}).Code
	}
	jsonCreate := func(code string) int {
		return postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create",
			map[string]any{"code": code, "uop_capacity": 4}).Code
	}
	jsonUpdate := func(code string) int {
		return postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update",
			map[string]any{"id": existing.ID, "code": code, "uop_capacity": 7}).Code
	}
	for _, c := range []struct {
		door string
		got  int
	}{
		{"json create, blank", jsonCreate("")},
		{"json create, spaces", jsonCreate("   ")},
		{"form create, blank", formCreate("")},
		{"form create, spaces", formCreate(" \t ")},
		{"json update, blank", jsonUpdate("")},
		{"form update, spaces", formUpdate("  ")},
	} {
		if c.got != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", c.door, c.got)
		}
	}
	if n := count(); n != before {
		t.Errorf("refused creates left %d payload rows, want %d", n, before)
	}
	kept, err := db.GetPayload(existing.ID)
	if got := testutil.Must(t, kept, err, "read the kept part"); got.Code != "SYN-CODE-KEEP" {
		t.Errorf("code after refused updates = %+v, want SYN-CODE-KEEP", got)
	}

	// A padded code is stored as the part's number, trimmed: never a second
	// part beside the unpadded one.
	if got := jsonCreate("  SYN-CODE-PAD  "); got != http.StatusOK {
		t.Fatalf("json create, padded: status %d, want 200", got)
	}
	if p, err := db.GetPayloadByCode("SYN-CODE-PAD"); err != nil || p == nil {
		t.Errorf("padded create not stored trimmed: %v", err)
	}
	if got := jsonCreate("SYN-CODE-PAD "); got == http.StatusOK {
		t.Errorf("json create of a padded spelling of an existing code: status %d, want a refusal", got)
	}
}

// TestPayloadCreateRefusals_TheFloorRefusesWhatTheDesktopRefuses: each create
// the desktop's template API refuses, the floor's part create refuses too, and
// neither writes: one set of checks, in the service, behind both doors. A
// manifest line the desktop refuses (a blank part, a ratio below 1) has no
// floor arm: the floor states no manifest, Core builds its one line from the
// number, at ratio 1. The floor's own refusals are TestDoorAsk_PartCreate's.
func TestPayloadCreateRefusals_TheFloorRefusesWhatTheDesktopRefuses(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	d := h.doors()
	carrier := mintBinType(t, db, "SYN-PAR-BT")
	bare := mintBareType(t, db, "SYN-PAR-CART")
	// SYN-PAR-LOOSE is a part Core knows, with a CATID, that no payload is
	// yet: the seed's second manifest line.
	rec := postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create", map[string]any{
		"code": "SYN-PAR-KEEP", "uop_capacity": 5,
		"manifest": []map[string]any{
			{"part_number": "SYN-PAR-KEEP", "catid": "SYN-PAR-C1", "parts_per_cycle": 1},
			{"part_number": "SYN-PAR-LOOSE", "catid": "SYN-PAR-C2", "parts_per_cycle": 1},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: status %d; body=%s", rec.Code, rec.Body.String())
	}
	count := func() int {
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM payloads`).Scan(&n), "count payloads")
		return n
	}

	type submit struct {
		code    string
		pack    int
		carrier *bins.BinType
		catid   string
		layers  *int
		perTote *int
	}
	zero, minus := 0, -1
	for _, c := range []struct {
		name string
		s    submit
	}{
		{"a blank code", submit{code: "", pack: 5, carrier: carrier}},
		{"a code of spaces", submit{code: "   ", pack: 5, carrier: carrier}},
		{"the padded code of a part Core has", submit{code: " SYN-PAR-KEEP ", pack: 5, carrier: carrier}},
		{"a pack of 0", submit{code: "SYN-PAR-1", pack: 0, carrier: carrier}},
		{"a pack below 0", submit{code: "SYN-PAR-2", pack: -3, carrier: carrier}},
		{"tote layers of 0", submit{code: "SYN-PAR-3", pack: 5, carrier: carrier, layers: &zero}},
		{"a UoP per tote below 0", submit{code: "SYN-PAR-4", pack: 5, carrier: carrier, perTote: &minus}},
		{"a bare carrier", submit{code: "SYN-PAR-5", pack: 5, carrier: bare}},
		{"a CATID that contradicts the part's own", submit{code: "SYN-PAR-LOOSE", pack: 5, carrier: carrier, catid: "SYN-PAR-C9"}},
	} {
		before := count()
		body := map[string]any{"code": c.s.code, "uop_capacity": c.s.pack, "bin_type_ids": []int64{c.s.carrier.ID},
			"manifest": []map[string]any{{"part_number": c.s.code, "catid": c.s.catid, "parts_per_cycle": 1}}}
		if c.s.layers != nil {
			body["tote_layers"] = *c.s.layers
		}
		if c.s.perTote != nil {
			body["uop_per_tote"] = *c.s.perTote
		}
		desk := postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create", body)
		floor := d.AskPartCreate("edge.par", mustJSON(t, protocol.PartCreateRequest{ProcessID: 17, PartNumber: c.s.code,
			StandardPack: c.s.pack, BinTypeCode: c.s.carrier.Code, CATID: c.s.catid, ToteLayers: c.s.layers, UOPPerTote: c.s.perTote}))
		if desk.Code == http.StatusOK {
			t.Errorf("%s: the desktop created it", c.name)
		}
		if floor.Status != http.StatusConflict || floor.Sentence == "" {
			t.Errorf("%s: the floor answered %d %q, want 409 with a sentence", c.name, floor.Status, floor.Sentence)
		}
		if after := count(); after != before {
			t.Errorf("%s: %d payloads written", c.name, after-before)
		}
	}
}

// TestPayloadRatioChangeWarnsThenSaves: an Edit Payload save that changes the
// ratio of a part whose bin holds a count writes nothing and answers 409 with
// the warning and each bin's reading before and after; the same save with
// confirm_ratio_change writes the ratio and moves no count. A change between
// one part and a kit is refused while the count stands, confirmed or not
// (owner, 2026-11-19).
func TestPayloadRatioChangeWarnsThenSaves(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	p := &payloads.Payload{Code: "RATIO-A", Description: "d", UOPCapacity: 800}
	testutil.MustNoErr(t, db.CreatePayload(p), "seed payload")
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{PayloadID: p.ID, PartNumber: "RATIO-A", PartsPerCycle: 8}, ""), "seed line")
	bt := &bins.BinType{Code: "RATIO-BT"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "seed bin type")
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "RATIO-BIN", Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "seed bin")
	if _, err := db.Exec(`UPDATE bins SET payload_code = $1, uop_remaining = 800 WHERE id = $2`, p.Code, bin.ID); err != nil {
		t.Fatalf("seed count: %v", err)
	}
	save := func(manifest []map[string]any, confirm bool) (int, map[string]any) {
		body := map[string]any{"id": p.ID, "code": p.Code, "uop_capacity": 800, "manifest": manifest}
		if confirm {
			body["confirm_ratio_change"] = true
		}
		rec := postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update", body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	ratio := func() int64 {
		lines, err := db.ListPayloadManifest(p.ID)
		testutil.MustNoErr(t, err, "read lines")
		if len(lines) != 1 {
			t.Fatalf("lines = %d, want 1", len(lines))
		}
		return lines[0].PartsPerCycle
	}
	count := func() int {
		b, err := db.GetBin(bin.ID)
		testutil.MustNoErr(t, err, "read bin")
		return b.UOPRemaining
	}
	at4 := []map[string]any{{"part_number": "RATIO-A", "parts_per_cycle": 4}}

	code, out := save(at4, false)
	rc, _ := out["ratio_change"].(map[string]any)
	if code != http.StatusConflict || rc == nil || ratio() != 8 || count() != 800 {
		t.Fatalf("unconfirmed 8 -> 4: %d %v, ratio %d, count %d; want 409 with the warning, 8, 800", code, out, ratio(), count())
	}
	rows, _ := rc["rows"].([]any)
	row, _ := func() (map[string]any, bool) {
		if len(rows) == 1 {
			r, ok := rows[0].(map[string]any)
			return r, ok
		}
		return nil, false
	}()
	if row == nil || row["what"] != "RATIO-BIN" || row["parts"] != float64(800) || row["uop_before"] != float64(100) || row["uop_after"] != float64(200) {
		t.Errorf("the warning's rows = %v, want RATIO-BIN 800 parts, 100 -> 200 UoP", rows)
	}
	if code, out := save(at4, true); code != http.StatusOK || ratio() != 4 || count() != 800 {
		t.Fatalf("confirmed 8 -> 4: %d %v, ratio %d, count %d; want 200, 4, 800 (no count moves)", code, out, ratio(), count())
	}
	kit := []map[string]any{{"part_number": "RATIO-A", "parts_per_cycle": 1}, {"part_number": "RATIO-A2", "parts_per_cycle": 2}}
	if code, out := save(kit, true); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "RATIO-BIN") || ratio() != 4 {
		t.Errorf("one part to a kit with a count standing: %d %v; want 409 naming RATIO-BIN, nothing written", code, out)
	}
	// A part at 1 a cycle reads at the ratio a kit does, so its flip is caught
	// by the line count alone, not by a ratio change.
	at1 := []map[string]any{{"part_number": "RATIO-A", "parts_per_cycle": 1}}
	if code, out := save(at1, true); code != http.StatusOK || ratio() != 1 {
		t.Fatalf("confirmed 4 -> 1: %d %v, ratio %d; want 200, 1", code, out, ratio())
	}
	if code, out := save(kit, true); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "RATIO-BIN") || ratio() != 1 {
		t.Errorf("a part at 1 a cycle to a kit with a count standing: %d %v; want 409 naming RATIO-BIN, nothing written", code, out)
	}
}

// TestManifestLineDoorsAskThePayloadGuards: every door that writes a
// payload's manifest lines asks the payload guards, not Edit Payload's alone.
// A line's edit and a line's delete that change the ratio under a counted bin
// answer 409 with the warning and write nothing, then save on
// confirm_ratio_change; a line added that makes the part a kit is refused,
// confirmed or not; the importer's replace has no warning to show and refuses
// a ratio change outright.
func TestManifestLineDoorsAskThePayloadGuards(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	p := &payloads.Payload{Code: "DOOR-A", Description: "d", UOPCapacity: 800}
	testutil.MustNoErr(t, db.CreatePayload(p), "seed payload")
	line := &payloads.ManifestItem{PayloadID: p.ID, PartNumber: "DOOR-A", PartsPerCycle: 8}
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(line, ""), "seed line")
	bt := &bins.BinType{Code: "DOOR-BT"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "seed bin type")
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "DOOR-BIN", Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "seed bin")
	if _, err := db.Exec(`UPDATE bins SET payload_code = $1, uop_remaining = 800 WHERE id = $2`, p.Code, bin.ID); err != nil {
		t.Fatalf("seed count: %v", err)
	}
	lines := func() []int64 {
		ls, err := db.ListPayloadManifest(p.ID)
		testutil.MustNoErr(t, err, "read lines")
		var out []int64
		for _, l := range ls {
			out = append(out, l.PartsPerCycle)
		}
		return out
	}
	call := func(door http.HandlerFunc, path string, body map[string]any) (int, map[string]any) {
		rec := postJSON(t, door, path, body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	edit := map[string]any{"id": line.ID, "part_number": "DOOR-A", "parts_per_cycle": 4}
	if code, out := call(h.apiUpdateManifestItem, "/api/payloads/manifest/update", edit); code != http.StatusConflict ||
		out["ratio_change"] == nil || fmt.Sprint(lines()) != "[8]" {
		t.Fatalf("a line's edit 8 -> 4, unconfirmed: %d %v, lines %v; want 409 with the warning, [8]", code, out, lines())
	}
	edit["confirm_ratio_change"] = true
	if code, out := call(h.apiUpdateManifestItem, "/api/payloads/manifest/update", edit); code != http.StatusOK || fmt.Sprint(lines()) != "[4]" {
		t.Fatalf("a line's edit 8 -> 4, confirmed: %d %v, lines %v; want 200, [4]", code, out, lines())
	}
	add := map[string]any{"payload_id": p.ID, "part_number": "DOOR-B", "parts_per_cycle": 1, "confirm_ratio_change": true}
	if code, out := call(h.apiCreateManifestItem, "/api/payloads/manifest/create", add); code != http.StatusConflict ||
		!strings.Contains(fmt.Sprint(out["error"]), "DOOR-BIN") || fmt.Sprint(lines()) != "[4]" {
		t.Errorf("a line added (a kit) under a count: %d %v, lines %v; want 409 naming DOOR-BIN, [4]", code, out, lines())
	}
	del := map[string]any{"id": line.ID}
	if code, out := call(h.apiDeleteManifestItem, "/api/payloads/manifest/delete", del); code != http.StatusConflict ||
		out["ratio_change"] == nil || fmt.Sprint(lines()) != "[4]" {
		t.Errorf("the only line deleted (4 -> 1), unconfirmed: %d %v, lines %v; want 409 with the warning, [4]", code, out, lines())
	}
	refused := h.engine.PayloadService().ReplaceManifest(p.ID, []payloads.Line{{PartNumber: "DOOR-A", PartsPerCycle: 2}})
	var unseen *service.RatioChangeUnseen
	if !errors.As(refused, &unseen) || fmt.Sprint(lines()) != "[4]" {
		t.Errorf("the importer's replace 4 -> 2 under a count: %v, lines %v; want RatioChangeUnseen, [4]", refused, lines())
	}
	del["confirm_ratio_change"] = true
	if code, out := call(h.apiDeleteManifestItem, "/api/payloads/manifest/delete", del); code != http.StatusOK || len(lines()) != 0 {
		t.Errorf("the only line deleted, confirmed: %d %v, lines %v; want 200, none", code, out, lines())
	}
	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "read bin")
	if b.UOPRemaining != 800 {
		t.Errorf("the bin's count after every door = %d, want 800: no count moves", b.UOPRemaining)
	}
}
