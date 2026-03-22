package www

import (
	"net/http"
	"strconv"

	"shingoedge/store"
)

func (h *Handlers) handleChangeover(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()
	processes, _ := db.ListProcesses()

	var activeProcess *store.Process
	if processParam := r.URL.Query().Get("process"); processParam != "" {
		if processID, err := strconv.ParseInt(processParam, 10, 64); err == nil {
			for i := range processes {
				if processes[i].ID == processID {
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
	var styles []store.Style
	var currentStyleName string
	var activeChangeover *store.ProcessChangeover
	var stationTasks []store.ChangeoverStationTask
	nodeTaskMap := map[int64][]store.ChangeoverNodeTask{}

	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		styles, _ = db.ListStylesByProcess(activeProcess.ID)
		if activeProcess.ActiveStyleID != nil {
			if s, err := db.GetStyle(*activeProcess.ActiveStyleID); err == nil {
				currentStyleName = s.Name
			}
		}
		activeChangeover, _ = db.GetActiveProcessChangeover(activeProcess.ID)
		if activeChangeover != nil {
			stationTasks, _ = db.ListChangeoverStationTasks(activeChangeover.ID)
			for _, task := range stationTasks {
				nodeTaskMap[task.ID], _ = db.ListChangeoverNodeTasks(task.ID)
			}
		}
	}

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]interface{}{
		"Page":             "changeover",
		"Processes":        processes,
		"ActiveProcessID":  activeProcessID,
		"Styles":           styles,
		"CurrentStyle":     currentStyleName,
		"ActiveChangeover": activeChangeover,
		"StationTasks":     stationTasks,
		"NodeTaskMap":      nodeTaskMap,
		"Anomalies":        anomalies,
		"ReportingPointMap": rpMap,
	}
	h.renderTemplate(w, r, "changeover.html", data)
}
