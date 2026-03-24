package www

import (
	"net/http"

	"shingoedge/engine"
	"shingoedge/store"
)

// enrichViewBinState fetches bin state from Core and attaches it to each node in the views.
func enrichViewBinState(coreAPI *engine.CoreClient, views []store.OperatorStationView) {
	if coreAPI == nil || !coreAPI.Available() {
		return
	}
	var nodeNames []string
	for _, v := range views {
		for _, n := range v.Nodes {
			if n.Node.CoreNodeName != "" {
				nodeNames = append(nodeNames, n.Node.CoreNodeName)
			}
		}
	}
	if len(nodeNames) == 0 {
		return
	}
	bins, err := coreAPI.FetchNodeBins(nodeNames)
	if err != nil || len(bins) == 0 {
		return
	}
	binMap := make(map[string]engine.NodeBinInfo, len(bins))
	for _, b := range bins {
		binMap[b.NodeName] = b
	}
	for i := range views {
		for j := range views[i].Nodes {
			name := views[i].Nodes[j].Node.CoreNodeName
			if info, ok := binMap[name]; ok {
				views[i].Nodes[j].BinState = &store.NodeBinState{
					BinLabel:     info.BinLabel,
					PayloadCode:  info.PayloadCode,
					UOPRemaining: info.UOPRemaining,
					Occupied:     info.Occupied,
				}
			}
		}
	}
}

// enrichSingleViewBinState enriches a single station view.
func enrichSingleViewBinState(coreAPI *engine.CoreClient, view *store.OperatorStationView) {
	if view == nil {
		return
	}
	views := []store.OperatorStationView{*view}
	enrichViewBinState(coreAPI, views)
	view.Nodes = views[0].Nodes
}

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
	enrichViewBinState(h.engine.CoreAPI(), stationViews)

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
	enrichViewBinState(h.engine.CoreAPI(), stationViews)

	data := map[string]interface{}{
		"StationViews": stationViews,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "material-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
