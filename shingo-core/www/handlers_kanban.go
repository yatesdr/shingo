package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"shingo/protocol"
	"shingocore/service"
)

// Kanban-group settings endpoints — the Kanban section of the node
// group settings modal.
//
// A kanban is an NGRP whose EMPTY-CARRIER level Core holds: so many
// unclaimed carriers of each declared type, at all times, near the equipment that
// consumes them. The level keeper (engine/kanban_keeper.go) holds the levels
// written here. The layer wants that ride a level's save tell the unloader
// which heights the group is short of; the one at 0 layers is the type's tops &
// bottoms row, a cap of its own that the store caps judge a cart at 0 against.
// Nothing sources on any of them.
//
// ONE WRITE ENDPOINT PER THING AN OPERATOR EDITS, rather than one save-everything
// call. The section has independent halves — the settings, the running cover, one
// level line, one level's overflow, the supported processes, the sources — and a
// single endpoint that took all of them would have to decide what an omitted
// field means. Omitted-means-clear silently deletes a level when the form fails
// to populate; omitted-means-keep makes clearing impossible. Per-thing endpoints
// have no such question to answer.
//
// Each carries its partial to NodeService.SaveKanban, the one write, with the
// session's user as the actor of the audit rows it appends.

// kanbanActor names the audit rows of this request: the session's user, or the
// generic UI actor when there is none.
func (h *Handlers) kanbanActor(r *http.Request) string {
	if u := h.getUsername(r); u != "" {
		return u
	}
	return protocol.AuditActorUI
}

// kanbanSettings is the settings cluster, as one save: they are one
// group of controls on one screen. The audit trail reads one row per setting that
// changed.
type kanbanSettings struct {
	GroupNodeID         int64  `json:"group_node_id"`
	StrictSourcing      bool   `json:"strict_sourcing"`
	MaintenanceStation  string `json:"maintenance_station"`
	OverflowDestination string `json:"overflow_destination"`
	// Force carries the operator's answer to the holds-bins guard. It overrides
	// the FLOOR check only — the save-time rules have no force, because "this
	// group is already a loader's staging group" does not become true because
	// somebody clicked again.
	Force bool `json:"force"`
}

// apiKanban returns one group's whole kanban configuration.
//
// GET /api/nodes/kanban?id=<group node id>
func (h *Handlers) apiKanban(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id == 0 {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	cfg, err := h.engine.NodeService().GetKanban(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, cfg)
}

// apiKanbanProcessOptions returns the picker's contents: each process
// with the Core nodes its claims resolve to.
//
// GET /api/nodes/process-options
func (h *Handlers) apiKanbanProcessOptions(w http.ResponseWriter, r *http.Request) {
	opts, err := h.engine.NodeService().ListProcessNodeOptions()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{"processes": opts})
}

// apiKanbanCheckTypes runs the holds-bins guard for a pending Allowed
// Bins change WITHOUT writing anything.
//
// POST /api/nodes/kanban/check-types
//
// It exists because that control does not save through JSON — it rides the node
// form's ordinary POST, which navigates. A 409 there would replace the page with
// an error document instead of asking a question. So the browser asks first,
// through this, and carries the answer into the form as force; the form handler
// still runs the same guard, so a caller that skips the question is still
// refused.
func (h *Handlers) apiKanbanCheckTypes(w http.ResponseWriter, r *http.Request) {
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
	g, err := h.engine.NodeService().CheckKanbanTypesChange(req.NodeID, req.BinTypeIDs, false)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, map[string]any{"blocked": g.Blocked})
}

// apiKanbanSettingsSet writes the settings: strict, station and the
// default overflow.
//
// POST /api/nodes/kanban/settings
func (h *Handlers) apiKanbanSettingsSet(w http.ResponseWriter, r *http.Request) {
	var req kanbanSettings
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.GroupNodeID == 0 {
		h.jsonError(w, "group_node_id is required", http.StatusBadRequest)
		return
	}
	set := service.KanbanSettings{
		StrictSourcing:      req.StrictSourcing,
		MaintenanceStation:  req.MaintenanceStation,
		OverflowDestination: req.OverflowDestination,
	}
	// The rules first, so a refusal is named ahead of the question the guard asks.
	chk, err := h.engine.NodeService().CheckKanbanSettings(req.GroupNodeID, set)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.kanbanRefused(w, chk) {
		return
	}
	// The floor guard, second: turning the reserve on changes what the carriers
	// standing in the group MEAN, and force is the operator saying they already
	// know.
	g, err := h.engine.NodeService().CheckKanbanSettingsChange(req.GroupNodeID, set, req.Force)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.kanbanHeld(w, g) {
		return
	}
	chk, err = h.engine.NodeService().SaveKanban(service.KanbanSave{
		GroupNodeID: req.GroupNodeID,
		Actor:       h.kanbanActor(r),
		Strict:      &req.StrictSourcing,
		Station:     &req.MaintenanceStation,
		Overflow:    &req.OverflowDestination,
	})
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.kanbanRefused(w, chk) {
		return
	}
	h.kanbanOK(w, chk, g)
}

