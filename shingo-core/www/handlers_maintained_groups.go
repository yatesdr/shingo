package www

import (
	"encoding/json"
	"net/http"
	"strconv"

	"shingocore/service"
)

// Maintained-group settings endpoints — the Maintained Group section of the node
// group settings modal.
//
// A maintained group is an NGRP whose EMPTY-CARRIER level Core holds: so many
// unclaimed carriers of each declared type, at all times, near the equipment that
// consumes them. Everything these endpoints write is INERT — no level keeper
// reads it yet.
//
// FOUR WRITE ENDPOINTS, ONE PER THING AN OPERATOR EDITS, rather than one
// save-everything call. The section has independent halves — the four scalars,
// one level line, the whole level set, the supported processes — and a single
// endpoint that took all of them would have to decide what an omitted field
// means. Omitted-means-clear silently deletes a level when the form fails to
// populate; omitted-means-keep makes clearing impossible. Per-thing endpoints
// have no such question to answer.
//
// The scalars go through setNodePropertyAudited, the same call
// /api/nodes/properties/set makes, so they arrive with the identical old→new
// audit row. That is the whole reason they are properties rather than columns on
// a table of their own.

// maintainedGroupSettings is the four scalars, as one save.
//
// They travel together because they are one control cluster on one screen and an
// operator flipping "maintain this group" while naming its station is making ONE
// decision. Each is still written through the property path individually, so the
// audit trail reads as four keys changing, which is what happened.
type maintainedGroupSettings struct {
	GroupNodeID         int64  `json:"group_node_id"`
	MaintainEnabled     bool   `json:"maintain_enabled"`
	StrictSourcing      bool   `json:"strict_sourcing"`
	MaintenanceStation  string `json:"maintenance_station"`
	OverflowDestination string `json:"overflow_destination"`
	// Force carries the operator's answer to the holds-bins guard. It overrides
	// the FLOOR check only — the save-time rules have no force, because "this
	// group is already a loader's staging group" does not become true because
	// somebody clicked again.
	Force bool `json:"force"`
}

// apiMaintainedGroup returns one group's whole maintained-group configuration.
//
// GET /api/nodes/maintained-group?id=<group node id>
func (h *Handlers) apiMaintainedGroup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id == 0 {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	cfg, err := h.engine.NodeService().GetMaintainedGroup(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, cfg)
}

// apiMaintainedGroupProcessOptions returns the picker's contents: each process
// with the Core nodes its claims resolve to.
//
// GET /api/nodes/process-options
func (h *Handlers) apiMaintainedGroupProcessOptions(w http.ResponseWriter, r *http.Request) {
	opts, err := h.engine.NodeService().ListProcessNodeOptions()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{"processes": opts})
}

// apiMaintainedGroupCheckTypes runs the holds-bins guard for a pending Allowed
// Bins change WITHOUT writing anything.
//
// POST /api/nodes/maintained-group/check-types
//
// It exists because that control does not save through JSON — it rides the node
// form's ordinary POST, which navigates. A 409 there would replace the page with
// an error document instead of asking a question. So the browser asks first,
// through this, and carries the answer into the form as force; the form handler
// still runs the same guard, so a caller that skips the question is still
// refused.
func (h *Handlers) apiMaintainedGroupCheckTypes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID     int64   `json:"node_id"`
		BinTypeIDs []int64 `json:"bin_type_ids"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.NodeID == 0 {
		h.jsonError(w, "node_id is required", http.StatusBadRequest)
		return
	}
	g, err := h.engine.NodeService().CheckMaintainedGroupTypesChange(req.NodeID, req.BinTypeIDs, false)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{"blocked": g.Blocked})
}

// apiMaintainedGroupSettingsSet writes the four scalars.
//
// POST /api/nodes/maintained-group/settings
func (h *Handlers) apiMaintainedGroupSettingsSet(w http.ResponseWriter, r *http.Request) {
	var req maintainedGroupSettings
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.GroupNodeID == 0 {
		h.jsonError(w, "group_node_id is required", http.StatusBadRequest)
		return
	}
	set := service.MaintainedGroupSettings{
		MaintainEnabled:     req.MaintainEnabled,
		StrictSourcing:      req.StrictSourcing,
		MaintenanceStation:  req.MaintenanceStation,
		OverflowDestination: req.OverflowDestination,
	}
	// RULES FIRST, WRITE SECOND — and nothing at all is written when they
	// refuse. Four properties written one at a time have no transaction around
	// them, so a refusal discovered halfway would leave a group configured half
	// the way the operator asked and half the way it was.
	chk, err := h.engine.NodeService().CheckMaintainedGroupSettings(req.GroupNodeID, set)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.maintainedGroupRefused(w, chk) {
		return
	}
	// The floor guard, second: turning maintenance off or the reserve on changes
	// what the carriers standing in the group MEAN, and force is the operator
	// saying they already know.
	g, err := h.engine.NodeService().CheckMaintainedGroupSettingsChange(req.GroupNodeID, set, req.Force)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.maintainedGroupHeld(w, g) {
		return
	}
	// The service names the keys and the on/off spelling; this loop does the
	// writing, because the audit row can only be appended where the actor is
	// known. Four rows, one per key, which is what happened.
	for _, wr := range service.MaintainedGroupPropertyWrites(set) {
		if err := h.setNodePropertyAudited(req.GroupNodeID, wr.Key, wr.Value); err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	h.maintainedGroupOK(w, chk, g)
}

// apiMaintainedGroupLevelSet declares how many empties of one type a group holds.
//
// POST /api/nodes/maintained-group/level
func (h *Handlers) apiMaintainedGroupLevelSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupNodeID int64 `json:"group_node_id"`
		BinTypeID   int64 `json:"bin_type_id"`
		Want        int   `json:"want"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.GroupNodeID == 0 || req.BinTypeID == 0 {
		h.jsonError(w, "group_node_id and bin_type_id are required", http.StatusBadRequest)
		return
	}
	// want=0 is a legitimate declared value ("none of this type right now") and
	// is NOT the same as removing the line, so it is not rejected. Negative is
	// refused by the table's CHECK and, ahead of it, by the service — which
	// gives the operator a sentence instead of a constraint-violation string.
	chk, err := h.engine.NodeService().SetMaintainLevel(req.GroupNodeID, req.BinTypeID, req.Want)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.maintainedGroupRefused(w, chk) {
		return
	}
	// No floor guard on a level line: declaring or undeclaring a carrier type
	// changes what Core HOLDS, not what a resident is allowed to be.
	h.maintainedGroupOK(w, chk, service.MaintainedGroupGuard{})
}

