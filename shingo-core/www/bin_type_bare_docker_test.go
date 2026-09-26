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

// bin_type_bare_docker_test.go — bare is not a form field. It is derived from
// bin_types.bare_of, which only a stage-1 CLEAR writes, so the bin-type form
// can neither make a type bare nor un-make a marker; a posted `bare` is ignored.

func TestBinTypeForm_BareFieldIsIgnored(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rec := postForm(t, h.handleBinTypeCreate, "/bin-types/create",
		url.Values{"code": {"BAREFORM-NEW"}, "bare": {"false", "true"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status %d; body=%s", rec.Code, rec.Body.String())
	}
	created, err := db.GetBinTypeByCode("BAREFORM-NEW")
	testutil.MustNoErr(t, err, "read created")
	if created.Bare {
		t.Error("a type created with bare=true in the form reads back bare")
	}

	markerID, err := db.EnsureBareMarker(created.ID)
	testutil.MustNoErr(t, err, "derive marker")
	marker, err := db.GetBinType(markerID)
	testutil.MustNoErr(t, err, "read marker")
	rec = postForm(t, h.handleBinTypeUpdate, "/bin-types/update", url.Values{
		"id": {strconv.FormatInt(marker.ID, 10)}, "code": {marker.Code}, "description": {"edited"}, "bare": {"false"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update status %d; body=%s", rec.Code, rec.Body.String())
	}
	got, err := db.GetBinType(marker.ID)
	testutil.MustNoErr(t, err, "reread marker")
	if !got.Bare || got.Description != "edited" {
		t.Errorf("marker after an edit posting bare=false = bare %v %q, want still bare with the edit", got.Bare, got.Description)
	}
}

// TestPayloadRuleSave_BareTypeIsABadRequest: the JSON rule door answers 400.
func TestPayloadRuleSave_BareTypeIsABadRequest(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	carrier := &bins.BinType{Code: "BARERULE"}
	testutil.MustNoErr(t, db.CreateBinType(carrier), "create carrier")
	markerID, err := db.EnsureBareMarker(carrier.ID)
	testutil.MustNoErr(t, err, "derive marker")

	rec := postJSON(t, h.apiSavePayloadBinTypes, "/api/payloads/templates/bin-types",
		map[string]any{"payload_id": sd.Payload.ID, "bin_type_ids": []int64{sd.BinType.ID, markerID}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "BARERULE-BARE")
}
