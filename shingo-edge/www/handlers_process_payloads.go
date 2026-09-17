// handlers_process_payloads.go — the process PART SET endpoint, and the
// routing set's list-shaped write beside it.
//
// TWO LISTS, TWO BODIES, TWO REFUSALS. Add Process asks an engineer three
// questions at once (what the cell is, what screen works it, and what it may
// route and run) and used to answer the last of them with one POST per name:
// six routing names was six round trips on a Pi with one SQLite connection,
// each its own transaction, each able to half-land. These take the list.
//
// NEITHER BROADCASTS material-refresh. Every board on the plant answers that
// event with a view build (operator.js's onMaterialRefresh → scheduleRefresh),
// and a part set and a routing set are engineering facts: no tile on any board
// changes when one is written. The writes that DO broadcast are the ones that
// move material or change what a screen owns — a claim, an active style, a
// station's node list.

package www

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"shingoedge/domain"
	"shingoedge/service"
)

// apiListProcessPayloads is the STORED half of the part set — what the sheet
// that edits it opens on.
//
// THE STORED HALF AND NOT THE PALETTE, which is the union with what the
// process's live claims already run (store/processes.ProcessPalette). A sheet
// opened on the union would let an engineer untick a part they cannot remove:
// the claim keeps it offered, so Save would look like it had done nothing. The
// composer reads the palette; this one is for the editor.
func (h *Handlers) apiListProcessPayloads(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	codes, err := h.engine.ProcessService().ListProcessPayloads(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if codes == nil {
		codes = []string{}
	}
	writeJSON(w, codes)
}

// apiSetProcessPayloads replaces a process's part set with the list in the
// body. A SET-TO, because the picker holds the whole list and an engineer who
// unticks a part means it should stop being offered.
//
// AN EMPTY LIST IS AN ANSWER, not a body to refuse: "this cell has no part set
// of its own" is a real state, and the palette is still the union with what
// its claims run, so an empty set never empties a working picker.
func (h *Handlers) apiSetProcessPayloads(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		Payloads []string `json:"payloads"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.ProcessService().ReplaceProcessPayloads(id, req.Payloads); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-payloads-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiPutRoutingNodes writes a whole routing list in one trip.
//
// VALIDATED WHOLE, THEN WRITTEN. A bad role or a name Core does not have
// refuses the BODY, before any of it lands — the same rule SetNodes states for
// the station's node list: a partial write reports success for a set the
// caller never asked for, and nothing says which half landed.
//
// IT ADDS AND ADOPTS AND NEVER DELETES. See service.PutRoutingNodes for why a
// set-to is the wrong shape for a table whose delete is refused while a live
// claim routes through the name.
func (h *Handlers) apiPutRoutingNodes(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		Nodes []struct {
			CoreNodeName string `json:"core_node_name"`
			Role         string `json:"role"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows := make([]domain.RoutingNodeInput, 0, len(req.Nodes))
	for _, n := range req.Nodes {
		name := strings.TrimSpace(n.CoreNodeName)
		if name == "" {
			writeError(w, http.StatusBadRequest, "a routing row needs a node name")
			return
		}
		if !domain.IsRoutingRole(n.Role) {
			writeError(w, http.StatusBadRequest,
				name+": role must be source, staging or destination, not "+n.Role)
			return
		}
		// The same guard as a process_node write and as the single-row POST: a
		// name Core does not have is refused when Core's list is there to check,
		// and allowed (and logged) when it is not.
		if msg, unknown := h.coreNodeNameIsUnknown(name); unknown {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		rows = append(rows, domain.RoutingNodeInput{CoreNodeName: name, Role: n.Role})
	}
	user, _ := h.sessions.getUser(r)
	if err := h.engine.ProcessService().PutRoutingNodes(id, rows, user); err != nil {
		switch {
		case errors.Is(err, service.ErrRoutingNodeIsPosition):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, service.ErrInvalidRoutingRole):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	h.requestBackup("routing-node-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}
