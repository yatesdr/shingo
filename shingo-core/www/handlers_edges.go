package www

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"shingocore/store/registry"
)

// The edge-station identity surface.
//
// FOUR ENDPOINTS, AND THE INTERESTING ONE IS THE MISSING FIFTH. There is no
// "re-issue a uid to replacement hardware" call, because re-issuing is not an
// operation Core performs — it is the operator READING the existing uid off
// the list and typing it into the new Pi's shingoedge.yaml. That is the entire
// answer to the owner's constraint, and its whole content is that Core still
// holds the value. A self-minted first-boot UUID has no equivalent, not
// because the API is missing but because nobody but the dead SD card ever knew
// the number.
//
// All four are auth-gated: enrolling a station and rebinding one are fleet
// operations, and renaming one shows up on every board in the plant.

// apiEdges lists enrolled stations, including the binding and conflict state.
//
// This is the read the replacement-hardware procedure runs.
func (h *Handlers) apiEdges(w http.ResponseWriter, r *http.Request) {
	edges, err := h.engine.NodeService().ListEdges()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if edges == nil {
		edges = []registry.Edge{}
	}
	h.jsonOK(w, edges)
}

// apiEdgeEnroll mints a NEW station.
//
// POST /api/edges/enroll  {"display_name": "SPRINGFIELD / EDGE-2"}
//
// USE THIS ONLY FOR A STATION THAT DOES NOT EXIST YET. Enrolling replacement
// hardware would mint a second identity for one physical station and split its
// history at the moment of the swap — which is the failure the whole model
// exists to prevent, reached by the one route the model cannot block, namely
// somebody choosing the wrong action. The response says so; the field guide is
// in NodeService's enrollment comment.
func (h *Handlers) apiEdgeEnroll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName string `json:"display_name"`
	}
	// A missing/empty body is fine — display_name defaults to the uid.
	_ = json.NewDecoder(r.Body).Decode(&req)

	e, err := h.engine.NodeService().EnrollEdge(strings.TrimSpace(req.DisplayName))
	if errors.Is(err, registry.ErrAlreadyEnrolled) {
		h.jsonError(w, "station uid already enrolled", http.StatusConflict)
		return
	}
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{
		"station_uid":  e.StationUID,
		"display_name": e.DisplayName,
		"next_step": "put `station_uid: " + e.StationUID + "` in that Pi's " +
			"/etc/shingo/shingoedge.yaml, remove any `group_id:` line under messaging.kafka, " +
			"and restart shingoedge",
	})
}

// apiEdgeRename sets the operator-facing display name.
//
// POST /api/edges/rename?uid=stn-…  {"display_name": "…"}
//
// SAFE, AND THAT IS THE POINT OF THE WHOLE CHANGE. Before v66 the operator
// string and the identity were one column, so this operation rewrote a key
// that orders, mission_telemetry, outbox, node_stations, cell_targets and the
// Edge's own backup manifest were built on — a plant stop caused by typing in
// a text box. Now it writes one column nothing reads but a person.
func (h *Handlers) apiEdgeRename(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" {
		h.jsonError(w, "uid required", http.StatusBadRequest)
		return
	}
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	ok, err := h.engine.NodeService().RenameEdge(uid, strings.TrimSpace(req.DisplayName))
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		h.jsonError(w, "no enrolled station with uid "+uid, http.StatusNotFound)
		return
	}
	h.jsonSuccess(w)
}

// apiEdgeRebind moves a station's binding to a new machine and clears its
// conflict record.
//
// POST /api/edges/rebind?uid=stn-…  {"hostname": "shingo-pi-2"}
//
// THIS EXISTS SO THE ALARM CAN BE TURNED OFF, which is not a convenience.
// bound_hostname is never reassigned by a register, so after a legitimate box
// replacement every subsequent register mismatches and conflict_at stays
// permanently fresh — and a signal that cannot be cleared is a signal people
// learn to ignore. Deliberately a separate, explicit act rather than something
// a register can do to itself.
//
// Omitting hostname takes the station's current last-seen hostname, which is
// the replacement box that has already registered. That is the common case and
// it saves the operator retyping a name Core is already displaying.
func (h *Handlers) apiEdgeRebind(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" {
		h.jsonError(w, "uid required", http.StatusBadRequest)
		return
	}
	var req struct {
		Hostname string `json:"hostname"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	hostname := strings.TrimSpace(req.Hostname)

	ns := h.engine.NodeService()
	if hostname == "" {
		e, err := ns.GetEdge(uid)
		if errors.Is(err, sql.ErrNoRows) {
			h.jsonError(w, "no enrolled station with uid "+uid, http.StatusNotFound)
			return
		}
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		hostname = e.Hostname
	}
	ok, err := ns.RebindEdge(uid, hostname)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		h.jsonError(w, "no enrolled station with uid "+uid, http.StatusNotFound)
		return
	}
	h.jsonOK(w, map[string]any{"station_uid": uid, "bound_hostname": hostname})
}
