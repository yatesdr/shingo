//go:build docker

package www

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"shingocore/service"
	"shingocore/store/payloads"
)

// payload_capacity_doors_test.go — a payload's UoP capacity is required at
// every door that writes it (I1).
//
// uop_capacity is the standard pack: how many production cycles a full bin of
// the payload holds. A zero there is not "unknown", it is a bin that holds
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
