// handlers_station_claims.go — station-side claim management
// (which process nodes a station owns). Distinct from style-node claims
// (handlers_styles.go), which are about which physical positions
// a STYLE expects to occupy.

package www

import (
	"encoding/json"
	"errors"
	"net/http"

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
	kept, err := h.engine.StationService().SetNodes(id, req.Nodes)
	if err != nil {
		// A name Core does not have, or a slot another process already models,
		// is bad input, not a server fault — and the message names which one, so
		// a 400 puts it in front of whoever typed it instead of in a log.
		if errors.Is(err, service.ErrUnknownCoreNodes) || errors.Is(err, service.ErrPositionOfOtherProcess) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// kept: removed positions that stayed, disabled, because an order is still
	// running at them. Always present so the page need not tell "none" from
	// "not reported".
	if kept == nil {
		kept = []string{}
	}
	writeJSON(w, map[string]any{"status": "ok", "kept": kept})
}

// apiStationRemovalImpact answers the screen sheet before it saves a list
// that takes positions off the station: the recipes and styles that still
// name them (StationService.RemovalImpact). It writes nothing; the body is the
// claimed-nodes PUT's own, and an empty warning means go ahead.
func (h *Handlers) apiStationRemovalImpact(w http.ResponseWriter, r *http.Request) {
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
	warning, err := h.engine.StationService().RemovalImpact(id, req.Nodes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"warning": warning})
}
