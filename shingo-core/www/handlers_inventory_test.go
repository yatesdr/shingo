//go:build docker

package www

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"shingocore/internal/testdb"
)

// Characterization tests for handlers_inventory.go —
//   - handleInventory: renders /inventory (HTML)
//   - apiInventory: GET /api/inventory returns []InventoryRow
//   - apiInventoryExport: GET /api/inventory/export streams an XLSX workbook

// --- handleInventory --------------------------------------------------------

// TestHandleInventory_RendersHTML pins the page-render path: the handler
// returns 200 with an HTML body containing the "Inventory" heading from
// inventory.html.
func TestHandleInventory_RendersHTML(t *testing.T) {
	h, _ := testHandlersForPages(t)

	req := httptest.NewRequest(http.MethodGet, "/inventory", nil)
	rec := httptest.NewRecorder()
	h.handleInventory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("expected HTML output, got prefix %q", body[:min(120, len(body))])
	}
	if !strings.Contains(body, "Inventory") {
		t.Errorf("expected 'Inventory' heading in body, not found; len=%d", len(body))
	}
}

// --- apiInventory -----------------------------------------------------------

// TestApiInventory_EmptyDB pins the empty-DB response: 200 + a JSON array
// (possibly empty) decode.
func TestApiInventory_EmptyDB(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiInventory, "/api/inventory")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestApiInventory_WithSeededBin pins the happy path: a bin at a storage
// node shows up as an InventoryRow with the bin label.
func TestApiInventory_WithSeededBin(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-INV-1")

	rec := getPlain(t, h.apiInventory, "/api/inventory")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var rows []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, r := range rows {
		if label, _ := r["bin_label"].(string); label == bin.Label {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected inventory row for bin %q, got %+v", bin.Label, rows)
	}
}

// --- apiInventoryExport -----------------------------------------------------

// TestApiInventoryExport_ContentType pins the download headers: the handler
// should set Content-Type to the XLSX MIME and Content-Disposition to an
// attachment with filename "inventory.xlsx".
func TestApiInventoryExport_ContentType(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiInventoryExport, "/api/inventory/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body size=%d", rec.Code, rec.Body.Len())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "spreadsheetml.sheet") {
		t.Errorf("Content-Type: got %q, want .../spreadsheetml.sheet", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "inventory.xlsx") {
		t.Errorf("Content-Disposition: got %q", cd)
	}
}

// TestApiInventoryExport_BodyIsValidXLSX pins the body shape: it is a ZIP-like
// XLSX that excelize can re-open, with the expected Inventory sheet name and
// the expected header row.
func TestApiInventoryExport_BodyIsValidXLSX(t *testing.T) {
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-EXPORT-1")

	rec := getPlain(t, h.apiInventoryExport, "/api/inventory/export")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	// Re-open the workbook with excelize to pin the sheet shape.
	f, err := excelize.OpenReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer f.Close()

	rows, err := f.GetRows("Inventory")
	if err != nil {
		t.Fatalf("get rows: %v", err)
	}
	if len(rows) < 1 {
		t.Fatalf("expected at least a header row, got %d", len(rows))
	}
	// Header row pinning: first column is "Group", bin-label is column E.
	want := []string{"Group", "Lane", "Node", "Zone", "Bin Label", "Bin Type", "Status"}
	for i, w := range want {
		if rows[0][i] != w {
			t.Errorf("header[%d]: got %q, want %q", i, rows[0][i], w)
		}
	}
	// The bin we seeded should appear in some data row under "Bin Label" (col E).
	foundBin := false
	for _, r := range rows[1:] {
		if len(r) >= 5 && r[4] == bin.Label {
			foundBin = true
		}
	}
	if !foundBin {
		t.Errorf("expected bin %q in exported rows, got %d rows", bin.Label, len(rows)-1)
	}
}

// --- cell helper ------------------------------------------------------------

// TestCellHelper pins the "A1"-style label generator used by the export path.
// This is a pure function but exported for the export code, so we lock it in.
func TestCellHelper(t *testing.T) {
	cases := []struct {
		col string
		row int
		want string
	}{
		{"A", 1, "A1"},
		{"B", 2, "B2"},
		{"Z", 100, "Z100"},
	}
	for _, tc := range cases {
		if got := cell(tc.col, tc.row); got != tc.want {
			t.Errorf("cell(%q,%d): got %q, want %q", tc.col, tc.row, got, tc.want)
		}
	}
}
