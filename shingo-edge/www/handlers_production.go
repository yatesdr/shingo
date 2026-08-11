package www

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
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

func buildStationViews(ctx context.Context, eng ServiceAccess, activeProcess *domain.Process) []domain.OperatorStationView {
	if activeProcess == nil {
		return nil
	}
	stations, _ := eng.StationService().ListByProcess(activeProcess.ID)
	var views []domain.OperatorStationView
	for _, station := range stations {
		if ctx.Err() != nil {
			break
		}
		if view, err := eng.StationService().BuildView(ctx, station.ID); err == nil {
			views = append(views, *view)
		}
	}
	return views
}

func buildLinesideRows(eng ServiceAccess) []linesideBucketRow {
	processList, _ := eng.ProcessService().List()
	allNodes, _ := eng.ProcessService().ListNodes()
	stations, _ := eng.StationService().List()
	allStyles, _ := eng.StyleService().List()

	processName := make(map[int64]string, len(processList))
	for _, p := range processList {
		processName[p.ID] = p.Name
	}
	stationName := make(map[int64]string, len(stations))
	for _, s := range stations {
		stationName[s.ID] = s.Name
	}
	styleName := make(map[int64]string, len(allStyles))
	for _, s := range allStyles {
		styleName[s.ID] = s.Name
	}

	rows := make([]linesideBucketRow, 0)
	for _, n := range allNodes {
		buckets, err := eng.ProcessService().ListLinesideBucketsForNode(n.ID)
		if err != nil || len(buckets) == 0 {
			continue
		}
		for _, b := range buckets {
			row := linesideBucketRow{
				BucketID:    b.ID,
				NodeID:      n.ID,
				NodeName:    n.Name,
				ProcessName: processName[n.ProcessID],
				StyleName:   styleName[b.StyleID],
				PartNumber:  b.PartNumber,
				PairKey:     b.PairKey,
				Qty:         b.Qty,
				State:       b.State,
			}
			if n.OperatorStationID != nil {
				row.StationName = stationName[*n.OperatorStationID]
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func (h *Handlers) handleProduction(w http.ResponseWriter, r *http.Request) {
	processes, _ := h.engine.ProcessService().List()
	activeProcess := resolveProcessFromQuery(r, processes)

	var activeProcessID int64
	var currentStyleName, targetStyleName string

	stationViews := buildStationViews(r.Context(), h.engine, activeProcess)
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

	linesideRows := buildLinesideRows(h.engine)

	shifts, _ := h.engine.ShiftService().List()
	shiftsJSON, _ := json.Marshal(shifts)
	if shiftsJSON == nil {
		shiftsJSON = []byte("[]")
	}
	todayStr := time.Now().Format("2006-01-02")
	hourlyCounts, _ := h.engine.CounterService().HourlyTotals(activeProcessID, todayStr)
	if hourlyCounts == nil {
		hourlyCounts = make(map[int]int64)
	}
	hourlyCountsJSON, _ := json.Marshal(hourlyCounts)
	if hourlyCountsJSON == nil {
		hourlyCountsJSON = []byte("{}")
	}

	anomalies, rpMap := loadAnomalyData(h)

	processNodes, _ := h.engine.ProcessService().ListNodes()
	coreNodes := h.engine.CoreNodes()
	type coreNodeOpt struct {
		Name       string `json:"name"`
		NodeType   string `json:"node_type,omitempty"`
		ParentType string `json:"parent_node_type,omitempty"`
	}
	coreNodeOpts := make([]coreNodeOpt, 0, len(coreNodes))
	for name, info := range coreNodes {
		coreNodeOpts = append(coreNodeOpts, coreNodeOpt{Name: name, NodeType: info.NodeType, ParentType: info.ParentNodeType})
	}
	type edgeNode struct {
		ID   string `json:"id"`
		Desc string `json:"desc"`
	}
	edgeNodeList := make([]edgeNode, 0, len(processNodes))
	for _, pn := range processNodes {
		edgeNodeList = append(edgeNodeList, edgeNode{ID: pn.CoreNodeName, Desc: pn.Name})
	}
	nodesJSON, _ := json.Marshal(edgeNodeList)
	coreNodesJSON, _ := json.Marshal(coreNodeOpts)

	data := map[string]any{
		"Page":              "production",
		"Processes":         processes,
		"ActiveProcessID":   activeProcessID,
		"StationViews":      stationViews,
		"CurrentStyle":      currentStyleName,
		"TargetStyle":       targetStyleName,
		"LinesideRows":      linesideRows,
		"ShiftsJSON":        template.JS(shiftsJSON),
		"HourlyCountsJSON":  template.JS(hourlyCountsJSON),
		"TodayDate":         todayStr,
		"ProcessNodes":      processNodes,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
		"NodesJSON":         string(nodesJSON),
		"CoreNodesJSON":     string(coreNodesJSON),
	}
	h.renderTemplate(w, r, "production.html", data)
}

func (h *Handlers) handleProductionPartial(w http.ResponseWriter, r *http.Request) {
	processes, _ := h.engine.ProcessService().List()
	activeProcess := resolveProcessFromQuery(r, processes)

	stationViews := buildStationViews(r.Context(), h.engine, activeProcess)
	enrichViewBinState(h.engine.CoreAPI(), stationViews)

	var activeProcessID int64
	if activeProcess != nil {
		activeProcessID = activeProcess.ID
	}

	data := map[string]any{
		"StationViews":    stationViews,
		"Processes":       processes,
		"ActiveProcessID":  activeProcessID,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "production-body", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (h *Handlers) apiListShifts(w http.ResponseWriter, r *http.Request) {
	shifts, err := h.engine.ShiftService().List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, shifts)
}

func (h *Handlers) apiSaveShifts(w http.ResponseWriter, r *http.Request) {
	var shifts []struct {
		ShiftNumber int    `json:"shift_number"`
		Name        string `json:"name"`
		StartTime   string `json:"start_time"`
		EndTime     string `json:"end_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&shifts); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	for _, s := range shifts {
		if s.ShiftNumber < 1 || s.ShiftNumber > 3 {
			continue
		}
		if s.StartTime == "" && s.EndTime == "" {
			h.engine.ShiftService().Delete(s.ShiftNumber)
			continue
		}
		if err := h.engine.ShiftService().Upsert(s.ShiftNumber, s.Name, s.StartTime, s.EndTime); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	h.requestBackup("shifts")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (h *Handlers) apiGetHourlyCounts(w http.ResponseWriter, r *http.Request) {
	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}
	processID, _ := strconv.ParseInt(r.URL.Query().Get("process_id"), 10, 64)

	if processID == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	}

	counts, err := h.engine.CounterService().HourlyTotals(processID, dateStr)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, counts)
}

func (h *Handlers) apiGetDailyCounts(w http.ResponseWriter, r *http.Request) {
	processID, _ := strconv.ParseInt(r.URL.Query().Get("process_id"), 10, 64)
	if processID == 0 {
		writeJSON(w, []any{})
		return
	}

	toDate := r.URL.Query().Get("to")
	if toDate == "" {
		toDate = time.Now().Format("2006-01-02")
	}
	fromDate := r.URL.Query().Get("from")
	if fromDate == "" {
		fromDate = time.Now().AddDate(0, 0, -90).Format("2006-01-02")
	}

	counts, err := h.engine.CounterService().DailyCounts(processID, fromDate, toDate)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, counts)
}
