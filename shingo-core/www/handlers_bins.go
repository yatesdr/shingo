package www

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"shingocore/engine"
	"shingocore/store"
)

// --- Bin Type form handlers (unchanged) ---

func (h *Handlers) handleBinTypeCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	widthIn, err := strconv.ParseFloat(r.FormValue("width_in"), 64)
	if err != nil && r.FormValue("width_in") != "" {
		http.Error(w, "invalid width_in", http.StatusBadRequest)
		return
	}
	heightIn, err := strconv.ParseFloat(r.FormValue("height_in"), 64)
	if err != nil && r.FormValue("height_in") != "" {
		http.Error(w, "invalid height_in", http.StatusBadRequest)
		return
	}

	bt := &store.BinType{
		Code:        r.FormValue("code"),
		Description: r.FormValue("description"),
		WidthIn:     widthIn,
		HeightIn:    heightIn,
	}

	if err := h.engine.DB().CreateBinType(bt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

func (h *Handlers) handleBinTypeUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	bt, err := h.engine.DB().GetBinType(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	bt.Code = r.FormValue("code")
	bt.Description = r.FormValue("description")
	if w, err := strconv.ParseFloat(r.FormValue("width_in"), 64); err == nil || r.FormValue("width_in") == "" {
		bt.WidthIn = w
	}
	if h, err := strconv.ParseFloat(r.FormValue("height_in"), 64); err == nil || r.FormValue("height_in") == "" {
		bt.HeightIn = h
	}

	if err := h.engine.DB().UpdateBinType(bt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

func (h *Handlers) handleBinTypeDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeleteBinType(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

// --- Page handler ---

func (h *Handlers) handleBins(w http.ResponseWriter, r *http.Request) {
	bins, err := h.engine.DB().ListBins()
	if err != nil {
		log.Printf("bins page: list bins: %v", err)
	}
	binTypes, err := h.engine.DB().ListBinTypes()
	if err != nil {
		log.Printf("bins page: list bin types: %v", err)
	}
	nodes, err := h.engine.DB().ListNodes()
	if err != nil {
		log.Printf("bins page: list nodes: %v", err)
	}
	payloads, err := h.engine.DB().ListPayloads()
	if err != nil {
		log.Printf("bins page: list payloads: %v", err)
	}

	// Build bin IDs for notes indicator
	binIDs := make([]int64, len(bins))
	for i, b := range bins {
		binIDs[i] = b.ID
	}
	binHasNotes, err := h.engine.DB().BinHasNotes(binIDs)
	if err != nil {
		log.Printf("bins page: check bin notes: %v", err)
	}

	// JSON-encode nodes and payloads for JS consumption
	nodesJSON, _ := json.Marshal(nodes)
	payloadsJSON, _ := json.Marshal(payloads)

	data := map[string]any{
		"Page":        "bins",
		"Bins":        bins,
		"BinTypes":    binTypes,
		"Nodes":       nodes,
		"Payloads":    payloads,
		"BinHasNotes": binHasNotes,
		"NodesJSON":   template.JS(nodesJSON),
		"PayloadsJSON": template.JS(payloadsJSON),
	}
	h.render(w, r, "bins.html", data)
}

// --- Bin create/delete form handlers ---

func (h *Handlers) handleBinCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	binTypeID, err := strconv.ParseInt(r.FormValue("bin_type_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid bin type", http.StatusBadRequest)
		return
	}

	count, err := strconv.Atoi(r.FormValue("quantity"))
	if err != nil && r.FormValue("quantity") != "" {
		http.Error(w, "invalid quantity", http.StatusBadRequest)
		return
	}
	if count <= 0 {
		count = 1
	}

	label := r.FormValue("label_prefix")
	status := r.FormValue("status")
	if status == "" {
		status = "available"
	}

	var nodeID *int64
	if nStr := r.FormValue("node_id"); nStr != "" {
		if nid, err := strconv.ParseInt(nStr, 10, 64); err == nil {
			nodeID = &nid
		}
	}

	for i := 0; i < count; i++ {
		binLabel := label + fmt.Sprintf("%04d", i+1)
		b := &store.Bin{
			BinTypeID: binTypeID,
			Label:     binLabel,
			NodeID:    nodeID,
			Status:    status,
		}
		if err := h.engine.DB().CreateBin(b); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

func (h *Handlers) handleBinDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.DB().DeleteBin(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

// --- Bin action API (single dispatch endpoint) ---

func (h *Handlers) apiBinAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     int64            `json:"id"`
		Action string           `json:"action"`
		Params json.RawMessage  `json:"params"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	b, err := h.engine.DB().GetBin(req.ID)
	if err != nil {
		h.jsonError(w, "bin not found", http.StatusNotFound)
		return
	}

	if err := h.executeBinAction(b, req.Action, req.Params); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) executeBinAction(b *store.Bin, action string, params json.RawMessage) error {
	db := h.engine.DB()
	oldStatus := b.Status

	switch action {
	case "activate":
		if err := db.UpdateBinStatus(b.ID, "available"); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "status", oldStatus, "available", "ui")
		h.emitBinUpdate(b, "status_changed", "")

	case "flag":
		if err := db.UpdateBinStatus(b.ID, "flagged"); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "status", oldStatus, "flagged", "ui")
		h.emitBinUpdate(b, "status_changed", "")

	case "quality_hold":
		var p struct {
			Reason string `json:"reason"`
			Actor  string `json:"actor"`
		}
		if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
			return fmt.Errorf("invalid params: %w", err)
		}
		if err := db.UpdateBinStatus(b.ID, "quality_hold"); err != nil {
			return err
		}
		actor := h.resolveActor(p.Actor)
		db.AppendAudit("bin", b.ID, "status", oldStatus, "quality_hold", actor)
		if p.Reason != "" {
			db.AddBinNote(b.ID, "hold", p.Reason, actor)
		}
		h.emitBinUpdate(b, "status_changed", "")

	case "maintenance":
		if err := db.UpdateBinStatus(b.ID, "maintenance"); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "status", oldStatus, "maintenance", "ui")
		h.emitBinUpdate(b, "status_changed", "")

	case "retire":
		if err := db.UpdateBinStatus(b.ID, "retired"); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "status", oldStatus, "retired", "ui")
		h.emitBinUpdate(b, "status_changed", "")

	case "release":
		if err := db.ReleaseStagedBin(b.ID); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "status", "staged", "available", "ui")
		h.emitBinUpdate(b, "status_changed", "")

	case "lock":
		var p struct {
			Actor string `json:"actor"`
		}
		if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
			return fmt.Errorf("invalid params: %w", err)
		}
		actor := h.resolveActor(p.Actor)
		if actor == "" {
			return fmt.Errorf("actor is required for lock")
		}
		if err := db.LockBin(b.ID, actor); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "locked", "", actor, actor)
		h.emitBinUpdate(b, "locked", actor)

	case "unlock":
		if err := db.UnlockBin(b.ID); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "unlocked", b.LockedBy, "", "ui")
		h.emitBinUpdate(b, "unlocked", "")

	case "load_payload":
		var p struct {
			PayloadCode string `json:"payload_code"`
			UOPOverride int    `json:"uop_override"`
		}
		if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
			return fmt.Errorf("invalid params: %w", err)
		}
		if p.PayloadCode == "" {
			return fmt.Errorf("payload_code is required")
		}
		if _, err := db.GetPayloadByCode(p.PayloadCode); err != nil {
			return fmt.Errorf("payload template %q not found", p.PayloadCode)
		}
		if err := db.SetBinManifestFromTemplate(b.ID, p.PayloadCode, p.UOPOverride); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "loaded", "", p.PayloadCode, "ui")
		h.emitBinUpdate(b, "loaded", p.PayloadCode)

	case "clear":
		oldCode := b.PayloadCode
		if err := db.ClearBinManifest(b.ID); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "cleared", oldCode, "", "ui")
		h.emitBinUpdate(b, "cleared", "")

	case "confirm_manifest":
		if b.Manifest == nil {
			return fmt.Errorf("bin has no manifest to confirm")
		}
		if err := db.ConfirmBinManifest(b.ID, ""); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "confirmed", "unconfirmed", "confirmed", "ui")
		h.emitBinUpdate(b, "loaded", "")

	case "unconfirm_manifest":
		if err := db.UnconfirmBinManifest(b.ID); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "unconfirmed", "confirmed", "unconfirmed", "ui")
		h.emitBinUpdate(b, "loaded", "")

	case "move":
		var p struct {
			NodeID int64 `json:"node_id"`
		}
		if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
			return fmt.Errorf("invalid params: %w", err)
		}
		if p.NodeID == 0 {
			return fmt.Errorf("node_id is required")
		}
		destNode, err := db.GetNode(p.NodeID)
		if err != nil {
			return fmt.Errorf("node not found")
		}
		if err := db.MoveBin(b.ID, p.NodeID); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "moved", b.NodeName, destNode.Name, "ui")
		h.engine.Events.Emit(engine.Event{Type: engine.EventBinUpdated, Payload: engine.BinUpdatedEvent{
			BinID:       b.ID,
			NodeID:      p.NodeID,
			Action:      "moved",
			PayloadCode: b.PayloadCode,
			FromNodeID:  derefInt64(b.NodeID),
			ToNodeID:    p.NodeID,
		}})

	case "record_count":
		var p struct {
			ActualUOP int    `json:"actual_uop"`
			Actor     string `json:"actor"`
		}
		if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
			return fmt.Errorf("invalid params: %w", err)
		}
		actor := h.resolveActor(p.Actor)
		expected := b.UOPRemaining
		if err := db.RecordBinCount(b.ID, p.ActualUOP, actor); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "counted", strconv.Itoa(expected), strconv.Itoa(p.ActualUOP), actor)
		if expected != p.ActualUOP {
			db.AddBinNote(b.ID, "count", fmt.Sprintf("Cycle count discrepancy: expected %d, actual %d (%+d)", expected, p.ActualUOP, p.ActualUOP-expected), actor)
		}
		h.emitBinUpdate(b, "counted", "")

	case "add_note":
		var p struct {
			NoteType string `json:"note_type"`
			Message  string `json:"message"`
			Actor    string `json:"actor"`
		}
		if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
			return fmt.Errorf("invalid params: %w", err)
		}
		if p.Message == "" {
			return fmt.Errorf("message is required")
		}
		actor := h.resolveActor(p.Actor)
		noteType := p.NoteType
		if noteType == "" {
			noteType = "general"
		}
		return db.AddBinNote(b.ID, noteType, p.Message, actor)

	case "update":
		var p struct {
			Label       *string `json:"label"`
			Description *string `json:"description"`
			BinTypeID   *int64  `json:"bin_type_id"`
		}
		if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
			return fmt.Errorf("invalid params: %w", err)
		}
		if p.Label != nil {
			b.Label = *p.Label
		}
		if p.Description != nil {
			b.Description = *p.Description
		}
		if p.BinTypeID != nil {
			b.BinTypeID = *p.BinTypeID
		}
		if err := db.UpdateBin(b); err != nil {
			return err
		}
		db.AppendAudit("bin", b.ID, "updated", "", "", "ui")
		h.emitBinUpdate(b, "status_changed", "")

	default:
		return fmt.Errorf("unknown action: %s", action)
	}

	return nil
}

func (h *Handlers) emitBinUpdate(b *store.Bin, action, detail string) {
	h.engine.Events.Emit(engine.Event{Type: engine.EventBinUpdated, Payload: engine.BinUpdatedEvent{
		BinID:       b.ID,
		NodeID:      derefInt64(b.NodeID),
		Action:      action,
		PayloadCode: b.PayloadCode,
	}})
}

func (h *Handlers) resolveActor(actor string) string {
	if actor != "" {
		return actor
	}
	return "ui"
}

func derefInt64(p *int64) int64 {
	if p != nil {
		return *p
	}
	return 0
}

// --- Bin detail API ---

type binDetailResponse struct {
	Bin          *store.Bin          `json:"bin"`
	Manifest     *store.BinManifest  `json:"manifest"`
	Template     *store.Payload      `json:"template,omitempty"`
	Audit        []*store.AuditEntry `json:"audit"`
	CurrentOrder *store.Order        `json:"current_order,omitempty"`
	RecentOrders []*store.Order      `json:"recent_orders"`
}

func (h *Handlers) apiBinDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}

	b, err := h.engine.DB().GetBin(id)
	if err != nil {
		h.jsonError(w, "bin not found", http.StatusNotFound)
		return
	}

	resp := binDetailResponse{Bin: b}

	// Parse manifest
	if m, err := b.ParseManifest(); err == nil {
		resp.Manifest = m
	}

	// Payload template
	if b.PayloadCode != "" {
		if p, err := h.engine.DB().GetPayloadByCode(b.PayloadCode); err == nil {
			resp.Template = p
		}
	}

	// Audit log
	resp.Audit, _ = h.engine.DB().ListEntityAudit("bin", id)

	// Current order
	if b.ClaimedBy != nil {
		resp.CurrentOrder, _ = h.engine.DB().GetOrder(*b.ClaimedBy)
	}

	// Recent orders
	resp.RecentOrders, _ = h.engine.DB().ListOrdersByBin(id, 20)
	if resp.RecentOrders == nil {
		resp.RecentOrders = []*store.Order{}
	}

	h.jsonOK(w, resp)
}

// --- Bulk bin action API ---

func (h *Handlers) apiBulkBinAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []int64          `json:"ids"`
		Action string           `json:"action"`
		Params json.RawMessage  `json:"params"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if len(req.IDs) == 0 || len(req.IDs) > 100 {
		h.jsonError(w, "ids must contain 1-100 entries", http.StatusBadRequest)
		return
	}

	type bulkResult struct {
		ID    int64  `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}

	results := make([]bulkResult, 0, len(req.IDs))
	for _, id := range req.IDs {
		b, err := h.engine.DB().GetBin(id)
		if err != nil {
			results = append(results, bulkResult{ID: id, Error: "not found"})
			continue
		}
		if b.Locked && req.Action != "unlock" {
			results = append(results, bulkResult{ID: id, Error: fmt.Sprintf("locked by %s", b.LockedBy)})
			continue
		}
		if err := h.executeBinAction(b, req.Action, req.Params); err != nil {
			results = append(results, bulkResult{ID: id, Error: err.Error()})
			continue
		}
		results = append(results, bulkResult{ID: id, OK: true})
	}

	h.jsonOK(w, map[string]any{"results": results})
}

// --- Request transport API ---

func (h *Handlers) apiRequestBinTransport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BinID             int64 `json:"bin_id"`
		DestinationNodeID int64 `json:"destination_node_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	b, err := h.engine.DB().GetBin(req.BinID)
	if err != nil {
		h.jsonError(w, "bin not found", http.StatusNotFound)
		return
	}
	if b.ClaimedBy != nil {
		h.jsonError(w, fmt.Sprintf("bin is claimed by order %d", *b.ClaimedBy), http.StatusConflict)
		return
	}
	if b.NodeID == nil {
		h.jsonError(w, "bin has no current location", http.StatusBadRequest)
		return
	}

	srcNode, err := h.engine.DB().GetNode(*b.NodeID)
	if err != nil {
		h.jsonError(w, "source node not found", http.StatusNotFound)
		return
	}
	destNode, err := h.engine.DB().GetNode(req.DestinationNodeID)
	if err != nil {
		h.jsonError(w, "destination node not found", http.StatusNotFound)
		return
	}

	// Create a spot move order using the existing spot order infrastructure
	h.jsonOK(w, map[string]any{
		"message": fmt.Sprintf("Transport requested: %s → %s", srcNode.Name, destNode.Name),
		"bin_id":  b.ID,
		"from":    srcNode.Name,
		"to":      destNode.Name,
	})
}

// --- Bin query APIs ---

func (h *Handlers) apiBinsByNode(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	bins, err := h.engine.DB().ListBinsByNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, bins)
}
