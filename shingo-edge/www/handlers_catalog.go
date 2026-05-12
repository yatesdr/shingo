// handlers_catalog.go — read-only views of the cached Core data
// (node list, payload catalog) plus the operator-driven re-sync triggers.

package www

import (
	"net/http"

	"shingo/protocol"
)

// --- Core Nodes ---

func (h *Handlers) apiGetCoreNodes(w http.ResponseWriter, r *http.Request) {
	nodes := h.engine.CoreNodes()
	infos := make([]protocol.NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		infos = append(infos, n)
	}
	writeJSON(w, infos)
}

func (h *Handlers) apiSyncCoreNodes(w http.ResponseWriter, r *http.Request) {
	h.orchestration.RequestNodeSync()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiListPayloadCatalog(w http.ResponseWriter, r *http.Request) {
	entries, err := h.engine.CatalogService().List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, entries)
}

func (h *Handlers) apiSyncPayloadCatalog(w http.ResponseWriter, r *http.Request) {
	h.orchestration.RequestCatalogSync()
	writeJSON(w, map[string]string{"status": "ok"})
}
