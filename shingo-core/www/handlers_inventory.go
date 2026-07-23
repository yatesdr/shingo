package www

import (
	"fmt"
	"net/http"
	"time"

	"github.com/xuri/excelize/v2"
	"shingo/protocol"
)

// InventoryInvariant is the Item 13 plant-wide running totals shape.
// BinSum is signed because the SME lock allows bins to go negative
// (overpack/underpack); over time the signed sum drifts in either
// direction as production smooths out, useful as a trend indicator
// rather than a hard equation. BucketSum stays non-negative by
// schema CHECK constraint. Total = BinSum + BucketSum, so dashboards
// can present either the components or the rolled-up plant total.
type InventoryInvariant struct {
	Total      int64     `json:"total"`
	BinSum     int64     `json:"bin_sum"`    // signed; can be negative per SME lock
	BucketSum  int64     `json:"bucket_sum"` // always >= 0
	ComputedAt time.Time `json:"computed_at"`
}

// apiInventoryInvariant returns the plant-wide running totals as JSON.
// Item 13 invariant probe — dashboards verify the signed sum stays
// approximately stable (overpack/underpack wash out at the aggregate
// level). The handler returns sums regardless of sign — clients must
// not assume non-negative bin_sum.
func (h *Handlers) apiInventoryInvariant(w http.ResponseWriter, r *http.Request) {
	inv, err := h.engine.InventoryDeltaService().SumInvariant()
	if err != nil {
		h.jsonError(w, "sum invariant: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, InventoryInvariant{
		Total:      inv.Total,
		BinSum:     inv.BinSum,
		BucketSum:  inv.BucketSum,
		ComputedAt: time.Now().UTC(),
	})
}

func (h *Handlers) handleInventory(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Page": "inventory",
	}
	h.render(w, r, "inventory.html", data)
}

