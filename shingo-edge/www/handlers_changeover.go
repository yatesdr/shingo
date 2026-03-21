package www

import (
	"net/http"
	"strconv"

	"shingoedge/changeover"
	"shingoedge/store"
)

func (h *Handlers) handleChangeover(w http.ResponseWriter, r *http.Request) {
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
	var fromJob, toJob, state string
	var active bool
	var styles []store.Style
	var activeStyleName string

	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		m := h.engine.ChangeoverMachine(activeProcess.ID)
		if m != nil {
			fromJob, toJob, state, active = m.Info()
		}
		styles, _ = db.ListStylesByProcess(activeProcess.ID)

		// Resolve active style name for the "From" field
		if activeProcess.ActiveStyleID != nil {
			for _, js := range styles {
				if js.ID == *activeProcess.ActiveStyleID {
					activeStyleName = js.Name
					break
				}
			}
		}
	}

	var changeoverLog []store.ChangeoverLog
	if activeProcessID > 0 {
		changeoverLog, _ = db.ListCurrentChangeoverLog(activeProcessID)
	}

	anomalies, rpMap := loadAnomalyData(h)

	data := map[string]interface{}{
		"Page":              "changeover",
		"Processes":         processes,
		"ActiveProcessID":   activeProcessID,
		"Styles":            styles,
		"ActiveStyle":       activeStyleName,
		"ChangeoverLog":     changeoverLog,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"Changeover": map[string]interface{}{
			"Active":       active,
			"FromJobStyle": fromJob,
			"ToJobStyle":   toJob,
			"State":        state,
			"StateIndex":   changeover.StateIndex(state),
		},
	}

	h.renderTemplate(w, r, "changeover.html", data)
}
