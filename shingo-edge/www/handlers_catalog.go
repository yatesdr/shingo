// handlers_catalog.go — read-only views of the cached Core data
// (node list, payload catalog) plus the operator-driven re-sync triggers.

package www

import (
	"net/http"

	"shingo/protocol"
)

// --- Core Nodes ---

// coreNodeRow is one row of the node list the pickers draw from. Members is set
// on a group: the plain nodes standing in it, so a picker of positions can take
// a group as its nodes the moment it is picked (StationService.expandGroups is
// the same rule at save).
//
// SharedLoader is set on a window or position of a Core-owned loader. Such a
// node may be a position of several processes at once (StationService.SetNodes
// accepts it), so the positions checklist offers it even when another process
// already holds it, and greys out every other node another process holds.
type coreNodeRow struct {
	protocol.NodeInfo
	Members      []string `json:"members,omitempty"`
	SharedLoader bool     `json:"shared_loader,omitempty"`
}

func (h *Handlers) apiGetCoreNodes(w http.ResponseWriter, r *http.Request) {
	nodes := h.engine.CoreNodes()
	stations := h.engine.StationService()
	groups := stations.GroupMembers()
	infos := make([]coreNodeRow, 0, len(nodes))
	for _, n := range nodes {
		infos = append(infos, coreNodeRow{NodeInfo: n, Members: groups[n.Name],
			SharedLoader: stations.LoaderHolds(n.Name)})
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
