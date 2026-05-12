// handlers_process_nodes.go — process-node CRUD endpoints. Process
// nodes are the physical-floor positions a station claims; CRUD lives
// separately from the station handlers so the file names match what
// they manage.

package www

import (
	"encoding/json"
	"net/http"

	"shingoedge/domain"
)

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
		writeError(w, http.StatusInternalServerError, err.Error())
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
		writeError(w, http.StatusInternalServerError, err.Error())
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
