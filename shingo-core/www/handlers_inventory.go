package www

import (
	"fmt"
	"net/http"

	"github.com/xuri/excelize/v2"
)

func (h *Handlers) handleInventory(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Page": "inventory",
	}
	h.render(w, r, "inventory.html", data)
}

func (h *Handlers) apiInventory(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.ListInventory()
	if err != nil {
		h.jsonError(w, "Failed to load inventory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, rows)
}

func (h *Handlers) apiInventoryExport(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.ListInventory()
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

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="inventory.xlsx"`)
	f.Write(w)
}

// cell builds a cell reference like "A2" from a column letter and row number.
func cell(col string, row int) string {
	return fmt.Sprintf("%s%d", col, row)
}
