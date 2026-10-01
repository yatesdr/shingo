package www

import (
	"encoding/json"
	"net/http"

	"shingo/protocol"
)

// apiReleasePoints answers what releasing each named order would let its
// robot do next: the nodes its pending segment enters, the lifts it waits on,
// and the lineside bin it departs with (dispatch.ReleasePoints).
// POST /api/release/points
func (h *Handlers) apiReleasePoints(w http.ResponseWriter, r *http.Request) {
	var req protocol.ReleasePointsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "release points: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.StationID == "" {
		h.jsonError(w, "release points: station_id is required", http.StatusBadRequest)
		return
	}
	h.jsonOK(w, protocol.ReleasePointsResponse{
		Points: h.engine.Dispatcher().ReleasePoints(req.StationID, req.OrderUUIDs),
	})
}
