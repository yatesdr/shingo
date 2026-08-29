// handlers_payload_import.go — bulk payload import on the Payloads page.
//
// The operator picks a .xlsx or .csv file whose columns are, in order:
//
//	Payload Code | UoP Capacity | Manifest Part (CATID) | Qty
//
// One row per manifest part, with the Payload Code repeated — a payload
// with two manifest parts is two rows with the same code (that is the
// whole point of the format: every cell holds one simple value a human
// can author and verify in Excel). A code may also appear on a single
// row with no part, creating the payload with an empty manifest.
//
// Per-row results, never abort-on-first-error: the response reports
// created / skipped / failed / warning rows so a 200-row file with one
// bad quantity still imports the 199 good ones and NAMES the bad one.
package www

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"

	"shingocore/domain"
)

// maxPayloadImportBytes caps the uploaded file. A payload list is text;
// anything near this size is not a payload list.
const maxPayloadImportBytes = 5 << 20 // 5 MiB

// importRowResult is one line of the import report. Line is the 1-based
// row number in the file (the header, when present, is line 1).
type importRowResult struct {
	Line   int    `json:"line"`
	Code   string `json:"code"`
	Status string `json:"status"` // "created" | "skipped" | "failed" | "warning"
	Reason string `json:"reason,omitempty"`
}

type importReport struct {
	Summary struct {
		Created  int `json:"created"`
		Skipped  int `json:"skipped"`
		Failed   int `json:"failed"`
		Warnings int `json:"warnings"`
	} `json:"summary"`
	Rows []importRowResult `json:"rows"`
}

// apiImportPayloadTemplates accepts multipart/form-data with one file
// field ("file") and bulk-creates payloads. Duplicates are SKIPPED and
// named in the report, not errors.
func (h *Handlers) apiImportPayloadTemplates(w http.ResponseWriter, r *http.Request) {
	// ParseMultipartForm's argument is a MEMORY threshold, not a size cap —
	// the real cap is MaxBytesReader, which errors once the body exceeds it.
	r.Body = http.MaxBytesReader(w, r.Body, maxPayloadImportBytes)
	if err := r.ParseMultipartForm(maxPayloadImportBytes); err != nil {
		h.jsonError(w, "invalid upload: "+err.Error(), 400)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		h.jsonError(w, "missing file field", 400)
		return
	}
	defer file.Close()

	var rows [][]string
	switch ext := strings.ToLower(filepath.Ext(hdr.Filename)); ext {
	case ".csv":
		rows, err = readCSVRows(file)
	case ".xlsx", ".xlsm":
		rows, err = readXLSXRows(file)
	default:
		h.jsonError(w, "unsupported file type "+ext+" — use .csv, .xlsx or .xlsm", 400)
		return
	}
	if err != nil {
		h.jsonError(w, "could not read file: "+err.Error(), 400)
		return
	}
	if len(rows) == 0 {
		h.jsonError(w, "file has no rows", 400)
		return
	}

	report := h.importPayloadGroups(rows)
	h.jsonOK(w, report)
}

// readCSVRows reads all records. Two tolerances the real world demands:
//
//   - Excel's "CSV UTF-8" export writes a BOM, which otherwise rides the
//     first cell and defeats header detection (the header row imports as
//     a bogus payload whose code starts with \ufeff).
//   - Ragged rows (fewer cells than the header) read as blank trailing
//     cells rather than failing the whole file — a missing trailing Qty
//     is a per-row warning downstream, not a 400.
func readCSVRows(r io.Reader) ([][]string, error) {
	br := bufio.NewReader(r)
	if b, err := br.Peek(3); err == nil && bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		br.Discard(3)
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	return cr.ReadAll()
}

// readXLSXRows reads the FIRST worksheet. Trailing empty cells arrive
// trimmed by excelize, which the grouping treats naturally.
func readXLSXRows(r io.Reader) ([][]string, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.GetRows(f.GetSheetName(0))
}

// importEntry is one FILE ROW belonging to a payload's group, carrying its
// own line number so a validation failure names the exact row that offended
// rather than the payload's first row.
type importEntry struct {
	line int
	uop  string
	part string
	qty  string
}

// importGroup is every row that shares one payload code, in file order.
type importGroup struct {
	code    string
	line    int // first row — the "home" line for payload-level results
	entries []importEntry
}

// groupImportRows folds the file into one group per payload code, IN FILE
// ORDER so the report reads top-to-bottom.
//
// It also owns the two per-row verdicts that can be reached before a payload
// exists at all: a wholly blank row is skipped (spreadsheets have them) and a
// row with cells but no code is failed where it sits.
func groupImportRows(rows [][]string, report *importReport) []*importGroup {
	// Header detection: a first row whose first cell names the code column
	// is a header, not a payload whose code is "Payload Code".
	start := 0
	if len(rows) > 0 && isImportHeaderRow(rows[0]) {
		start = 1
	}
	var order []*importGroup
	groups := map[string]*importGroup{}
	for i := start; i < len(rows); i++ {
		line := i + 1
		cells := rows[i]
		cell := func(n int) string {
			if n < len(cells) {
				return strings.TrimSpace(cells[n])
			}
			return ""
		}
		code := cell(0)
		// A wholly blank row is not an error — spreadsheets have them.
		if code == "" && cell(1) == "" && cell(2) == "" && cell(3) == "" {
			continue
		}
		if code == "" {
			report.add(importRowResult{Line: line, Status: "failed", Reason: "payload code is required"})
			continue
		}
		g, ok := groups[code]
		if !ok {
			g = &importGroup{code: code, line: line}
			groups[code] = g
			order = append(order, g)
		}
		g.entries = append(g.entries, importEntry{line: line, uop: cell(1), part: cell(2), qty: cell(3)})
	}
	return order
}

