package www

import (
	"encoding/json"
	"net/http"
)

// linesideBucketRow is the per-bucket view-model rendered on the admin
// "Lineside Buckets" page. One row per node_lineside_bucket entry — the
// chip the operator HMI shows for parts pulled to lineside during a
// release. Engineers use this page to clear stuck buckets or correct
// drifted qtys without restarting the edge service.
type linesideBucketRow struct {
	BucketID    int64
	NodeID      int64
	NodeName    string
	StationName string
	ProcessName string
	StyleName   string
	PartNumber  string
	PairKey     string
	Qty         int
	State       string
}

func (h *Handlers) handleLinesideBuckets(w http.ResponseWriter, r *http.Request) {
	processList, _ := h.engine.ProcessService().List()
	allNodes, _ := h.engine.ProcessService().ListNodes()
	stations, _ := h.engine.StationService().List()
	allStyles, _ := h.engine.StyleService().List()

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
		buckets, err := h.engine.ProcessService().ListLinesideBucketsForNode(n.ID)
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

	anomalies, rpMap := loadAnomalyData(h)
	data := map[string]any{
		"Page":              "lineside-buckets",
		"Rows":              rows,
		"Anomalies":         anomalies,
		"ReportingPointMap": rpMap,
	}
	h.renderTemplate(w, r, "lineside-buckets.html", data)
}

// apiAdminClearLinesideBucket sets the bucket qty to 0, deleting the
// row on edge and emitting a LinesideBucketDelta to Core.
func (h *Handlers) apiAdminClearLinesideBucket(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid bucket id")
		return
	}
	if err := h.orchestration.AdminAdjustLinesideBucket(id, 0, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

// apiAdminEditLinesideBucketQty sets the bucket to a specific qty.
// Body: {"qty": N}. Negative qty is rejected by the engine method.
func (h *Handlers) apiAdminEditLinesideBucketQty(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid bucket id")
		return
	}
	var body struct {
		Qty int `json:"qty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := h.orchestration.AdminAdjustLinesideBucket(id, body.Qty, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}
