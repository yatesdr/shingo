// handlers_station_claims.go — station-side claim management
// (which process nodes a station owns), and the same write for the
// process's positions that are on no screen. Distinct from style-node claims
// (handlers_styles.go), which are about which physical positions
// a STYLE expects to occupy.

package www

import (
	"database/sql"
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
	svc := h.engine.StationService()
	kept, err := svc.SetNodes(id, req.Nodes)
	if err != nil {
		if !writePositionsRefusal(w, err) {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// One read after the write: the list the claimed-nodes GET would answer.
	nodes, err := svc.GetNodeNames(id)
	writePositionsSaved(w, kept, nodes, err)
}

// apiSetProcessPositions is PUT /api/processes/{id}/positions: the process's
// positions on NO screen, set in one write (StationService.SetLoosePositions)
// — the claimed-nodes PUT's body and refusals, over the rows whose
// operator_station_id is NULL. A press is set up before it has a screen, and
// its positions exist from the setup; a screen added later takes them with
// its own claimed-nodes PUT.
//
// The reply is {"status":"ok","kept":[...]} and no more: kept as in
// writePositionsSaved, but no nodes and no read-back to fill it. The screen
// write's nodes is there because the page keeps a screen's list from the
// reply; the page draws the positions on no screen from its own draft and
// never read this route's list, so the read that filled it cost a statement
// per save for nothing.
//
// A position that is on one of the process's screens is refused by name
// (service.ErrPositionOnAScreen), never moved off the screen by this write.
// A process that does not exist is 404: the save reads it first.
func (h *Handlers) apiSetProcessPositions(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		Nodes []string `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	svc := h.engine.StationService()
	kept, err := svc.SetLoosePositions(id, req.Nodes)
	if err != nil {
		switch {
		case writePositionsRefusal(w, err):
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, "unknown process")
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if kept == nil {
		kept = []string{}
	}
	writeJSON(w, map[string]any{"status": "ok", "kept": kept})
}

// writePositionsRefusal maps the refusals both position writes share. A name
// Core does not have, a slot another process already models, or (the loose
// write's) a position on a screen is bad input, not a server fault — and the
// message names which one, so a 400 puts it in front of whoever typed it
// instead of in a log. Reports whether it wrote.
func writePositionsRefusal(w http.ResponseWriter, err error) bool {
	if errors.Is(err, service.ErrUnknownCoreNodes) || errors.Is(err, service.ErrPositionOfOtherProcess) ||
		errors.Is(err, service.ErrPositionOnAScreen) {
		writeError(w, http.StatusBadRequest, err.Error())
		return true
	}
	return false
}

// writePositionsSaved is the screen write's reply (the claimed-nodes PUT):
// {"status":"ok","kept":[...],"nodes":[...]}. The loose write answers its
// status and kept only (apiSetProcessPositions).
//
// kept: removed positions that stayed, disabled, because an order is still
// running at them. nodes: the list the write leaves, read once after it,
// exactly what the screen's claimed-nodes GET answers, so the page takes its
// read-back from the reply instead of a second GET. It is the server's
// reading, not an echo: a group sent comes back as its nodes, and a kept
// position comes back because it is still there. Both are always arrays, so
// the page need not tell "none" from "not reported".
//
// A read-back that fails after the write landed is a 500 that says so, rather
// than an "ok" with a list missing or a refusal that did not happen.
func writePositionsSaved(w http.ResponseWriter, kept, nodes []string, readErr error) {
	if readErr != nil {
		writeError(w, http.StatusInternalServerError, "the positions were saved, but could not be read back: "+readErr.Error())
		return
	}
	if kept == nil {
		kept = []string{}
	}
	if nodes == nil {
		nodes = []string{}
	}
	writeJSON(w, map[string]any{"status": "ok", "kept": kept, "nodes": nodes})
}

// apiStationRemovalImpact answers the screen sheet before it saves a list
// that takes positions off the station: the recipes and styles that still
// name them, and the light curtain, station counter and auto-reorder off the
// take-off clears (StationService.RemovalImpact). It writes nothing; the body
// is the claimed-nodes PUT's own, and an empty warning means go ahead.
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

// apiProcessPositionsImpact is apiStationRemovalImpact for the positions on
// no screen: the body is PUT /api/processes/{id}/positions' own, the answer
// the recipes and styles that still name a position the list drops and what
// the take-off clears there (StationService.LooseRemovalImpact). It writes
// nothing; an empty warning means go ahead.
func (h *Handlers) apiProcessPositionsImpact(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		Nodes []string `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	warning, err := h.engine.StationService().LooseRemovalImpact(id, req.Nodes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"warning": warning})
}
