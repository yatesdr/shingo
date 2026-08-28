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
	"encoding/csv"
	"fmt"
	"io"
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

// readCSVRows reads all records; FieldsPerRecord is left at the default
// so a ragged file errors loudly rather than silently mis-parsing.
func readCSVRows(r io.Reader) ([][]string, error) {
	return csv.NewReader(r).ReadAll()
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

// importPayloadGroups is the parse → group → validate → create pipeline,
// separated from HTTP so the tests can drive it with plain rows.
func (h *Handlers) importPayloadGroups(rows [][]string) *importReport {
	report := &importReport{Rows: []importRowResult{}}

	// Header detection: a first row whose first cell names the code column
	// is a header, not a payload whose code is "Payload Code".
	start := 0
	if len(rows) > 0 && isImportHeaderRow(rows[0]) {
		start = 1
	}

	// Group rows by code IN FILE ORDER so the report reads top-to-bottom.
	type group struct {
		line   int
		code   string
		uopRaw string
		parts  []string
		qtys   []string
	}
	var order []string
	groups := map[string]*group{}
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
			g = &group{line: line, code: code, uopRaw: cell(1)}
			groups[code] = g
			order = append(order, code)
		}
		if part := cell(2); part != "" {
			g.parts = append(g.parts, part)
			g.qtys = append(g.qtys, cell(3))
		}
	}

	for _, code := range order {
		g := groups[code]
		// UoP: blank = 0 (legal but flagged); otherwise a whole number ≥ 0.
		uop := 0
		if g.uopRaw != "" {
			v, err := strconv.Atoi(g.uopRaw)
			if err != nil || v < 0 {
				report.add(importRowResult{Line: g.line, Code: code, Status: "failed",
					Reason: fmt.Sprintf("UoP %q must be a whole number ≥ 0", g.uopRaw)})
				continue
			}
			uop = v
		}
		// Quantities: blank = 0; otherwise a whole number ≥ 0. A part with
		// zero quantity is warned, not failed — the modal allows it too.
		qtyFail := false
		for i, q := range g.qtys {
			if q == "" {
				continue
			}
			v, err := strconv.Atoi(q)
			if err != nil || v < 0 {
				report.add(importRowResult{Line: g.line, Code: code, Status: "failed",
					Reason: fmt.Sprintf("quantity %q for part %s must be a whole number ≥ 0", q, g.parts[i])})
				qtyFail = true
			}
		}
		if qtyFail {
			continue
		}

		// Duplicate policy: SKIP and say so.
		if existing, err := h.engine.PayloadService().GetByCode(code); err == nil && existing != nil {
			report.add(importRowResult{Line: g.line, Code: code, Status: "skipped",
				Reason: "payload already exists"})
			continue
		}

		p := &domain.Payload{Code: code, UOPCapacity: uop}
		if err := h.engine.PayloadService().Create(p); err != nil {
			report.add(importRowResult{Line: g.line, Code: code, Status: "failed",
				Reason: "create: " + err.Error()})
			continue
		}
		report.Summary.Created++
		report.add(importRowResult{Line: g.line, Code: code, Status: "created"})

		// The warnings ride the payload's FIRST row so each distinct issue
		// appears once, beside the payload it is about.
		if uop == 0 {
			report.add(importRowResult{Line: g.line, Code: code, Status: "warning",
				Reason: "UoP is 0 — the bin cannot hold anything"})
		}
		for i, part := range g.parts {
			q := int64(0)
			if g.qtys[i] != "" {
				q, _ = strconv.ParseInt(g.qtys[i], 10, 64)
			}
			if q == 0 {
				report.add(importRowResult{Line: g.line, Code: code, Status: "warning",
					Reason: fmt.Sprintf("part %s has quantity 0", part)})
			}
		}
		if len(g.parts) > 0 {
			items := make([]*domain.PayloadManifestItem, len(g.parts))
			for i, part := range g.parts {
				var q int64
				if g.qtys[i] != "" {
					q, _ = strconv.ParseInt(g.qtys[i], 10, 64)
				}
				items[i] = &domain.PayloadManifestItem{PartNumber: part, Quantity: q}
			}
			if err := h.engine.PayloadService().ReplaceManifest(p.ID, items); err != nil {
				report.add(importRowResult{Line: g.line, Code: code, Status: "failed",
					Reason: "created, but manifest failed: " + err.Error()})
			}
		}
	}
	return report
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
