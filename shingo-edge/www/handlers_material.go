package www

import (
	"net/http"

	"shingoedge/store"
)

func buildStationViews(db *store.DB, activeProcess *store.Process) []store.OperatorStationView {
	if activeProcess == nil {
		return nil
	}
	stations, _ := db.ListOperatorStationsByProcess(activeProcess.ID)
	var views []store.OperatorStationView
	for _, station := range stations {
		if view, err := db.BuildOperatorStationView(station.ID); err == nil {
			views = append(views, *view)
		}
	}
	return views
}

func (h *Handlers) handleMaterial(w http.ResponseWriter, r *http.Request) {
	db := h.engine.DB()
	processes, _ := db.ListProcesses()
	activeProcess := resolveProcessFromQuery(r, processes)

	var activeProcessID int64
	var currentStyleName, targetStyleName string

	stationViews := buildStationViews(db, activeProcess)

	if activeProcess != nil {
		activeProcessID = activeProcess.ID
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
	activeProcess := resolveProcessFromQuery(r, processes)

	stationViews := buildStationViews(db, activeProcess)

	data := map[string]interface{}{
		"StationViews": stationViews,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "material-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