// resolveGroupUoP returns the group's UoP capacity and whether it validated.
//
// The first NON-EMPTY value in the group wins. The common authoring slip is
// leaving it blank on a continuation row while filling it on the first; the
// inverse (blank first, filled later) must not silently become UoP 0. A garbage
// value fails the payload — falling through to a later row would mask the typo.
func resolveGroupUoP(g *importGroup, report *importReport) (int64, bool) {
	for _, e := range g.entries {
		if e.uop == "" {
			continue
		}
		v, ok := parseImportInt(e.uop)
		switch {
		case !ok:
			report.add(importRowResult{Line: e.line, Code: g.code, Status: "failed",
				Reason: fmt.Sprintf("UoP %q must be a whole number ≥ 0", e.uop)})
			return 0, false
		case v > math.MaxInt32: // payloads.uop_capacity is a Postgres integer
			report.add(importRowResult{Line: e.line, Code: g.code, Status: "failed",
				Reason: fmt.Sprintf("UoP %q is too large", e.uop)})
			return 0, false
		default:
			return v, true
		}
	}
	return 0, true // no row carried one: UoP 0, which earns a warning downstream
}

// validateGroupQuantities reports whether every part row's quantity parses.
//
// Blank = 0; otherwise a whole number ≥ 0. FAILS FAST on the first bad
// quantity, reporting ITS row — one failed row per bad payload, pointing at
// the line that needs fixing rather than one per bad cell.
func validateGroupQuantities(g *importGroup, report *importReport) bool {
	for _, e := range g.entries {
		if e.part == "" {
			continue
		}
		q, ok := parseImportInt(e.qty)
		if !ok {
			report.add(importRowResult{Line: e.line, Code: g.code, Status: "failed",
				Reason: fmt.Sprintf("quantity %q for part %s must be a whole number ≥ 0", e.qty, e.part)})
			return false
		}
		if q > math.MaxInt64/2 { // absurd guard; bigint holds far more
			report.add(importRowResult{Line: e.line, Code: g.code, Status: "failed",
				Reason: fmt.Sprintf("quantity %q for part %s is too large", e.qty, e.part)})
			return false
		}
	}
	return true
}

// importPayloadGroups is the parse → group → validate → create pipeline,
// separated from HTTP so the tests can drive it with plain rows.
//
// The three validation phases are their own functions above. They were
// extracted rather than baselined when funlen caught this at 85 statements:
// the ceiling exists so the NEXT oversized function fails CI, and the
// characterization suite in handlers_payload_import_test.go is the behavioural
// evidence the ratchet requires for a body move.
func (h *Handlers) importPayloadGroups(rows [][]string) *importReport {
	report := &importReport{Rows: []importRowResult{}}

	for _, g := range groupImportRows(rows, report) {
		code := g.code
		uop, ok := resolveGroupUoP(g, report)
		if !ok {
			continue
		}
		if !validateGroupQuantities(g, report) {
			continue
		}

		// Duplicate policy: SKIP and say so.
		if existing, err := h.engine.PayloadService().GetByCode(code); err == nil && existing != nil {
			report.add(importRowResult{Line: g.line, Code: code, Status: "skipped",
				Reason: "payload already exists"})
			continue
		}

		p := &domain.Payload{Code: code, UOPCapacity: int(uop)}
		if err := h.engine.PayloadService().Create(p); err != nil {
			report.add(importRowResult{Line: g.line, Code: code, Status: "failed",
				Reason: "create: " + err.Error()})
			continue
		}
		report.Summary.Created++
		report.add(importRowResult{Line: g.line, Code: code, Status: "created"})

		// Payload-level warnings ride the payload's first row so each
		// distinct issue appears once, beside the payload it is about.
		if uop == 0 {
			report.add(importRowResult{Line: g.line, Code: code, Status: "warning",
				Reason: "UoP is 0 — the bin cannot hold anything"})
		}
		parts := make([]*domain.PayloadManifestItem, 0, len(g.entries))
		for _, e := range g.entries {
			if e.part == "" {
				continue
			}
			q, _ := parseImportInt(e.qty) // already validated above
			if q == 0 {
				report.add(importRowResult{Line: e.line, Code: code, Status: "warning",
					Reason: fmt.Sprintf("part %s has quantity 0", e.part)})
			}
			parts = append(parts, &domain.PayloadManifestItem{PartNumber: e.part, Quantity: q})
		}
		if len(parts) > 0 {
			if err := h.engine.PayloadService().ReplaceManifest(p.ID, parts); err != nil {
				report.add(importRowResult{Line: g.line, Code: code, Status: "failed",
					Reason: "created, but manifest failed: " + err.Error()})
			}
		}
	}
	return report
}

// parseImportInt accepts the spellings a spreadsheet cell produces:
// "40", "40.0", "40.00" (a 0.00-formatted cell) all mean forty; "40.5",
// "abc", and negatives are rejected. Blank is zero.
func parseImportInt(s string) (int64, bool) {
	if s == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f != math.Trunc(f) || f > math.MaxInt64 {
		return 0, false
	}
	return int64(f), true
}

func (rep *importReport) add(res importRowResult) {
	switch res.Status {
	case "skipped":
		rep.Summary.Skipped++
	case "failed":
		rep.Summary.Failed++
	case "warning":
		rep.Summary.Warnings++
	}
	rep.Rows = append(rep.Rows, res)
}

// isImportHeaderRow reports whether the first row names the columns
// rather than carrying a payload (a payload whose code literally starts
// with "payload" is still fine — the full phrase is what is checked).
func isImportHeaderRow(first []string) bool {
	if len(first) == 0 {
		return false
	}
	c := strings.ToLower(strings.TrimSpace(first[0]))
	return c == "payload code" || c == "payload" || c == "code"
}
