package www

import (
	"encoding/json"
	"net/http"
)

// linesideBucketRow is the per-bucket view-model for the lineside table. One row
// per node_lineside_bucket entry — the chip the operator HMI shows for parts
// pulled to lineside during a release. Engineers use it to clear stuck buckets
// or correct drifted qtys without restarting the edge service.
//
// It is built by buildLinesideRows (handlers_production.go) and rendered on the
// PRODUCTION page. It had a standalone admin page too, whose handler this file
// still carried after the table moved — routed from nowhere, rendering a
// template nothing else referenced, and duplicating buildLinesideRows line for
// line. Both are gone; the two mutating endpoints below are the live half and
// are routed (router.go, "embedded on Production page").
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
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshProduction")
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
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshProduction")
}
