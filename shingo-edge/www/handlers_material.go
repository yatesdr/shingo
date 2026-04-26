package www

import (
	"net/http"

	"shingoedge/engine"
	"shingoedge/store"
	"shingoedge/store/processes"
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
					BinLabel:          info.BinLabel,
					BinTypeCode:       info.BinTypeCode,
					PayloadCode:       info.PayloadCode,
					UOPRemaining:      info.UOPRemaining,
					Manifest:          info.Manifest,
					ManifestConfirmed: info.ManifestConfirmed,
					Occupied:          info.Occupied,
				}
			}
		}
	}
}


func buildStationViews(eng ServiceAccess, activeProcess *processes.Process) []store.OperatorStationView {
	if activeProcess == nil {
		return nil
	}
	stations, _ := eng.StationService().ListByProcess(activeProcess.ID)
	var views []store.OperatorStationView
	for _, station := range stations {
		if view, err := eng.StationService().BuildView(station.ID); err == nil {
			views = append(views, *view)
		}
	}
	return views
}

func (h *Handlers) handleMaterial(w http.ResponseWriter, r *http.Request) {
	processes, _ := h.engine.ProcessService().List()
	activeProcess := resolveProcessFromQuery(r, processes)

	var activeProcessID int64
	var currentStyleName, targetStyleName string

	stationViews := buildStationViews(h.engine, activeProcess)
	enrichViewBinState(h.engine.CoreAPI(), stationViews)

	if activeProcess != nil {
		activeProcessID = activeProcess.ID
		if activeProcess.ActiveStyleID != nil {
			if style, err := h.engine.StyleService().Get(*activeProcess.ActiveStyleID); err == nil {
				currentStyleName = style.Name
			}
		}
		if activeProcess.TargetStyleID != nil {
			if style, err := h.engine.StyleService().Get(*activeProcess.TargetStyleID); err == nil {
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
	processes, _ := h.engine.ProcessService().List()
	activeProcess := resolveProcessFromQuery(r, processes)

	stationViews := buildStationViews(h.engine, activeProcess)
	enrichViewBinState(h.engine.CoreAPI(), stationViews)

	data := map[string]interface{}{
		"StationViews": stationViews,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "material-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
