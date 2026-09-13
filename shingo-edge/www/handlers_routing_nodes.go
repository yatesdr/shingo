// handlers_routing_nodes.go — the process ROUTING SET endpoints and the
// flow-composer gate.
//
// The routing set is the list of nodes a process may route material through
// that are not its own positions (domain/routing_set.go). The Processes page's
// Routing panel reads and edits it here; the HMI flow composer's pickers will
// offer exactly its enabled members. Admin-gated like every other spec write.

package www

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"shingoedge/domain"
	"shingoedge/service"
)

// routingSetView is the Routing panel's whole picture: the summary line it
// shows first, the rows, and the gate.
//
// NO RAW REPORT. It shipped the whole RoutingDeriveReport beside the sentence
// built from it, and the panel read neither — it recomputed its own sentence
// from the rows, with different numbers. One sentence, the server's, because
// the counts in it (claims read, names still waiting for a decision) are the
// derivation's own and the page cannot see them.
type routingSetView struct {
	Summary             string               `json:"summary"`
	Rows                []domain.RoutingNode `json:"rows"`
	FlowComposerEnabled bool                 `json:"flow_composer_enabled"`
}

// routingNameChecker is the "needs a decision" test the backfill report
// applies to each derived name: coreNodeNameIsUnknown, reused verbatim, so
// the routing panel and the process-node write agree on what an unknown
// name is — including the bare-name rule for group children.
//
// Nil when Core's list is EMPTY. An empty list is not evidence that a name
// is wrong; it means Core has not been heard from, and the report then says
// nothing needs a decision — the absence of a check, not a clean bill. One
// log line rather than one per name.
func (h *Handlers) routingNameChecker() func(string) bool {
	if len(h.engine.CoreNodes()) == 0 {
		log.Printf("routing set: core node list is EMPTY, so no derived name could be checked against Core's plant — reporting 0 needing a decision. This is not a pass: Core has not been heard from.")
		return nil
	}
	return func(name string) bool {
		_, unknown := h.coreNodeNameIsUnknown(name)
		return unknown
	}
}

