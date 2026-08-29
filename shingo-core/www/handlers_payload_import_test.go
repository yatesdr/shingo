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
	// FOUR, and the label above is the count: BAD-UOP and NEG-UOP fail on the
	// UoP cell, the code-less row fails before it is ever grouped, and BAD-QTY
	// fails on the quantity. Every one of them is a failure this fixture asks
	// for on purpose — see the row comments — so if this number ever needs
	// lowering, a row has to leave the fixture with it.
	mustEq(t, rep.Summary.Failed, 4, "failed (BAD-UOP, NEG-UOP, no-code, BAD-QTY)")

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

// --- Review-fix regression tests -------------------------------------------

// TestReadCSVRows_StripsBOM pins the Excel "CSV UTF-8" trap: that export
// writes a UTF-8 BOM, which used to ride the first cell, defeat header
// detection, and import the header as a payload whose code began with
// an invisible character.
func TestReadCSVRows_StripsBOM(t *testing.T) {
	t.Parallel()
	in := "\ufeffPayload Code,UoP Capacity,Manifest Part (CATID),Qty\nPL-A,10,,\n"
	rows, err := readCSVRows(strings.NewReader(in))
	if err != nil {
		t.Fatalf("readCSVRows: %v", err)
	}
	if len(rows) != 2 || rows[0][0] != "Payload Code" {
		t.Fatalf("rows[0] = %q (len %d), want BOM-free header", rows[0], len(rows))
	}
	if !isImportHeaderRow(rows[0]) {
		t.Errorf("BOM-free header not detected as header")
	}
}

// TestReadCSVRows_ToleratesRaggedRows: a row with fewer cells than the
// header is blank trailing cells, not a file-fatal parse error.
func TestReadCSVRows_RaggedRows(t *testing.T) {
	t.Parallel()
	in := "Payload Code,UoP Capacity,Manifest Part (CATID),Qty\nPL-R,10,\n"
	rows, err := readCSVRows(strings.NewReader(in))
	if err != nil {
		t.Fatalf("readCSVRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}

// TestParseImportInt_ExcelNumberFormats: a cell formatted 0.00 yields
// "40.00" — that is forty, not a validation failure. "40.5" and negatives
// stay rejected.
func TestParseImportInt_ExcelNumberFormats(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"40", 40, true},
		{"40.0", 40, true},
		{"40.00", 40, true},
		{" 40 ", 0, false}, // caller trims; raw spaces are not accepted here
		{"40.5", 0, false},
		{"abc", 0, false},
		{"-3", 0, false},
		{"", 0, true}, // blank is zero
	}
	for _, c := range cases {
		got, ok := parseImportInt(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseImportInt(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestImportPayloadGroups_UoPFromLaterRow: the first row's UoP is blank
// but a continuation row carries it — the payload must not silently land
// at UoP 0 (and its warning).
func TestImportPayloadGroups_UoPFromLaterRow(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	rep := h.importPayloadGroups(importRows(
		[]string{"LATE-UOP", "", "40016911", "2"},
		[]string{"LATE-UOP", "40", "", ""},
	))
	mustEq(t, rep.Summary.Created, 1, "created")
	mustEq(t, rep.Summary.Warnings, 0, "warnings — UoP was provided, just later")

	got, err := db.GetPayloadByCode("LATE-UOP")
	testutil.MustNoErr(t, err, "get LATE-UOP")
	if got.UOPCapacity != 40 {
		t.Errorf("LATE-UOP UoP = %d, want 40", got.UOPCapacity)
	}
}

// TestImportPayloadGroups_FailureNamesItsOwnLine: a bad quantity on a
// group's SECOND row must report that row's line, and one payload yields
// ONE failed row — not one per bad cell, all pointing at the first line.
//
// ONE PAYLOAD, THREE ROWS. The fixture used to carry two codes (MULTI-BAD
// then MULTI), which grouped into two payloads — a healthy one and a
// single-row one whose offending cell was its FIRST row. That is not the
// shape the sentence above describes and it could not fail the way this
// test is meant to fail. The three rows are one code now.
//
// LINE 3 IS THE DISCRIMINATOR. The group's home line — what the report
// would name if it regressed to reporting g.line for every failure — is
// line 2. Asserting the offender's own line 3 is what separates the two.
func TestImportPayloadGroups_FailureNamesItsRow(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	rep := h.importPayloadGroups(importRows(
		[]string{"MULTI-BAD", "10", "40016911", "1"},  // line 2: the group's home row, fine
		[]string{"MULTI-BAD", "", "40017250", "oops"}, // line 3: the offender
		[]string{"MULTI-BAD", "", "40017300", "2"},    // line 4: never reached — fail-fast
	))
	mustEq(t, rep.Summary.Failed, 1, "failed — one per payload, fail-fast")
	var failed []importRowResult
	for _, r := range rep.Rows {
		if r.Status == "failed" {
			failed = append(failed, r)
		}
	}
	if len(failed) != 1 || failed[0].Line != 3 {
		t.Errorf("failed rows = %+v, want exactly one at line 3 — the offending row's OWN line, not "+
			"the group's home line 2", failed)
	}
}
