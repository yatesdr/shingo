// handlers_process_nodes.go — process-node CRUD endpoints. Process
// nodes are the physical-floor positions a station claims; CRUD lives
// separately from the station handlers so the file names match what
// they manage.

package www

import (
	"encoding/json"
	"net/http"
	"strings"

	"shingoedge/domain"
)

// writeNodeWriteError maps a failed process-node write to a status.
//
// A Core node may be modelled by exactly ONE process_nodes row per process —
// UNIQUE(process_id, core_node_name), installed by the collapse migration after
// Hopkinsville accumulated three PLN_01 rows and counted every press stroke three
// times. Mapping a node that is already mapped is now refused by the database,
// which is the point; but it is the caller's mistake, not a server fault, and it
// deserves a sentence an admin can act on rather than a 500 carrying raw SQLite.
func writeNodeWriteError(w http.ResponseWriter, in domain.NodeInput, err error) {
	if strings.Contains(err.Error(), "idx_process_nodes_process_core_name") ||
		(strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "core_node_name")) {
		writeError(w, http.StatusConflict,
			"Core node "+in.CoreNodeName+" is already mapped to a process node in this process. "+
				"A Core node belongs to one process node — move the existing one to this station instead of adding a second.")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func (h *Handlers) apiListConfiguredProcessNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.engine.ProcessService().ListNodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiListConfiguredProcessNodesByStation(w http.ResponseWriter, r *http.Request) {
	stationID, err := parseID(r, "stationID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	nodes, err := h.engine.ProcessService().ListNodesByStation(stationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, nodes)
}

func (h *Handlers) apiCreateProcessNode(w http.ResponseWriter, r *http.Request) {
	var in domain.NodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := h.engine.ProcessService().CreateNode(in)
	if err != nil {
		writeNodeWriteError(w, in, err)
		return
	}
	_, _ = h.engine.ProcessService().EnsureNodeRuntime(id)
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateProcessNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in domain.NodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.engine.ProcessService().UpdateNode(id, in); err != nil {
		writeNodeWriteError(w, in, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteProcessNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.engine.ProcessService().DeleteNode(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