// apiKanbanCoverSet sets the running cover: the extra the kanban wants
// of a type per supported process running it.
//
// POST /api/nodes/kanban/cover
func (h *Handlers) apiKanbanCoverSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupNodeID  int64 `json:"group_node_id"`
		RunningCover int   `json:"running_cover"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.GroupNodeID == 0 {
		h.jsonError(w, "group_node_id is required", http.StatusBadRequest)
		return
	}
	h.saveKanbanAnswer(w, service.KanbanSave{GroupNodeID: req.GroupNodeID, Actor: h.kanbanActor(r), Cover: &req.RunningCover})
}

// apiKanbanSourcesSet replaces the sources: the groups the keeper may
// pull from, the list order being the rank.
//
// POST /api/nodes/kanban/sources
func (h *Handlers) apiKanbanSourcesSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupNodeID   int64   `json:"group_node_id"`
		SourceNodeIDs []int64 `json:"source_node_ids"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.GroupNodeID == 0 {
		h.jsonError(w, "group_node_id is required", http.StatusBadRequest)
		return
	}
	if req.SourceNodeIDs == nil {
		req.SourceNodeIDs = []int64{}
	}
	h.saveKanbanAnswer(w, service.KanbanSave{GroupNodeID: req.GroupNodeID, Actor: h.kanbanActor(r), Sources: &req.SourceNodeIDs})
}

