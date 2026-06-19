package www

import (
	"net/http"
	"time"

	"shingoedge/domain"
	"shingoedge/engine"
)

// enrichViewBinState fetches bin state from Core and attaches it to each node in the views.
func enrichViewBinState(coreAPI *engine.CoreClient, views []domain.OperatorStationView) {
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
				views[i].Nodes[j].BinState = &domain.NodeBinState{
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

func buildStationViews(eng ServiceAccess, activeProcess *domain.Process) []domain.OperatorStationView {
	if activeProcess == nil {
		return nil
	}
	stations, _ := eng.StationService().ListByProcess(activeProcess.ID)
	var views []domain.OperatorStationView
	for _, station := range stations {
		if view, err := eng.StationService().BuildView(station.ID); err == nil {
			views = append(views, *view)
		}
	}
	return views
}

func (h *Handlers) handleHome(w http.ResponseWriter, r *http.Request) {
	anomalies, rpMap := loadAnomalyData(h)

	// System health
	coreAvailable := h.engine.CoreAPI() != nil && h.engine.CoreAPI().Available()
	warLinkConnected := false
	warLinkErrorMsg := ""
	if plcMgr := h.engine.PLCManager(); plcMgr != nil {
		warLinkConnected = plcMgr.IsWarLinkConnected()
		if err := plcMgr.WarLinkError(); err != nil {
			warLinkErrorMsg = err.Error()
		}
	}

	// Active orders
	activeOrders, _ := h.engine.OrderService().ListActive()

	// Processes with styles
	processes, _ := h.engine.ProcessService().List()
	type processSummary struct {
		Name          string
		ActiveStyle   string
		TargetStyle   string
		CounterPLC    string
		CounterTag    string
		CounterOn     bool
	}
	var procSummaries []processSummary
	for _, p := range processes {
		var activeStyle, targetStyle string
		if p.ActiveStyleID != nil {
			if s, err := h.engine.StyleService().Get(*p.ActiveStyleID); err == nil {
				activeStyle = s.Name
			}
		}
		if p.TargetStyleID != nil {
			if s, err := h.engine.StyleService().Get(*p.TargetStyleID); err == nil {
				targetStyle = s.Name
			}
		}
		procSummaries = append(procSummaries, processSummary{
			Name:        p.Name,
			ActiveStyle: activeStyle,
			TargetStyle: targetStyle,
			CounterPLC:  p.CounterPLCName,
			CounterTag:  p.CounterTagName,
			CounterOn:   p.CounterEnabled,
		})
	}

	// Today's production totals
	today := time.Now().Format("2006-01-02")
	var todayTotal int64
	for _, p := range processes {
		totals, err := h.engine.CounterService().HourlyTotals(p.ID, today)
		if err == nil {
			for _, v := range totals {
				todayTotal += v
			}
		}
	}

	data := map[string]any{
		"Page":            "home",
		"Anomalies":        anomalies,
		"ReportingPointMap": rpMap,
		"CoreAvailable":    coreAvailable,
		"WarLinkConnected": warLinkConnected,
		"WarLinkError":     warLinkErrorMsg,
		"ActiveOrderCount": len(activeOrders),
		"TodayProduction":  todayTotal,
		"Processes":        procSummaries,
	}
	h.renderTemplate(w, r, "home.html", data)
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
	data := map[string]any{
		"Page":              "material",
		"Processes":         processes,
		"ActiveProcessID":   activeProcessID,
		"StationViews":      stationViews,
		"CurrentStyle":      currentStyleName,
		"TargetStyle":       targetStyleName,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
	}
	h.renderTemplate(w, r, "material.html", data)
}

func (h *Handlers) handleMaterialPartial(w http.ResponseWriter, r *http.Request) {
	processes, _ := h.engine.ProcessService().List()
	activeProcess := resolveProcessFromQuery(r, processes)

	stationViews := buildStationViews(h.engine, activeProcess)
	enrichViewBinState(h.engine.CoreAPI(), stationViews)

	data := map[string]any{
		"StationViews": stationViews,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "material-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