// apiMaintainedGroupLevelRemove stops declaring a carrier type for a group.
//
// POST /api/nodes/maintained-group/level/remove
func (h *Handlers) apiMaintainedGroupLevelRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupNodeID int64 `json:"group_node_id"`
		BinTypeID   int64 `json:"bin_type_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.GroupNodeID == 0 || req.BinTypeID == 0 {
		h.jsonError(w, "group_node_id and bin_type_id are required", http.StatusBadRequest)
		return
	}
	chk, err := h.engine.NodeService().RemoveMaintainLevel(req.GroupNodeID, req.BinTypeID)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.maintainedGroupRefused(w, chk) {
		return
	}
	// No floor guard on a level line: declaring or undeclaring a carrier type
	// changes what Core HOLDS, not what a resident is allowed to be.
	h.maintainedGroupOK(w, chk, service.MaintainedGroupGuard{})
}

// apiMaintainedGroupSupportsSet replaces the set of process nodes a group serves.
//
// POST /api/nodes/maintained-group/supports
//
// NODE IDS ON THE WIRE, resolved by the browser from the process options it was
// handed. The editor speaks process; what is stored and later enforced is nodes,
// and the resolution happens once at save time because a claim lives on the Edge
// and Core cannot read one when it matters.
func (h *Handlers) apiMaintainedGroupSupportsSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupNodeID    int64   `json:"group_node_id"`
		ProcessNodeIDs []int64 `json:"process_node_ids"`
		Force          bool    `json:"force"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.GroupNodeID == 0 {
		h.jsonError(w, "group_node_id is required", http.StatusBadRequest)
		return
	}
	// The floor guard runs BEFORE the write, and only on a narrowing: adding a
	// process gives more people access to what is standing there and strands
	// nobody.
	g, err := h.engine.NodeService().CheckMaintainedGroupSupportsChange(req.GroupNodeID, req.ProcessNodeIDs, req.Force)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.maintainedGroupHeld(w, g) {
		return
	}
	// An empty set is a legitimate save: it is what "this group serves nobody
	// yet" looks like, and refusing it would leave no way to undo a mistake.
	chk, err := h.engine.NodeService().SetMaintainSupports(req.GroupNodeID, req.ProcessNodeIDs)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.maintainedGroupRefused(w, chk) {
		return
	}
	h.maintainedGroupOK(w, chk, g)
}

// maintainedGroupRefused writes the refusal response and reports whether the
// caller should CONTINUE (true = nothing was refused).
//
// 409 rather than 400: the request is well-formed, and what stops it is the
// state of the plant's configuration around it — a distinction worth keeping,
// because a client cannot fix a 400 by changing anything but the request.
func (h *Handlers) maintainedGroupRefused(w http.ResponseWriter, chk service.MaintainedGroupCheck) bool {
	if err := chk.Err(); err != nil {
		h.jsonError(w, err.Error(), http.StatusConflict)
		return false
	}
	return true
}

// maintainedGroupHeld writes the holds-bins response and reports whether the
// caller should CONTINUE (true = nothing is standing in the way).
//
// 409 with needs_force, which the browser turns into a confirm dialog and
// re-sends with force. The distinction from a plain refusal matters to the
// client and to nobody else: a rule refusal is final, this one has an answer.
func (h *Handlers) maintainedGroupHeld(w http.ResponseWriter, g service.MaintainedGroupGuard) bool {
	if g.Blocked == "" {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":       g.Blocked,
		"needs_force": true,
		"drain":       g.Drain,
	})
	return false
}

// maintainedGroupOK answers a successful save, carrying any warnings with it.
//
// WARNINGS RIDE WITH SUCCESS. Every one of them describes a state a plant can
// legitimately be in mid-configuration, so none may block the save — but an
// operator who is one position short of room for a returning carrier should be
// told at the moment they made it so, not the first time something parks.
//
// The drain list rides here too, and it is the reason it is not a warning: it is
// not about the configuration being questionable, it is a list of orders already
// on their way that the reserve does not reach back and stop.
func (h *Handlers) maintainedGroupOK(w http.ResponseWriter, chk service.MaintainedGroupCheck, g service.MaintainedGroupGuard) {
	h.jsonOK(w, map[string]any{
		"status":   "ok",
		"warnings": chk.Warnings,
		"drain":    g.Drain,
	})
}
