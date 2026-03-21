package www

import (
	"net/http"
	"strconv"

	"shingoedge/store"
)

func (h *Handlers) handleMaterial(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()

	processes, _ := db.ListProcesses()

	// Determine active process from query param or default to first
	var activeProcess *store.Process
	if lineParam := r.URL.Query().Get("process"); lineParam != "" {
		if lineID, err := strconv.ParseInt(lineParam, 10, 64); err == nil {
			for i := range processes {
				if processes[i].ID == lineID {
					activeProcess = &processes[i]
					break
				}
			}
		}
	}
	if activeProcess == nil && len(processes) > 0 {
		activeProcess = &processes[0]
	}

	var activeProcessID int64
	var activeStyleName string
	var slots []store.MaterialSlot

	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		if activeProcess.ActiveStyleID != nil {
			js, err := db.GetStyle(*activeProcess.ActiveStyleID)
			if err == nil {
				activeStyleName = js.Name
				slots, _ = db.ListSlotsByStyle(js.ID)
			}
		}
	}

	if slots == nil && activeProcess != nil {
		// No active style set — show all slots for this process's styles
		styles, _ := db.ListStylesByProcess(activeProcessID)
		for _, s := range styles {
			sp, _ := db.ListSlotsByStyle(s.ID)
			slots = append(slots, sp...)
		}
	}

	if slots == nil {
		slots, _ = db.ListSlots()
	}

	anomalies, rpMap := loadAnomalyData(h)

	data := map[string]interface{}{
		"Page":              "material",
		"Processes":         processes,
		"ActiveProcessID":   activeProcessID,
		"Slots":             slots,
		"ActiveStyle":       activeStyleName,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
	}

	h.renderTemplate(w, r, "material.html", data)
}
