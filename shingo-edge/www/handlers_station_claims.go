// handlers_station_claims.go — station-side claim management
// (which process nodes a station owns) and the AB-side flip used by
// two-side lineside layouts. Distinct from style-node claims
// (handlers_styles.go), which are about which physical positions
// a STYLE expects to occupy.

package www

import (
	"encoding/json"
	"net/http"
)

func (h *Handlers) apiGetStationClaimedNodes(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	names, err := h.engine.StationService().GetNodeNames(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, names)
}

func (h *Handlers) apiSetStationClaimedNodes(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	var req struct {
		Nodes []string `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.StationService().SetNodes(id, req.Nodes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "station-nodes-updated"}})
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiFlipABNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.orchestration.FlipABNode(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}
