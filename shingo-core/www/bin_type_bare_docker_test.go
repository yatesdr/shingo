//go:build docker

package www

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// bin_type_bare_docker_test.go — the bin-type form's bare checkbox and its
// hidden companion. The edit door is a full-record write, so the companion is
// what tells "unticked" apart from "this POST never asked"
// (TestPinBinTypeUpdate_AFieldTheFormOmitsIsOverwritten is that hazard on a
// field without one).

func editBinType(t *testing.T, h *Handlers, bt *bins.BinType, bare []string) *bins.BinType {
	t.Helper()
	form := url.Values{"id": {strconv.FormatInt(bt.ID, 10)}, "code": {bt.Code}, "description": {bt.Description}}
	if bare != nil {
		form["bare"] = bare
	}
	rec := postForm(t, h.handleBinTypeUpdate, "/bin-types/update", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d; body=%s", rec.Code, rec.Body.String())
	}
	got, err := h.engine.BinService().GetBinType(bt.ID)
	testutil.MustNoErr(t, err, "reread")
	return got
}

func TestBinTypeForm_ResavingABareTypeKeepsItBare(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	bt := &bins.BinType{Code: "BAREFORM-1", Description: "d", Bare: true}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create")

	// The form as the browser sends it with the box ticked: companion, then box.
	if got := editBinType(t, h, bt, []string{"false", "true"}); !got.Bare {
		t.Fatal("re-saving a bare type through the edit form un-flagged it")
	}
	// A POST that does not carry the field at all keeps the stored value.
	if got := editBinType(t, h, bt, nil); !got.Bare {
		t.Fatal("a POST without the bare field un-flagged the type; absent must keep it")
	}
	// Unticked: only the companion arrives, and that is an un-flag.
	if got := editBinType(t, h, bt, []string{"false"}); got.Bare {
		t.Fatal("an unticked box (companion only) left the type bare")
	}
}

func TestBinTypeForm_CreateReadsTheBox(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	for _, tc := range []struct {
		code string
		bare []string
		want bool
	}{
		{"BARECREATE-TICKED", []string{"false", "true"}, true},
		{"BARECREATE-UNTICKED", []string{"false"}, false},
		{"BARECREATE-ABSENT", nil, false},
	} {
		form := url.Values{"code": {tc.code}}
		if tc.bare != nil {
			form["bare"] = tc.bare
		}
		rec := postForm(t, h.handleBinTypeCreate, "/bin-types/create", form)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: status %d; body=%s", tc.code, rec.Code, rec.Body.String())
		}
		got, err := db.GetBinTypeByCode(tc.code)
		testutil.MustNoErr(t, err, "read "+tc.code)
		if got.Bare != tc.want {
			t.Errorf("%s: bare = %v, want %v", tc.code, got.Bare, tc.want)
		}
	}
}

// TestBinTypeForm_FlaggingARuleTypeIsABadRequest: the refusal is the admin's to
// fix, so it answers 400 with its sentence, and the type stays unflagged.
func TestBinTypeForm_FlaggingARuleTypeIsABadRequest(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	testutil.MustNoErr(t, db.SetPayloadBinTypes(sd.Payload.ID, []int64{sd.BinType.ID}), "rule")

	rec := postForm(t, h.handleBinTypeUpdate, "/bin-types/update", url.Values{
		"id": {strconv.FormatInt(sd.BinType.ID, 10)}, "code": {sd.BinType.Code}, "bare": {"false", "true"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	got, err := db.GetBinType(sd.BinType.ID)
	testutil.MustNoErr(t, err, "reread")
	if got.Bare {
		t.Error("the refused flag landed")
	}
}

// TestPayloadRuleSave_BareTypeIsABadRequest: the JSON rule door answers 400.
func TestPayloadRuleSave_BareTypeIsABadRequest(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bare := &bins.BinType{Code: "BARERULE-HALF", Bare: true}
	testutil.MustNoErr(t, db.CreateBinType(bare), "create bare")

	rec := postJSON(t, h.apiSavePayloadBinTypes, "/api/payloads/templates/bin-types",
		map[string]any{"payload_id": sd.Payload.ID, "bin_type_ids": []int64{sd.BinType.ID, bare.ID}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "BARERULE-HALF")
}
