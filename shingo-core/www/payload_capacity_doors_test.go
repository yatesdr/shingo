//go:build docker

package www

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
