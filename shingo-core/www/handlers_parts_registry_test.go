//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/payloads"
)

// The payloads form's one piece of recall, and the one question it is allowed
// to ask a person.

// TestApiGetPart_KnownAndUnknown: the form asks whether a typed part number is
// already known, so it can fill in the cat id without offering a list to pick
// from. An unknown number is a NORMAL answer — this page is where new parts
// enter shingo — and must not read as an error.
func TestApiGetPart_KnownAndUnknown(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	_, err := payloads.UpsertPart(db.DB, "51015-LH", "40016911", "left bracket")
	testutil.MustNoErr(t, err, "seed the part")

	rec := getPlain(t, h.apiGetPart, "/api/parts/lookup?part_number=51015-LH")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var part payloads.Part
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &part), "decode")
	if part.CATID != "40016911" {
		t.Errorf("catid = %q, want 40016911 — this is the value that saves the operator "+
			"typing it twice", part.CATID)
	}

	miss := getPlain(t, h.apiGetPart, "/api/parts/lookup?part_number=BRAND-NEW")
	if miss.Code != http.StatusNotFound {
		t.Errorf("unknown part status = %d, want 404 — an unknown number is a new part, "+
			"which is the ordinary case on this page", miss.Code)
	}
}

// TestApiSaveManifest_ConflictingCATIDIs409 is the drift-catcher reaching the
// operator.
//
// A part already known with a DIFFERENT cat id is a question — same part, or a
// typo? — and the server refuses rather than choosing. 409 rather than 500
// because the request is well-formed and the answer is a person's; 500 would
// read as "shingo is broken" for what is really "shingo needs you to confirm".
func TestApiSaveManifest_ConflictingCATIDIs409(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	_, err := payloads.UpsertPart(db.DB, "51015-RH", "40016911", "")
	testutil.MustNoErr(t, err, "seed the part with its cat id")

	rec := postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update",
		map[string]any{
			"id":           sd.Payload.ID,
			"code":         sd.Payload.Code,
			"uop_capacity": 10,
			"manifest": []map[string]any{
				{"part_number": "51015-RH", "catid": "99999999", "parts_per_cycle": 1},
			},
		})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}

	// And nothing was written — a half-saved manifest is a bad way to ask a
	// question.
	part, err := payloads.GetPartByNumber(db.DB, "51015-RH")
	testutil.MustNoErr(t, err, "re-read the part")
	if part.CATID != "40016911" {
		t.Errorf("cat id = %q after a refused save, want the original", part.CATID)
	}
}

// TestApiSaveManifest_BlankPartNumberIsRefused: the blank-line skip is gone.
// It made the per-cycle check optional for exactly the lines least likely to be
// deliberate, and it let a payload save with a line that named nothing.
func TestApiSaveManifest_BlankPartNumberIsRefused(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	rec := postJSON(t, h.apiUpdatePayloadTemplate, "/api/payloads/templates/update",
		map[string]any{
			"id":           sd.Payload.ID,
			"code":         sd.Payload.Code,
			"uop_capacity": 10,
			"manifest": []map[string]any{
				{"part_number": "", "catid": "40016911", "parts_per_cycle": 0},
			},
		})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — a line that names no part is not a line to skip; "+
			"skipping it is what made the ratio check optional. body=%s",
			rec.Code, rec.Body.String())
	}
}
