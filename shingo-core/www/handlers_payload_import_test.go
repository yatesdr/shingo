//go:build docker

package www

import (
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/payloads"
)

// Characterization tests for handlers_payload_import.go — the Payloads
// page bulk import (.csv/.xlsx): one row per manifest part, the Payload
// Code repeated; the server groups, validates, SKIPS duplicates, and
// reports per-row results instead of aborting on the first bad line.

// rows helper builds a [][]string with a leading header, the convention
// the plant will author.
func importRows(rows ...[]string) [][]string {
	out := [][]string{{"Payload Code", "UoP Capacity", "Manifest Part (CATID)", "Qty"}}
	out = append(out, rows...)
	return out
}

// mustEq is a tiny helper for the import counts, where the number IS the
// assertion (testutil only carries MustNoErr).
func mustEq(t *testing.T, got, want int, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %d, want %d", what, got, want)
	}
}

func TestImportPayloadGroups_CreatesPayloadsAndManifests(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rep := h.importPayloadGroups(importRows(
		[]string{"RAIL-KIT", "40", "40016911", "2"},
		[]string{"RAIL-KIT", "", "40017250", "1"}, // multi-part: same code, second part; UoP from first row
		[]string{"SINGLE", "25", "", ""},          // payload with no manifest parts
	))
	mustEq(t, rep.Summary.Created, 2, "created count")
	mustEq(t, rep.Summary.Failed, 0, "failed count")

	kit, err := db.GetPayloadByCode("RAIL-KIT")
	testutil.MustNoErr(t, err, "get RAIL-KIT")
	if kit.UOPCapacity != 40 {
		t.Errorf("RAIL-KIT UoP = %d, want 40 (blank repeat must not clobber)", kit.UOPCapacity)
	}
	items, err := db.ListPayloadManifest(kit.ID)
	testutil.MustNoErr(t, err, "list manifest")
	if len(items) != 2 || items[0].PartNumber != "40016911" || items[0].Quantity != 2 ||
		items[1].PartNumber != "40017250" || items[1].Quantity != 1 {
		t.Errorf("kit manifest = %+v, want 40016911x2 then 40017250x1", items)
	}

	single, err := db.GetPayloadByCode("SINGLE")
	testutil.MustNoErr(t, err, "get SINGLE")
	if single.UOPCapacity != 25 {
		t.Errorf("SINGLE UoP = %d, want 25", single.UOPCapacity)
	}
	items, err = db.ListPayloadManifest(single.ID)
	testutil.MustNoErr(t, err, "list single manifest")
	if len(items) != 0 {
		t.Errorf("SINGLE manifest = %+v, want empty", items)
	}
}

func TestImportPayloadGroups_SkipsDuplicates(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	existing := &payloads.Payload{Code: "DUP-PL", UOPCapacity: 10}
	testutil.MustNoErr(t, h.engine.PayloadService().Create(existing), "seed existing")

	rep := h.importPayloadGroups(importRows(
		[]string{"DUP-PL", "99", "40016911", "1"}, // would overwrite if mishandled
		[]string{"NEW-PL", "5", "", ""},
	))
	mustEq(t, rep.Summary.Created, 1, "created")
	mustEq(t, rep.Summary.Skipped, 1, "skipped")

	got, err := db.GetPayloadByCode("DUP-PL")
	testutil.MustNoErr(t, err, "get DUP-PL")
	if got.UOPCapacity != 10 {
		t.Errorf("duplicate was modified: UoP = %d, want untouched 10", got.UOPCapacity)
	}
	if items, _ := db.ListPayloadManifest(got.ID); len(items) != 0 {
		t.Errorf("duplicate gained manifest rows: %+v", items)
	}
}

