package www

import (
	"encoding/json"
	"net/http"
	"strconv"

	"shingoedge/domain"
	"shingoedge/engine"
)

// linesideBucketRow is the per-slot view-model rendered on the admin
// "Lineside Buckets" page. One row per process_node that currently has
// an active_bin_id pointer (i.e., edge believes a bin is at this slot).
type linesideBucketRow struct {
	NodeID       int64
	NodeName     string
	CoreNodeName string
	StationName  string
	ProcessName  string
	BinID        int64
	BinLabel     string
	PayloadCode  string
	UOPCount     int
	Capacity     int
}

func (h *Handlers) handleLinesideBuckets(w http.ResponseWriter, r *http.Request) {
	processList, _ := h.engine.ProcessService().List()
	allNodes, _ := h.engine.ProcessService().ListNodes()
	stations, _ := h.engine.StationService().List()

	processName := make(map[int64]string, len(processList))
	activeStyleByProcess := make(map[int64]int64, len(processList))
	for _, p := range processList {
		processName[p.ID] = p.Name
		if p.ActiveStyleID != nil {
			activeStyleByProcess[p.ID] = *p.ActiveStyleID
		}
	}

	stationName := make(map[int64]string, len(stations))
	for _, s := range stations {
		stationName[s.ID] = s.Name
	}

	// Build (styleID, coreNodeName) → claim lookup for nodes whose process
	// has an active style. Capacity comes off the claim.
	claimByKey := make(map[string]*domain.NodeClaim)
	for _, p := range processList {
		if p.ActiveStyleID == nil {
			continue
		}
		claims, _ := h.engine.StyleService().ListClaims(*p.ActiveStyleID)
		for i := range claims {
			c := claims[i]
			claimByKey[claimKey(*p.ActiveStyleID, c.CoreNodeName)] = &c
		}
	}

	// One bulk Core call for bin metadata across every node we'll show.
	// Edge has no local bins table — bin label / payload code live on Core.
	occupiedNodes := make([]domain.Node, 0, len(allNodes))
	occupiedRuntime := make(map[int64]*domain.RuntimeState)
	for i := range allNodes {
		n := allNodes[i]
		rt, err := h.engine.ProcessService().EnsureNodeRuntime(n.ID)
		if err != nil || rt == nil || rt.ActiveBinID == nil {
			continue
		}
		occupiedNodes = append(occupiedNodes, n)
		occupiedRuntime[n.ID] = rt
	}

	binByNode := make(map[string]engine.NodeBinInfo)
	if len(occupiedNodes) > 0 {
		names := make([]string, 0, len(occupiedNodes))
		for _, n := range occupiedNodes {
			names = append(names, n.CoreNodeName)
		}
		bins, _ := h.engine.CoreAPI().FetchNodeBins(names)
		for _, b := range bins {
			binByNode[b.NodeName] = b
		}
	}

	rows := make([]linesideBucketRow, 0, len(occupiedNodes))
	for _, n := range occupiedNodes {
		rt := occupiedRuntime[n.ID]
		row := linesideBucketRow{
			NodeID:       n.ID,
			NodeName:     n.Name,
			CoreNodeName: n.CoreNodeName,
			ProcessName:  processName[n.ProcessID],
			UOPCount:     rt.RemainingUOPCached,
		}
		if rt.ActiveBinID != nil {
			row.BinID = *rt.ActiveBinID
		}
		if n.OperatorStationID != nil {
			row.StationName = stationName[*n.OperatorStationID]
		}
		if styleID, ok := activeStyleByProcess[n.ProcessID]; ok {
			if claim, ok := claimByKey[claimKey(styleID, n.CoreNodeName)]; ok {
				row.Capacity = claim.UOPCapacity
			}
		}
		if b, ok := binByNode[n.CoreNodeName]; ok {
			row.BinLabel = b.BinLabel
			row.PayloadCode = b.PayloadCode
		}
		rows = append(rows, row)
	}

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]interface{}{
		"Page":              "lineside-buckets",
		"Rows":              rows,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
	}
	h.renderTemplate(w, r, "lineside-buckets.html", data)
}

func claimKey(styleID int64, coreNodeName string) string {
	return coreNodeName + "@" + strconv.FormatInt(styleID, 10)
}

// apiAdminClearLinesideSlot nulls active_bin_id and zeroes the runtime UOP
// for the given node. Engineer override — does not emit a bin delta.
func (h *Handlers) apiAdminClearLinesideSlot(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.orchestration.AdminAdjustLinesideUOP(id, 0, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

// apiAdminEditLinesideUOP applies a count override to the active bin at the
// given node. Body: {"count": N}. N is capped to [0, claim.UOPCapacity] in
// the engine method.
func (h *Handlers) apiAdminEditLinesideUOP(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var body struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := h.orchestration.AdminAdjustLinesideUOP(id, body.Count, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}
