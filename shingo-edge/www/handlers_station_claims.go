// handlers_station_claims.go — station-side claim management
// (which process nodes a station owns) and the AB-side flip used by
// two-side lineside layouts. Distinct from style-node claims
// (handlers_styles.go), which are about which physical positions
// a STYLE expects to occupy.

package www

import (
	"encoding/json"
	"errors"
	"net/http"

	"shingoedge/engine"
	"shingoedge/service"
)

// apiCreateLoaderBoard makes the operator screen for a Core loader that has
// none, and binds the loader's windows to it in one action.
//
// process_id is required and is NOT inferred here even when the service could
// guess: Core sends every loader to every edge, so which edge and which process
// owns a loader's windows is a human decision, and the screen that offers the
// button is where it gets made.
func (h *Handlers) apiCreateLoaderBoard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoaderKey string `json:"loader_key"`
		ProcessID int64  `json:"process_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.StationService().CreateLoaderBoard(req.LoaderKey, req.ProcessID)
	if err != nil {
		// An unknown window name is bad config, not a server fault, and the
		// message names which one.
		if errors.Is(err, service.ErrUnknownCoreNodes) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "station-nodes-updated"}})
	writeJSON(w, map[string]any{"status": "ok", "station_id": id})
}

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
		// A name Core does not have is bad input, not a server fault — and the
		// message names which one, so a 400 puts it in front of whoever typed it
		// instead of in a log.
		if errors.Is(err, service.ErrUnknownCoreNodes) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "station-nodes-updated"}})
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiSetActivePullSide is the operator's declaration of which side of an A/B
// pair the line is drawing from — the second door onto active_pull, beside the
// flip.
//
// It sits next to apiFlipABNode because that is where the fact lives, and the
// two are not the same click: the flip MOVES the line and writes the bit as part
// of doing so; this one only records what is already true. The state it exists
// for is a tooling evacuate, which darkens both sides correctly and leaves
// nothing to light either again — see engine.SetActivePullSide.
//
// No `confirm` field: there is nothing to override. The operator's statement IS
// the input, and it is audited by name at the engine.
func (h *Handlers) apiSetActivePullSide(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		CalledBy string `json:"called_by"`
	}
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.CalledBy == "" {
		req.CalledBy = "operator_station"
	}
	if err := h.orchestration.SetActivePullSide(id, engine.FlipRequest{
		CalledBy: req.CalledBy,
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiFlipABNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	// The readiness guard refuses an unready target with a reason and asks for
	// a confirm — the same refuse-then-confirm shape the release prompt uses. A
	// body is optional so the plain click keeps working; `confirm` is the second
	// click, and `called_by` names who made it in the audit line.
	var req struct {
		Confirm  bool   `json:"confirm"`
		CalledBy string `json:"called_by"`
	}
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.CalledBy == "" {
		req.CalledBy = "operator_station"
	}
	if err := h.orchestration.FlipABNode(id, engine.FlipRequest{
		Confirm: req.Confirm, CalledBy: req.CalledBy,
	}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}