func TestImportPayloadGroups_ValidatesAndWarns(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rep := h.importPayloadGroups(importRows(
		[]string{"BAD-UOP", "abc", "", ""},        // failed: UoP not a number
		[]string{"NEG-UOP", "-3", "", ""},         // failed: negative
		[]string{"ZERO-UOP", "", "", ""},          // created + warning
		[]string{"ZERO-QTY", "5", "40016911", ""}, // created + warning: qty blank = 0
		[]string{"", "5", "40016911", "1"},        // failed: no code
		[]string{"BAD-QTY", "5", "40016911", "x"}, // failed: qty not a number
	))
	mustEq(t, rep.Summary.Created, 2, "created (ZERO-UOP, ZERO-QTY)")
	mustEq(t, rep.Summary.Failed, 3, "failed (BAD-UOP, NEG-UOP, BAD-QTY, no-code)")

	// Every failure names its file line so the operator can find the row.
	for _, r := range rep.Rows {
		if r.Status == "failed" && r.Line == 0 {
			t.Errorf("failed row missing line number: %+v", r)
		}
	}
	byReason := func(substr string) bool {
		for _, r := range rep.Rows {
			if strings.Contains(r.Reason, substr) {
				return true
			}
		}
		return false
	}
	if !byReason("must be a whole number") {
		t.Errorf("no readable validation message in report: %+v", rep.Rows)
	}

	// ZERO-UOP exists with UoP 0 AND carries the warning.
	z, err := db.GetPayloadByCode("ZERO-UOP")
	testutil.MustNoErr(t, err, "get ZERO-UOP")
	if z.UOPCapacity != 0 {
		t.Errorf("ZERO-UOP UoP = %d, want 0", z.UOPCapacity)
	}
	foundWarn := false
	for _, r := range rep.Rows {
		if r.Status == "warning" && r.Code == "ZERO-UOP" && strings.Contains(r.Reason, "UoP is 0") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("missing UoP-0 warning for ZERO-UOP: %+v", rep.Rows)
	}
}

func TestImportPayloadGroups_BlankRowsAreIgnored(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rep := h.importPayloadGroups(importRows(
		[]string{"", "", "", ""},
		[]string{"REAL-PL", "7", "", ""},
		[]string{"", "", "", ""},
	))
	mustEq(t, rep.Summary.Created, 1, "created")
	mustEq(t, rep.Summary.Failed, 0, "failed")
	if _, err := db.GetPayloadByCode("REAL-PL"); err != nil {
		t.Fatalf("REAL-PL not created: %v", err)
	}
}

// TestApiImportPayloadTemplates_MultipartCSV drives the full HTTP path
// with a real multipart upload, the way the browser sends it.
func TestApiImportPayloadTemplates_MultipartCSV(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	var body strings.Builder
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", "payloads.csv")
	testutil.MustNoErr(t, err, "CreateFormFile")
	_, _ = fw.Write([]byte("Payload Code,UoP Capacity,Manifest Part (CATID),Qty\n" +
		"CSV-PL,30,40016911,3\n"))
	testutil.MustNoErr(t, w.Close(), "close multipart writer")

	req := httptest.NewRequest("POST", "/api/payloads/templates/import", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	h.apiImportPayloadTemplates(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got, err := db.GetPayloadByCode("CSV-PL")
	testutil.MustNoErr(t, err, "get CSV-PL")
	if got.UOPCapacity != 30 {
		t.Errorf("CSV-PL UoP = %d, want 30", got.UOPCapacity)
	}
}

// TestApiImportPayloadTemplates_RejectsUnknownType pins the guard against
// the .xls trap: users export "Excel" files that are really the old BIFF
// format, which excelize cannot read — the error must name the types we DO take.
func TestApiImportPayloadTemplates_RejectsUnknownType(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	var body strings.Builder
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", "payloads.xls") // old BIFF Excel
	testutil.MustNoErr(t, err, "CreateFormFile")
	_, _ = fw.Write([]byte("not a real xls"))
	testutil.MustNoErr(t, w.Close(), "close multipart writer")

	req := httptest.NewRequest("POST", "/api/payloads/templates/import", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	h.apiImportPayloadTemplates(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ".csv") {
		t.Errorf("error should name the accepted types, got: %s", rec.Body.String())
	}
}
