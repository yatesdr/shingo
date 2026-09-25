package www

import "net/http"

// linesideBucketRow is the per-pile view-model for the lineside table. One row
// per node_lineside_bucket entry: an active pile (parts pulled to lineside at a
// release, draining until the cutover) or a stranded one (what an active pile
// had left at a cutover, shown as a count anomaly). Engineers use it to clear
// a pile without restarting the edge service.
//
// It is built by buildLinesideRows (handlers_production.go) and rendered on the
// PRODUCTION page. It had a standalone admin page too, whose handler this file
// still carried after the table moved — routed from nowhere, rendering a
// template nothing else referenced, and duplicating buildLinesideRows line for
// line. Both are gone; the Clear endpoint below is the live half and is
// routed (router.go, "embedded on Production page").
type linesideBucketRow struct {
	BucketID    int64
	NodeID      int64
	NodeName    string
	StationName string
	ProcessName string
	PayloadCode string
	Qty         int
	State       string
}

// apiAdminClearLinesideBucket deletes one pile, active or stranded, and
// sends its level (0) to Core. Unconditional, and the only admin write to a
// pile: there is no qty edit, because a pile exists only for parts a bin paid
// for.
func (h *Handlers) apiAdminClearLinesideBucket(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid bucket id")
		return
	}
	if err := h.orchestration.AdminClearLinesideBucket(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshProduction")
}
