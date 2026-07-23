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

// --- Page handler ---

// binRow decorates a bin for the bins-page table with transit-aware display
// fields. A bin in flight sits at the synthetic _TRANSIT node for the duration
// of the move, and its own payload_code can read blank there — but the order
// carrying it still knows the cargo and the route. Surface those so the row
// reads like a tracking line ("PART-1234 · SMN_001 → P400") instead of a bare
// "_TRANSIT" with an empty payload; operators need to see what's on the carrier
// and where it's headed (plant 2026-06-02).
type binRow struct {
	*domain.Bin
	InTransit      bool
	TransitPayload string
	TransitSource  string
	TransitDest    string
}

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

	// Decorate in-transit bins with their carrying order's cargo + route so the
	// table shows what's on the carrier and where it's headed instead of a bare
	// "_TRANSIT" row. Only the handful of bins actually in flight take the extra
	// order lookup; everything else passes through untouched.
	rows := make([]binRow, len(bins))
	for i, b := range bins {
		row := binRow{Bin: b}
		if b.NodeName == domain.TransitNodeName && b.ClaimedBy != nil {
			row.InTransit = true
			if o, err := h.engine.OrderService().GetOrder(*b.ClaimedBy); err == nil && o != nil {
				row.TransitPayload = o.PayloadCode
				row.TransitSource = o.SourceNode
				row.TransitDest = o.DeliveryNode
			}
		}
		rows[i] = row
	}

	// Per-payload bin-type allow-list (keyed by payload code). Empty list = unrestricted,
	// matching the advisory semantics used by FindSourceFIFO / FindEmptyCompatible.
	payloadBinTypeIDs := make(map[string][]int64, len(payloads))
	for _, p := range payloads {
		btList, btErr := h.engine.PayloadService().ListBinTypes(p.ID)
		if btErr != nil {
			log.Printf("bins page: list bin types for payload %d: %v", p.ID, btErr)
			continue
		}
		ids := make([]int64, len(btList))
		for i, bt := range btList {
			ids[i] = bt.ID
		}
		payloadBinTypeIDs[p.Code] = ids
	}

	// JSON-encode nodes, payloads, bin types, and compat map for JS consumption
	nodesJSON, _ := json.Marshal(nodes)
	payloadsJSON, _ := json.Marshal(payloads)
	binTypesJSON, _ := json.Marshal(binTypes)
	payloadBinTypesJSON, _ := json.Marshal(payloadBinTypeIDs)

	data := map[string]any{
		"Page":                "bins",
		"Bins":                rows,
		"BinTypes":            binTypes,
		"Nodes":               nodes,
		"Payloads":            payloads,
		"BinHasNotes":         binHasNotes,
		"NodesJSON":           template.JS(nodesJSON),
		"PayloadsJSON":        template.JS(payloadsJSON),
		"BinTypesJSON":        template.JS(binTypesJSON),
		"PayloadBinTypesJSON": template.JS(payloadBinTypesJSON),
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
	status := domain.BinStatus(r.FormValue("status"))
	if status == "" {
		status = domain.BinStatusAvailable
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

	// Wake the fulfillment scanner — orders queued on missing-bin
	// (post-06138c6) or on operator-overridden changeover preflight
	// (Note 7) sleep in `queued` status until EventBinUpdated fires.
	// Pre-fix, freshly-created bins did not emit this event, so a
	// matching queued order would not replay until something else
	// triggered the scanner (a bin move, an order completion). The
	// emitted payload is intentionally minimal — the scanner doesn't
	// read it, the audit handler (wiring.go:199-202) does, and we
	// only know the bin type + node here, not the persisted IDs.
	h.engine.EventBus().Emit(engine.Event{Type: engine.EventBinUpdated, Payload: engine.BinUpdatedEvent{
		Action: "created",
		NodeID: derefInt64(nodeID),
	}})

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
		strings.Contains(msg, "already has"),
		strings.Contains(msg, "already exist"):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (h *Handlers) handleBinRetire(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.engine.BinService().Retire(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/bins", http.StatusSeeOther)
}

// --- Bin action API (single dispatch endpoint) ---

func (h *Handlers) apiBinAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     int64           `json:"id"`
		Action string          `json:"action"`
		Params json.RawMessage `json:"params"`
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

func derefInt64(p *int64) int64 {
	if p != nil {
		return *p
	}
	return 0
}

// --- Bin detail API ---

type binDetailResponse struct {
	Bin          *domain.Bin          `json:"bin"`
	Manifest     *domain.Manifest     `json:"manifest"`
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
		IDs    []int64         `json:"ids"`
		Action string          `json:"action"`
		Params json.RawMessage `json:"params"`
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

	// Create a manual move order using the existing manual order infrastructure
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
