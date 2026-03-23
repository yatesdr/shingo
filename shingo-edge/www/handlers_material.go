package www

import (
	"net/http"
	"strconv"

	"shingoedge/store"
)

func (h *Handlers) handleMaterial(w http.ResponseWriter, r *http.Request) {
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
	var stationViews []store.OperatorStationView
	var currentStyleName, targetStyleName string

	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		stations, _ := db.ListOperatorStationsByProcess(activeProcess.ID)
		for _, station := range stations {
			if view, err := db.BuildOperatorStationView(station.ID); err == nil {
				stationViews = append(stationViews, *view)
			}
		}
		if activeProcess.ActiveStyleID != nil {
			if style, err := db.GetStyle(*activeProcess.ActiveStyleID); err == nil {
				currentStyleName = style.Name
			}
		}
		if activeProcess.TargetStyleID != nil {
			if style, err := db.GetStyle(*activeProcess.TargetStyleID); err == nil {
				targetStyleName = style.Name
			}
		}
	}

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]interface{}{
		"Page":            "material",
		"Processes":       processes,
		"ActiveProcessID": activeProcessID,
		"StationViews":    stationViews,
		"CurrentStyle":    currentStyleName,
		"TargetStyle":     targetStyleName,
		"Anomalies":       anomalies,
		"ReportingPointMap": rpMap,
	}
	h.renderTemplate(w, r, "material.html", data)
}

func (h *Handlers) handleMaterialPartial(w http.ResponseWriter, r *http.Request) {
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

	var stationViews []store.OperatorStationView
	if activeProcess != nil {
		stations, _ := db.ListOperatorStationsByProcess(activeProcess.ID)
		for _, station := range stations {
			if view, err := db.BuildOperatorStationView(station.ID); err == nil {
				stationViews = append(stationViews, *view)
			}
		}
	}

	data := map[string]interface{}{
		"StationViews": stationViews,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "material-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
