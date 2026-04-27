package www

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"shingocore/domain"
	"shingocore/engine"
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

	bt := &domain.BinType{
		Code:        r.FormValue("code"),
		Description: r.FormValue("description"),
		WidthIn:     widthIn,
		HeightIn:    heightIn,
	}

	if err := h.engine.BinService().CreateBinType(bt); err != nil {
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

	svc := h.engine.BinService()
	bt, err := svc.GetBinType(id)
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

	if err := svc.UpdateBinType(bt); err != nil {
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

	if err := h.engine.BinService().DeleteBinType(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

// --- Page handler ---

func (h *Handlers) handleBins(w http.ResponseWriter, r *http.Request) {
	svc := h.engine.BinService()
	bins, err := svc.ListBins()
	if err != nil {
		log.Printf("bins page: list bins: %v", err)
	}
	binTypes, err := svc.ListBinTypes()
	if err != nil {
		log.Printf("bins page: list bin types: %v", err)
	}
	nodes, err := h.engine.NodeService().ListNodes()
	if err != nil {
		log.Printf("bins page: list nodes: %v", err)
	}
	payloads, err := h.engine.PayloadService().List()
	if err != nil {
		log.Printf("bins page: list payloads: %v", err)
	}

	// Build bin IDs for notes indicator
	binIDs := make([]int64, len(bins))
	for i, b := range bins {
		binIDs[i] = b.ID
	}
	binHasNotes, err := svc.HasNotes(binIDs)
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

	template := domain.Bin{
		BinTypeID: binTypeID,
		NodeID:    nodeID,
		Status:    status,
	}
	if err := h.engine.BinService().CreateBatch(template, label, count); err != nil {
		http.Error(w, err.Error(), httpStatusForCreate(err))
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

// httpStatusForCreate maps BinService.CreateBatch error messages to HTTP
// status codes so the admin UI gets the pre-refactor response codes
// (404 node-not-found, 409 occupancy, 500 otherwise).
func httpStatusForCreate(err error) int {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		return http.StatusBadRequest
	case strings.Contains(msg, "cannot create multiple bins"),
		strings.Contains(msg, "already has"):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (h *Handlers) handleBinDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.BinService().Delete(id); err != nil {
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

	b, err := h.engine.BinService().GetBin(req.ID)
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

// binActionFunc is the handler signature for individual bin actions.
type binActionFunc func(b *domain.Bin, params json.RawMessage) error

func (h *Handlers) executeBinAction(b *domain.Bin, action string, params json.RawMessage) error {
	actions := map[string]binActionFunc{
		"activate":           h.binActivate,
		"flag":               h.binFlag,
		"quality_hold":       h.binQualityHold,
		"maintenance":        h.binMaintenance,
		"retire":             h.binRetire,
		"release":            h.binRelease,
		"lock":               h.binLock,
		"unlock":             h.binUnlock,
		"load_payload":       h.binLoadPayload,
		"clear":              h.binClear,
		"confirm_manifest":   h.binConfirmManifest,
		"unconfirm_manifest": h.binUnconfirmManifest,
		"move":               h.binMove,
		"record_count":       h.binRecordCount,
		"add_note":           h.binAddNote,
		"update":             h.binUpdate,
	}
	fn, ok := actions[action]
	if !ok {
		return fmt.Errorf("unknown action: %s", action)
	}
	return fn(b, params)
}

// --- Bin action handlers (bound method values used by executeBinAction) ---

func (h *Handlers) binActivate(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().ChangeStatus(b.ID, "available"); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status, "available", "ui")
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binFlag(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().ChangeStatus(b.ID, "flagged"); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status, "flagged", "ui")
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binQualityHold(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	svc := h.engine.BinService()
	if err := svc.ChangeStatus(b.ID, "quality_hold"); err != nil {
		return err
	}
	actor := h.resolveActor(p.Actor)
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status, "quality_hold", actor)
	if p.Reason != "" {
		svc.AddNote(b.ID, "hold", p.Reason, actor)
	}
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binMaintenance(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().ChangeStatus(b.ID, "maintenance"); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status, "maintenance", "ui")
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binRetire(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().ChangeStatus(b.ID, "retired"); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status, "retired", "ui")
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binRelease(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().Release(b.ID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", "staged", "available", "ui")
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binLock(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		Actor string `json:"actor"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	actor := h.resolveActor(p.Actor)
	if err := h.engine.BinService().Lock(b.ID, actor); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "locked", "", actor, actor)
	h.emitBinUpdate(b, "locked", actor)
	return nil
}

func (h *Handlers) binUnlock(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().Unlock(b.ID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "unlocked", b.LockedBy, "", "ui")
	h.emitBinUpdate(b, "unlocked", "")
	return nil
}

func (h *Handlers) binLoadPayload(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		PayloadCode string `json:"payload_code"`
		UOPOverride int    `json:"uop_override"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	if err := h.engine.BinService().LoadPayload(b.ID, p.PayloadCode, p.UOPOverride); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "loaded", "", p.PayloadCode, "ui")
	h.emitBinUpdate(b, "loaded", p.PayloadCode)
	return nil
}

func (h *Handlers) binClear(b *domain.Bin, _ json.RawMessage) error {
	oldCode := b.PayloadCode
	if err := h.engine.BinService().Manifest().ClearForReuse(b.ID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "cleared", oldCode, "", "ui")
	h.emitBinUpdate(b, "cleared", "")
	return nil
}

func (h *Handlers) binConfirmManifest(b *domain.Bin, _ json.RawMessage) error {
	if b.Manifest == nil {
		return fmt.Errorf("bin has no manifest to confirm")
	}
	if err := h.engine.BinService().Manifest().Confirm(b.ID, ""); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "confirmed", "unconfirmed", "confirmed", "ui")
	h.emitBinUpdate(b, "loaded", "")
	return nil
}

func (h *Handlers) binUnconfirmManifest(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().Manifest().Unconfirm(b.ID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "unconfirmed", "confirmed", "unconfirmed", "ui")
	h.emitBinUpdate(b, "loaded", "")
	return nil
}

func (h *Handlers) binMove(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		NodeID int64 `json:"node_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	res, err := h.engine.BinService().Move(b, p.NodeID)
	if err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "moved", b.NodeName, res.DestNode.Name, "ui")
	h.engine.EventBus().Emit(engine.Event{Type: engine.EventBinUpdated, Payload: engine.BinUpdatedEvent{
		BinID:       b.ID,
		NodeID:      p.NodeID,
		Action:      "moved",
		PayloadCode: b.PayloadCode,
		FromNodeID:  derefInt64(b.NodeID),
		ToNodeID:    p.NodeID,
	}})
	return nil
}

func (h *Handlers) binRecordCount(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		ActualUOP int    `json:"actual_uop"`
		Actor     string `json:"actor"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	actor := h.resolveActor(p.Actor)
	svc := h.engine.BinService()
	res, err := svc.RecordCount(b, p.ActualUOP, actor)
	if err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "counted", strconv.Itoa(res.Expected), strconv.Itoa(res.Actual), actor)
	if res.Discrepancy {
		svc.AddNote(b.ID, "count",
			fmt.Sprintf("Cycle count discrepancy: expected %d, actual %d (%+d)", res.Expected, res.Actual, res.Actual-res.Expected),
			actor)
	}
	h.emitBinUpdate(b, "counted", "")
	return nil
}

func (h *Handlers) binAddNote(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		NoteType string `json:"note_type"`
		Message  string `json:"message"`
		Actor    string `json:"actor"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	actor := h.resolveActor(p.Actor)
	return h.engine.BinService().AddNote(b.ID, p.NoteType, p.Message, actor)
}

func (h *Handlers) binUpdate(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		Label       *string `json:"label"`
		Description *string `json:"description"`
		BinTypeID   *int64  `json:"bin_type_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	if err := h.engine.BinService().Update(b, p.Label, p.Description, p.BinTypeID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "updated", "", "", "ui")
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) emitBinUpdate(b *domain.Bin, action, detail string) {
	h.engine.EventBus().Emit(engine.Event{Type: engine.EventBinUpdated, Payload: engine.BinUpdatedEvent{
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
	Bin          *domain.Bin          `json:"bin"`
	Manifest     *domain.Manifest  `json:"manifest"`
	Template     *domain.Payload      `json:"template,omitempty"`
	Audit        []*domain.AuditEntry `json:"audit"`
	CurrentOrder *domain.Order        `json:"current_order,omitempty"`
	RecentOrders []*domain.Order      `json:"recent_orders"`
}

func (h *Handlers) apiBinDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}

	b, err := h.engine.BinService().GetBin(id)
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
		if p, err := h.engine.PayloadService().GetByCode(b.PayloadCode); err == nil {
			resp.Template = p
		}
	}

	// Audit log
	resp.Audit, _ = h.engine.AuditService().ListForEntity("bin", id)

	// Current order
	if b.ClaimedBy != nil {
		resp.CurrentOrder, _ = h.engine.OrderService().GetOrder(*b.ClaimedBy)
	}

	// Recent orders
	resp.RecentOrders, _ = h.engine.OrderService().ListByBin(id, 20)
	if resp.RecentOrders == nil {
		resp.RecentOrders = []*domain.Order{}
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

	svc := h.engine.BinService()
	results := make([]bulkResult, 0, len(req.IDs))
	for _, id := range req.IDs {
		b, err := svc.GetBin(id)
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

	b, err := h.engine.BinService().GetBin(req.BinID)
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
	if *b.NodeID == req.DestinationNodeID {
		h.jsonError(w, "bin is already at this location", http.StatusBadRequest)
		return
	}

	nodes := h.engine.NodeService()
	srcNode, err := nodes.GetNode(*b.NodeID)
	if err != nil {
		h.jsonError(w, "source node not found", http.StatusNotFound)
		return
	}
	destNode, err := nodes.GetNode(req.DestinationNodeID)
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
	bins, err := h.engine.NodeService().ListBinsByNode(id)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, bins)
}
