//go:build docker

package www

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/bins"
)

// tote_numbers_admin_docker_test.go — the admin doors and pages that carry the
// tote numbers: a bin type's totes per layer (the Bin Types forms on the Bins
// page, handleBinTypeCreate / handleBinTypeUpdate), and a payload's layers and
// UOP per tote (the Payloads page). NULL is unknown and a door that does not
// carry a field leaves it standing.

func perLayerOf(t *testing.T, h *Handlers, code string) *int {
	t.Helper()
	bt, err := h.engine.BinService().GetBinTypeByCode(code)
	testutil.MustNoErr(t, err, "read bin type "+code)
	return bt.TotesPerLayer
}

func intIs(p *int, want int) bool { return p != nil && *p == want }

func TestBinTypeDoors_TotesPerLayer(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rec := postForm(t, h.handleBinTypeCreate, "/bin-types/create",
		url.Values{"code": {"TPL-TOTE-1"}, "totes_per_layer": {"8"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := perLayerOf(t, h, "TPL-TOTE-1"); !intIs(got, 8) {
		t.Fatalf("created totes_per_layer = %v, want 8", deref(got))
	}
	created, err := h.engine.BinService().GetBinTypeByCode("TPL-TOTE-1")
	testutil.MustNoErr(t, err, "read the created type")
	id := strconv.FormatInt(created.ID, 10)

	rec = postForm(t, h.handleBinTypeCreate, "/bin-types/create", url.Values{"code": {"TPL-KD-1"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create without the field: status %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := perLayerOf(t, h, "TPL-KD-1"); got != nil {
		t.Errorf("a type created without the field reads %d totes a layer, want unknown", *got)
	}

	steps := []struct {
		name   string
		form   url.Values
		status int
		want   *int
	}{
		{"an edit that does not carry the field leaves it standing",
			url.Values{"id": {id}, "code": {"TPL-TOTE-1"}, "description": {"edited"}}, http.StatusSeeOther, intPtr(8)},
		{"a number is written",
			url.Values{"id": {id}, "code": {"TPL-TOTE-1"}, "totes_per_layer": {"4"}}, http.StatusSeeOther, intPtr(4)},
		{"0 is refused and the value stands",
			url.Values{"id": {id}, "code": {"TPL-TOTE-1"}, "totes_per_layer": {"0"}}, http.StatusBadRequest, intPtr(4)},
		{"a word is refused and the value stands",
			url.Values{"id": {id}, "code": {"TPL-TOTE-1"}, "totes_per_layer": {"four"}}, http.StatusBadRequest, intPtr(4)},
		{"a blank box is unknown",
			url.Values{"id": {id}, "code": {"TPL-TOTE-1"}, "totes_per_layer": {""}}, http.StatusSeeOther, nil},
	}
	for _, s := range steps {
		rec := postForm(t, h.handleBinTypeUpdate, "/bin-types/update", s.form)
		if rec.Code != s.status {
			t.Fatalf("%s: status %d, want %d; body=%s", s.name, rec.Code, s.status, rec.Body.String())
		}
		got := perLayerOf(t, h, "TPL-TOTE-1")
		if (got == nil) != (s.want == nil) || (got != nil && *got != *s.want) {
			t.Errorf("%s: totes_per_layer = %v, want %v", s.name, deref(got), deref(s.want))
		}
	}
}

func intPtr(v int) *int { return &v }

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestToteNumbers_PagesRender renders the two Core pages whose templates carry
// the tote numbers, with the numbers set and unset: the template executes, the
// form fields are there, and the Edit buttons carry the stored value (the
// edit forms open from them; a form that opened blank would save unknown over
// the stored number).
func TestToteNumbers_PagesRender(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForPages(t)
	cookie := loginCookie(t, h)

	perLayer := 8
	tote := &bins.BinType{Code: "TPR-TOTE-1", TotesPerLayer: &perLayer}
	testutil.MustNoErr(t, db.CreateBinType(tote), "create the tote carrier")
	kd := mintBinType(t, db, "TPR-KD-1")

	rec := postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create", map[string]any{
		"code": "TPR-PART-A", "uop_capacity": 400, "tote_layers": 5, "uop_per_tote": 10,
		"bin_type_ids": []int64{tote.ID, kd.ID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create payload: status %d; body=%s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, h.apiCreatePayloadTemplate, "/api/payloads/templates/create", map[string]any{
		"code": "TPR-PART-B", "uop_capacity": 40, "bin_type_ids": []int64{tote.ID},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create payload B: status %d; body=%s", rec.Code, rec.Body.String())
	}

	t.Run("payloads", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/payloads", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.handlePayloadsPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("payloads page status %d; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{
			`name="tote_layers" id="plc-tote-layers"`, `name="uop_per_tote" id="plc-uop-per-tote"`,
			`name="tote_layers" id="ple-tote-layers"`, `name="uop_per_tote" id="ple-uop-per-tote"`,
			`data-code="TPR-TOTE-1" data-totes-per-layer="8"`, `data-code="TPR-KD-1" data-totes-per-layer=""`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("payloads page missing %q", want)
			}
		}
		a, b := rowOf(t, body, "TPR-PART-A"), rowOf(t, body, "TPR-PART-B")
		for _, want := range []string{"TPR-TOTE-1: 10 × 8 × 5 = 400", `data-tote-layers="5"`, `data-uop-per-tote="10"`} {
			if !strings.Contains(a, want) {
				t.Errorf("TPR-PART-A row missing %q", want)
			}
		}
		if strings.Contains(a, "TPR-KD-1:") {
			t.Error("TPR-PART-A row shows a full cart on the KD carrier, which says no totes per layer")
		}
		for _, want := range []string{`data-tote-layers=""`, `data-uop-per-tote=""`} {
			if !strings.Contains(b, want) {
				t.Errorf("TPR-PART-B row missing %q", want)
			}
		}
		if strings.Contains(b, " × ") {
			t.Error("TPR-PART-B, with no tote numbers, shows a full cart")
		}
	})

	t.Run("bins", func(t *testing.T) {
		body := binsPage(t, h, cookie)
		for _, want := range []string{
			`name="totes_per_layer" min="1"`, `name="totes_per_layer" id="bt-edit-totes-per-layer"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("bins page missing %q", want)
			}
		}
		toteRow, kdRow := rowOf(t, body, "TPR-TOTE-1"), rowOf(t, body, "TPR-KD-1")
		if !strings.Contains(toteRow, `data-totes-per-layer="8"`) {
			t.Error(`TPR-TOTE-1's Edit button does not carry data-totes-per-layer="8"`)
		}
		if !strings.Contains(kdRow, `data-totes-per-layer=""`) {
			t.Error(`TPR-KD-1's Edit button does not carry an empty data-totes-per-layer`)
		}
	})
}

// rowOf is the table row that holds code, from the code to the row's end.
func rowOf(t *testing.T, body, code string) string {
	t.Helper()
	i := strings.Index(body, ">"+code+"<")
	if i < 0 {
		t.Fatalf("no row for %s", code)
	}
	end := strings.Index(body[i:], "</tr>")
	if end < 0 {
		t.Fatalf("row for %s has no end", code)
	}
	return body[i : i+end]
}