func (h *Handlers) apiInventory(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.InventoryService().List()
	if err != nil {
		h.jsonError(w, "Failed to load inventory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiInventoryMonitorTotals returns the per-payload Replenishment Health rollup:
// DB on-hand (bins + lineside split), the threshold monitor's cached total (for
// drift detection), and configured thresholds. Powers the inventory page's
// Replenishment Health meters and drift chips (monitor cache ≠ DB truth).
func (h *Handlers) apiInventoryMonitorTotals(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.ReplenishmentHealth(r.Context())
	if err != nil {
		h.jsonError(w, "replenishment health: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiBuckets returns every authoritative lineside_buckets row as JSON.
// Powers the "Lineside Buckets" section on the operator-facing
// inventory page. Round-3 Obs 10 added the Delete column on top of
// this read-side: apiBucketDelete (below) is the admin recovery hatch
// for clearing Core-only orphan rows.
//
// See lineside-buckets-investigation-2026-05-18.md.
func (h *Handlers) apiBuckets(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.InventoryService().ListLinesideBuckets()
	if err != nil {
		h.jsonError(w, "Failed to load lineside buckets: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

// apiBucketDelete removes one lineside_buckets row + its
// inventory_delta_dedup row by primary key. Round-3 Obs 10 — the
// operator-driven recovery hatch for the cross-namespace orphan
// shape that the Obs 8 protocol fix made impossible to create going
// forward. Auth-gated via requireAuth (binary in this codebase; no
// finer role distinction).
//
// The audit row records source="ui", actor=session username, so
// operations can trace which engineer cleared which bucket.
func (h *Handlers) apiBucketDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int64 `json:"id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.ID <= 0 {
		h.jsonError(w, "id required", http.StatusBadRequest)
		return
	}

	n, err := h.engine.InventoryService().DeleteLinesideBucket(req.ID)
	if err != nil {
		h.jsonError(w, "delete lineside bucket: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		h.jsonError(w, "no lineside bucket with that id", http.StatusNotFound)
		return
	}

	actor := h.getUsername(r)
	if actor == "" {
		actor = protocol.AuditActorUI
	}
	if as := h.engine.AuditService(); as != nil {
		as.Append("lineside_bucket", req.ID, "deleted", "active", "deleted", actor)
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiInventoryExport(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.InventoryService().List()
	if err != nil {
		http.Error(w, "Failed to load inventory", http.StatusInternalServerError)
		return
	}

	f := excelize.NewFile()
	sheet := "Inventory"
	f.SetSheetName("Sheet1", sheet)

	// Headers
	headers := []string{"Group", "Lane", "Node", "Zone", "Bin Label", "Bin Type", "Status", "In Transit", "Destination", "Payload Code", "Cat-ID", "Qty", "UOP Remaining", "Confirmed"}
	for i, hdr := range headers {
		c, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheet, c, hdr)
	}

	// Style the header row bold
	style, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	f.SetRowStyle(sheet, 1, 1, style)

	// Data rows
	for i, row := range rows {
		rn := i + 2
		f.SetCellValue(sheet, cell("A", rn), row.GroupName)
		f.SetCellValue(sheet, cell("B", rn), row.LaneName)
		f.SetCellValue(sheet, cell("C", rn), row.NodeName)
		f.SetCellValue(sheet, cell("D", rn), row.Zone)
		f.SetCellValue(sheet, cell("E", rn), row.BinLabel)
		f.SetCellValue(sheet, cell("F", rn), row.BinType)
		f.SetCellValue(sheet, cell("G", rn), row.Status)
		transit := ""
		if row.InTransit {
			transit = "Yes"
		}
		f.SetCellValue(sheet, cell("H", rn), transit)
		f.SetCellValue(sheet, cell("I", rn), row.Destination)
		f.SetCellValue(sheet, cell("J", rn), row.PayloadCode)
		f.SetCellValue(sheet, cell("K", rn), row.CatID)
		f.SetCellValue(sheet, cell("L", rn), row.Qty)
		f.SetCellValue(sheet, cell("M", rn), row.UOPRemaining)
		confirmed := ""
		if row.Confirmed {
			confirmed = "Yes"
		}
		f.SetCellValue(sheet, cell("N", rn), confirmed)
	}

	// Set reasonable column widths
	colWidths := map[string]float64{
		"A": 14, "B": 14, "C": 18, "D": 10, "E": 14, "F": 10, "G": 12,
		"H": 10, "I": 18, "J": 14, "K": 14, "L": 8, "M": 14, "N": 10,
	}
	for col, wd := range colWidths {
		f.SetColWidth(sheet, col, col, wd)
	}

	// Second sheet: lineside buckets. Same workbook so operators can
	// review both inventory views in one download. Read failures
	// degrade gracefully — the bins sheet still ships.
	if bucketRows, err := h.engine.InventoryService().ListLinesideBuckets(); err == nil {
		bucketSheet := "Lineside Buckets"
		if _, err := f.NewSheet(bucketSheet); err == nil {
			bucketHeaders := []string{"Cell", "Process", "Station", "Node", "Zone", "Style ID", "Part", "Payload Code", "State", "Qty"}
			for i, hdr := range bucketHeaders {
				c, _ := excelize.CoordinatesToCellName(i+1, 1)
				f.SetCellValue(bucketSheet, c, hdr)
			}
			f.SetRowStyle(bucketSheet, 1, 1, style)
			for i, br := range bucketRows {
				rn := i + 2
				f.SetCellValue(bucketSheet, cell("A", rn), br.GroupName)
				f.SetCellValue(bucketSheet, cell("B", rn), br.LaneName)
				f.SetCellValue(bucketSheet, cell("C", rn), br.Station)
				f.SetCellValue(bucketSheet, cell("D", rn), br.NodeName)
				f.SetCellValue(bucketSheet, cell("E", rn), br.Zone)
				f.SetCellValue(bucketSheet, cell("F", rn), br.StyleID)
				f.SetCellValue(bucketSheet, cell("G", rn), br.PartNumber)
				f.SetCellValue(bucketSheet, cell("H", rn), br.PayloadCode)
				f.SetCellValue(bucketSheet, cell("I", rn), br.State)
				f.SetCellValue(bucketSheet, cell("J", rn), br.Qty)
			}
		}
	}

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="inventory.xlsx"`)
	f.Write(w)
}

// cell builds a cell reference like "A2" from a column letter and row number.
func cell(col string, row int) string {
	return fmt.Sprintf("%s%d", col, row)
}
