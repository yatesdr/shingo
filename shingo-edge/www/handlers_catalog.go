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
//
// Group is the node group a member is in, so a picker for a role where Core
// picks leaves members out (desktop-bodies.js legExclusion), by the same map
// the leg refusal reads (domain.CoreNodeKinds.GroupOf).
type coreNodeRow struct {
	protocol.NodeInfo
	Members      []string `json:"members,omitempty"`
	Group        string   `json:"group,omitempty"`
	SharedLoader bool     `json:"shared_loader,omitempty"`
}

func (h *Handlers) apiGetCoreNodes(w http.ResponseWriter, r *http.Request) {
	nodes := h.engine.CoreNodes()
	stations := h.engine.StationService()
	groups := stations.GroupMembers()
	kinds := h.engine.CoreNodeKinds()
	infos := make([]coreNodeRow, 0, len(nodes))
	for _, n := range nodes {
		infos = append(infos, coreNodeRow{NodeInfo: n, Members: groups[n.Name], Group: kinds.GroupOf(n.Name),
			SharedLoader: stations.LoaderHolds(n.Name)})
	}
	writeJSON(w, infos)
}

func (h *Handlers) apiSyncCoreNodes(w http.ResponseWriter, r *http.Request) {
	if err := h.orchestration.RequestNodeSync(); err != nil {
		writeSyncNotSent(w, "node list request", err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// writeSyncNotSent refuses a Sync whose ask was not sent, with the engine's
// reason and then what was not sent. Either the engine had no heartbeater to
// ask through, until main.go wires the two syncs (cmd/shingoedge
// setupKafkaSubscribers, once messaging connects; never, on an Edge with no
// broker configured): "the heartbeater is not running (messaging not connected
// yet)", the debug page's words for its heartbeat in the same window
// (apiSendManualMessage). Or Kafka did not take the one send a press makes:
// "Kafka did not take it (...)" (docket R72). Both doors used to answer ok in
// each case, the second after the sender's three tries. PINS:
// TestApiConfig_SyncBeforeTheSyncsAreWired, and messaging
// TestHeartbeater_ASyncPressSendsOnceAndAnswersWhetherKafkaTookIt.
func writeSyncNotSent(w http.ResponseWriter, what string, err error) {
	writeError(w, http.StatusServiceUnavailable, err.Error()+": no "+what+" sent")
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
	if err := h.orchestration.RequestCatalogSync(); err != nil {
		writeSyncNotSent(w, "catalog request", err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}