// apiKanbanLevelOverflowSet sets one level's own overflow; 0 clears it,
// and the type takes the kanban's default.
//
// POST /api/nodes/kanban/level/overflow
func (h *Handlers) apiKanbanLevelOverflowSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupNodeID    int64 `json:"group_node_id"`
		BinTypeID      int64 `json:"bin_type_id"`
		OverflowNodeID int64 `json:"overflow_node_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.GroupNodeID == 0 || req.BinTypeID == 0 {
		h.jsonError(w, "group_node_id and bin_type_id are required", http.StatusBadRequest)
		return
	}
	h.saveKanbanAnswer(w, service.KanbanSave{GroupNodeID: req.GroupNodeID, Actor: h.kanbanActor(r),
		Levels: []service.KanbanLevelSave{{BinTypeID: req.BinTypeID, Overflow: &req.OverflowNodeID}}})
}

// saveKanbanAnswer runs a save with no floor guard and answers it.
func (h *Handlers) saveKanbanAnswer(w http.ResponseWriter, save service.KanbanSave) {
	chk, err := h.engine.NodeService().SaveKanban(save)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.kanbanRefused(w, chk) {
		return
	}
	h.kanbanOK(w, chk, service.KanbanGuard{})
}

// kanbanLayerWant is one wanted height on a level's save: how many carts of
// the level's type, standing this many layers high, the group wants.
type kanbanLayerWant struct {
	Layers int `json:"layers"`
	Want   int `json:"want"`
}

// apiKanbanLevelSet declares how many empties of one type a group holds,
// and, when the request carries layer_wants, replaces that type's wanted
// heights in the same transaction.
//
// POST /api/nodes/kanban/level
//
// AN ABSENT layer_wants LEAVES THE STORED ONES STANDING; a list, empty
// included, replaces them. So a caller that never loaded the heights cannot
// clear them by saving the level. Pin: TestKanbanLevel_LayerWants.
func (h *Handlers) apiKanbanLevelSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GroupNodeID int64              `json:"group_node_id"`
		BinTypeID   int64              `json:"bin_type_id"`
		Want        int                `json:"want"`
		LayerWants  *[]kanbanLayerWant `json:"layer_wants"`
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
	line := service.KanbanLevelSave{BinTypeID: req.BinTypeID, Want: &req.Want}
	if req.LayerWants != nil {
		wants, werr := layerWantRows(*req.LayerWants)
		if werr != nil {
			h.jsonError(w, werr.Error(), http.StatusBadRequest)
			return
		}
		line.LayerWants = &wants
	}
	// No floor guard on a level line: declaring or undeclaring a carrier type
	// changes what Core HOLDS, not what a resident is allowed to be.
	h.saveKanbanAnswer(w, service.KanbanSave{GroupNodeID: req.GroupNodeID, Actor: h.kanbanActor(r),
		Levels: []service.KanbanLevelSave{line}})
}

// layerWantRows reads a level save's wanted heights: each 0 layers (the tops &
// bottoms row) or more, wanted zero or more times, no height twice. A
// malformed list is the request's fault (400), the same as a missing id; what
// the group can hold, and whether the type can be wanted at a height at all,
// are the save's rules (the service's setKanbanLevel).
func layerWantRows(in []kanbanLayerWant) ([]service.KanbanLayerWant, error) {
	out := make([]service.KanbanLayerWant, 0, len(in))
	seen := map[int]bool{}
	for _, lw := range in {
		if lw.Layers < 0 {
			return nil, fmt.Errorf("a wanted height is 0 layers (tops & bottoms) or more (got %d)", lw.Layers)
		}
		if lw.Want < 0 {
			return nil, fmt.Errorf("the want at %d layers cannot be negative (got %d)", lw.Layers, lw.Want)
		}
		if seen[lw.Layers] {
			return nil, fmt.Errorf("%d layers is listed twice", lw.Layers)
		}
		seen[lw.Layers] = true
		out = append(out, service.KanbanLayerWant{Layers: lw.Layers, Want: lw.Want})
	}
	return out, nil
}

// apiKanbanLevelRemove stops declaring a carrier type for a group; its
// layer wants are removed with it.
//
// POST /api/nodes/kanban/level/remove
func (h *Handlers) apiKanbanLevelRemove(w http.ResponseWriter, r *http.Request) {
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
	// No floor guard on a level line: declaring or undeclaring a carrier type
	// changes what Core HOLDS, not what a resident is allowed to be.
	h.saveKanbanAnswer(w, service.KanbanSave{GroupNodeID: req.GroupNodeID, Actor: h.kanbanActor(r),
		RemoveLevels: []int64{req.BinTypeID}})
}

// apiKanbanSupportsSet replaces the set of process nodes a group serves.
//
// POST /api/nodes/kanban/supports
//
// NODE IDS ON THE WIRE, resolved by the browser from the process options it was
// handed. The editor speaks process; what is stored and later enforced is nodes,
// and the resolution happens once at save time because a claim lives on the Edge
// and Core cannot read one when it matters.
func (h *Handlers) apiKanbanSupportsSet(w http.ResponseWriter, r *http.Request) {
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
	g, err := h.engine.NodeService().CheckKanbanSupportsChange(req.GroupNodeID, req.ProcessNodeIDs, req.Force)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.kanbanHeld(w, g) {
		return
	}
	// An empty set is a legitimate save: it is what "this group serves nobody
	// yet" looks like, and refusing it would leave no way to undo a mistake.
	supports := req.ProcessNodeIDs
	if supports == nil {
		supports = []int64{}
	}
	chk, err := h.engine.NodeService().SaveKanban(service.KanbanSave{GroupNodeID: req.GroupNodeID, Actor: h.kanbanActor(r),
		Supports: &supports})
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.kanbanRefused(w, chk) {
		return
	}
	h.kanbanOK(w, chk, g)
}

// kanbanRefused writes the refusal response and reports whether the
// caller should CONTINUE (true = nothing was refused).
//
// 409 rather than 400: the request is well-formed, and what stops it is the
// state of the plant's configuration around it — a distinction worth keeping,
// because a client cannot fix a 400 by changing anything but the request.
func (h *Handlers) kanbanRefused(w http.ResponseWriter, chk service.KanbanCheck) bool {
	if err := chk.Err(); err != nil {
		h.jsonError(w, err.Error(), http.StatusConflict)
		return false
	}
	return true
}

// kanbanHeld writes the holds-bins response and reports whether the
// caller should CONTINUE (true = nothing is standing in the way).
//
// 409 with needs_force, which the browser turns into a confirm dialog and
// re-sends with force. The distinction from a plain refusal matters to the
// client and to nobody else: a rule refusal is final, this one has an answer.
func (h *Handlers) kanbanHeld(w http.ResponseWriter, g service.KanbanGuard) bool {
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

// kanbanOK answers a successful save, carrying any warnings with it.
//
// WARNINGS RIDE WITH SUCCESS. Every one of them describes a state a plant can
// legitimately be in mid-configuration, so none may block the save — but an
// operator who is one position short of room for a returning carrier should be
// told at the moment they made it so, not the first time something parks.
//
// The drain list rides here too, and it is the reason it is not a warning: it is
// not about the configuration being questionable, it is a list of orders already
// on their way that the reserve does not reach back and stop.
func (h *Handlers) kanbanOK(w http.ResponseWriter, chk service.KanbanCheck, g service.KanbanGuard) {
	h.jsonOK(w, map[string]any{
		"status":   "ok",
		"warnings": chk.Warnings,
		"drain":    g.Drain,
	})
}