func (h *Handlers) writeRoutingSet(w http.ResponseWriter, processID int64, report domain.RoutingDeriveReport) {
	rows, err := h.engine.ProcessService().ListRoutingNodes(processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeRoutingSetRows(w, report, rows)
}

func writeRoutingSetRows(w http.ResponseWriter, report domain.RoutingDeriveReport, rows []domain.RoutingNode) {
	if rows == nil {
		rows = []domain.RoutingNode{}
	}
	writeJSON(w, routingSetView{
		Summary:             report.Line(),
		Rows:                rows,
		FlowComposerEnabled: report.FlowComposerEnabled,
	})
}

func (h *Handlers) apiListRoutingNodes(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	// ONE PASS. The response is the rows and a line describing them, and the
	// route used to compute those apart — the report re-read the same table
	// twice beside the list it was about (store/processes/routing_nodes.go,
	// RoutingSetReportFrom). The Routing tab is a tab: it is opened and
	// re-opened while an engineer works through a plant's names.
	report, rows, err := h.engine.ProcessService().RoutingSet(id, h.routingNameChecker())
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeRoutingSetRows(w, report, rows)
}

// apiDeriveRoutingNodes re-runs the backfill for one process against the
// live Core list. A no-op once the flow composer is enabled; the response
// says so.
func (h *Handlers) apiDeriveRoutingNodes(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	report, err := h.engine.ProcessService().DeriveRoutingNodes(id, h.routingNameChecker())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("routing-set-derived")
	h.writeRoutingSet(w, id, report)
}

// apiUpsertRoutingNode adds an engineer's row. origin and called_by are
// server-stamped: the input type does not accept them from the body, and the
// session user is the only author this endpoint knows.
func (h *Handlers) apiUpsertRoutingNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var in domain.RoutingNodeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in.ProcessID = id
	in.CoreNodeName = strings.TrimSpace(in.CoreNodeName)
	in.Origin = domain.RoutingOriginEngineer
	in.CalledBy, _ = h.sessions.getUser(r)
	if !domain.IsRoutingRole(in.Role) {
		writeError(w, http.StatusBadRequest, "role must be source, staging or destination")
		return
	}
	// The same guard as a process_node write: a name Core does not have is
	// refused when Core's list is there to check, and allowed (and logged)
	// when it is not.
	if msg, unknown := h.coreNodeNameIsUnknown(in.CoreNodeName); unknown {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	rowID, err := h.engine.ProcessService().UpsertRoutingNode(in)
	if err != nil {
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
	writeJSON(w, map[string]int64{"id": rowID})
}

// apiPatchRoutingNode flips one row's enabled flag. A body that says nothing is
// refused rather than treated as a no-op.
//
// THE SWITCH IS THE APPROVAL, and the SERVER is what records it. The body
// carries `enabled` and nothing else — origin and called_by are not accepted
// from a request here any more than they are on the upsert — and the store
// stamps origin='engineer' plus the session user when a row is switched on.
// A page that sent an origin of its own would be a page deciding authorship,
// which is the thing the server-stamped fields exist to prevent.
func (h *Handlers) apiPatchRoutingNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	rowID, err := parseID(r, "rowID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid routing node id")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	user, _ := h.sessions.getUser(r)
	if err := h.engine.ProcessService().SetRoutingNodeEnabled(id, rowID, *req.Enabled, user); err != nil {
		if errors.Is(err, service.ErrRoutingNodeNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("routing-node-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteRoutingNode(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	rowID, err := parseID(r, "rowID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid routing node id")
		return
	}
	if err := h.engine.ProcessService().DeleteRoutingNode(id, rowID); err != nil {
		switch {
		case errors.Is(err, service.ErrRoutingNodeInUse):
			// A precondition the engineer can clear, not a fault: the message
			// names the style whose claim still routes through the node.
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, service.ErrRoutingNodeNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	h.requestBackup("routing-node-deleted")
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiPatchProcess is the flow-composer gate. PATCH rather than a field on the
// PUT because the gate is flipped on its own, after a review of the routing
// set — never as a side effect of saving a name. Only the fields named in
// the body change; a body naming none is refused.
func (h *Handlers) apiPatchProcess(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		FlowComposerEnabled *bool `json:"flow_composer_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.FlowComposerEnabled == nil {
		writeError(w, http.StatusBadRequest, "nothing to change: flow_composer_enabled is the only PATCHable field")
		return
	}
	if err := h.engine.ProcessService().SetFlowComposerEnabled(id, *req.FlowComposerEnabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("process-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiProcessComposer is the DESKTOP's read: the composer block for a whole
// process, picture included.
//
// One endpoint and not a page's worth of them. SPEC §4 lists what the Processes
// page reads, and everything on that list already existed except this — the
// station had its composer block on the station view and the desktop had no
// equivalent door. Admin-gated with the rest of the process routes: this is the
// engineer's screen, and the shop-floor group has no business serving it.
func (h *Handlers) apiProcessComposer(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	data, err := h.engine.StationService().ComposerForProcess(processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if data == nil {
		writeError(w, http.StatusNotFound, "no such process")
		return
	}
	writeJSON(w, data)
}

// apiStationComposer is the STATION's read: the same block, station-shaped,
// fetched once when an operator opens the composer and held for the session.
//
// ITS OWN DOOR, ON THE SHOP FLOOR. The station has no login, so it cannot
// reach the admin read above — which is precisely why the whole composer used
// to ride the station view, rebuilt on every 500 ms poll and discarded. It is
// not the admin route moved: that one serves the plant map, every style's
// Advanced policy block and the press picture, and none of that belongs on
// the floor network. This serves the station's shape and nothing more.
//
// KEYED BY STATION, NOT BY PROCESS. The station id is what the board already
// has and what the URL already carries, and resolving the process here means
// a station can only ever ask for its own press.
func (h *Handlers) apiStationComposer(w http.ResponseWriter, r *http.Request) {
	stationID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	station, err := h.engine.StationService().Get(stationID)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such station")
		return
	}
	data, err := h.engine.StationService().ComposerForStation(station.ProcessID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if data == nil {
		writeError(w, http.StatusNotFound, "no such process")
		return
	}
	writeJSON(w, data)
}
