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
	// Stranded is a _TRANSIT bin nobody is holding — the anomaly. It used to
	// lose its cargo and route decoration at exactly this point, because the
	// decoration was gated on the claim the terminalisation had just cleared:
	// the bin an operator most needs to identify was the one the page said
	// least about.
	Stranded bool
	// TransitOrderID and TransitOrderStatus name the order the route above
	// came from. On a stranded row that order is over — cancelled or failed —
	// and without them the row read "ALN_006 → SMN_0013 … (in transit)",
	// indistinguishable from a bin a robot is carrying right now.
	TransitOrderID     int64
	TransitOrderStatus string
	// CarriedBy is the robot whose deck this bin is riding (_ROBOT:<vehicle>).
	CarriedBy string
}

// carrierNodePrefix names the per-robot synthetic nodes a bin rides on.
// Duplicated from store/bins.CarrierNodePrefix rather than imported: depguard
// forbids www reaching the store. Kept byte-identical; the store constant is the
// definition and carries the reasoning.
const carrierNodePrefix = "_ROBOT:"

// transitOrderFor finds the order behind an in-flight bin: its live claim if it
// has one, otherwise the most recent order that carried it.
func (h *Handlers) transitOrderFor(b *domain.Bin) *domain.Order {
	svc := h.engine.OrderService()
	if b.ClaimedBy != nil {
		if o, err := svc.GetOrder(*b.ClaimedBy); err == nil {
			return o
		}
		return nil
	}
	if ords, err := svc.ListByBin(b.ID, 1); err == nil && len(ords) > 0 {
		return ords[0]
	}
	return nil
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
		switch {
		case b.NodeName == domain.TransitNodeName:
			row.InTransit = true
			row.Stranded = b.ClaimedBy == nil
		case strings.HasPrefix(b.NodeName, carrierNodePrefix):
			row.InTransit = true
			row.CarriedBy = strings.TrimPrefix(b.NodeName, carrierNodePrefix)
		}
		if row.InTransit {
			// The claim first, then the bin's last order. A stranded bin has no
			// claim left — that release is what stranded it — so its history is
			// the only way back to what it was carrying and where it was going.
			if o := h.transitOrderFor(b); o != nil {
				row.TransitPayload = o.PayloadCode
				row.TransitSource = o.SourceNode
				row.TransitDest = o.DeliveryNode
				row.TransitOrderID = o.ID
				row.TransitOrderStatus = string(o.Status)
			}
		}
		rows[i] = row
	}

	// Per-payload bin-type allow-list (keyed by payload code). Empty list = unrestricted,
	// matching the advisory semantics used by FindSourceFIFO / FindEmptyCompatible.
	// One read for the whole table: the codes come back sorted per payload, and
	// the bin types loaded above turn each code back into its id.
	payloadBinTypeIDs := make(map[string][]int64, len(payloads))
	if codesByPayload, btErr := h.engine.PayloadService().BinTypeCodesByPayload(); btErr != nil {
		log.Printf("bins page: list bin types by payload: %v", btErr)
	} else {
		idByCode := make(map[string]int64, len(binTypes))
		for _, bt := range binTypes {
			idByCode[bt.Code] = bt.ID
		}
		for _, p := range payloads {
			ids := make([]int64, 0, len(codesByPayload[p.ID]))
			for _, code := range codesByPayload[p.ID] {
				if id, ok := idByCode[code]; ok {
					ids = append(ids, id)
				}
			}
			payloadBinTypeIDs[p.Code] = ids
		}
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
		Action: engine.BinActionCreated,
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
	// The answer carries the bin as it now is (LC12): what the pop-up and the
	// table row draw, without the history, so the page draws from the answer
	// instead of reading the bin again. "status":"ok" stays for every caller
	// that only checks it. A re-read that fails after a successful action
	// still answers ok, bare, as before.
	fresh, err := h.engine.BinService().GetBin(req.ID)
	if err != nil {
		log.Printf("bin action %s: re-read bin %d: %v", req.Action, req.ID, err)
		h.jsonSuccess(w)
		return
	}
	h.jsonOK(w, binActionAnswer{Status: "ok", binView: h.binDetailView(fresh)})
}

// binActionAnswer is apiBinAction's answer: the bare ok plus the bin's
// history-free detail, flattened, so it reads like GET /api/bins/detail?history=0.
type binActionAnswer struct {
	Status string `json:"status"`
	binView
}

func derefInt64(p *int64) int64 {
	if p != nil {
		return *p
	}
	return 0
}

// --- Bin detail API ---

type binDetailResponse struct {
	Bin      *domain.Bin      `json:"bin"`
	Manifest *domain.Manifest `json:"manifest"`
	Template *domain.Payload  `json:"template,omitempty"`
	// TemplateManifest carries the payload template's per-cycle ratios so the
	// page can show a count per manifest line: bin.uop_remaining x the
	// matching line's parts_per_cycle. The bin manifest stores no count of
	// its own, so without this the page has a part list and no numbers.
	TemplateManifest []*domain.PayloadManifestItem `json:"template_manifest,omitempty"`
	Audit            []*domain.AuditEntry          `json:"audit"`
	CurrentOrder     *domain.Order                 `json:"current_order,omitempty"`
	RecentOrders     []*domain.Order               `json:"recent_orders"`
}

// binView is binDetailResponse without the history: what the bin pop-up and
// the table row draw (LC12). The bin's audit has no limit, so it is read only
// when someone opens the Journal. Same fields, same order, same tags.
type binView struct {
	Bin              *domain.Bin                   `json:"bin"`
	Manifest         *domain.Manifest              `json:"manifest"`
	Template         *domain.Payload               `json:"template,omitempty"`
	TemplateManifest []*domain.PayloadManifestItem `json:"template_manifest,omitempty"`
	CurrentOrder     *domain.Order                 `json:"current_order,omitempty"`
	RecentOrders     []*domain.Order               `json:"recent_orders"`
}

// binDetailView reads everything the detail answer carries except the audit.
func (h *Handlers) binDetailView(b *domain.Bin) binView {
	v := binView{Bin: b}

	// Parse manifest
	if m, err := b.ParseManifest(); err == nil {
		v.Manifest = m
	}

	// Payload template, and its manifest lines — the page derives each part's
	// count from uop_remaining x parts_per_cycle, so the ratios travel with
	// the bin rather than costing the page a second round trip.
	if b.PayloadCode != "" {
		if p, err := h.engine.PayloadService().GetByCode(b.PayloadCode); err == nil {
			v.Template = p
			if items, err := h.engine.PayloadService().ListManifest(p.ID); err == nil {
				v.TemplateManifest = items
			}
		}
	}

	// Current order
	if b.ClaimedBy != nil {
		v.CurrentOrder, _ = h.engine.OrderService().GetOrder(*b.ClaimedBy)
	}

	// Recent orders
	v.RecentOrders, _ = h.engine.OrderService().ListByBin(b.ID, 20)
	if v.RecentOrders == nil {
		v.RecentOrders = []*domain.Order{}
	}
	return v
}

// apiBinDetail answers GET /api/bins/detail?id=N. The default answer is
// unchanged: the bin, its manifest, template, orders and its whole audit.
// ?history=0 is the history-free read (LC12): the same answer without the
// audit, which is what the Bins page reads on open, on a live update and in
// the cycle-count wizard.
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

	v := h.binDetailView(b)
	if r.URL.Query().Get("history") == "0" {
		h.jsonOK(w, v)
		return
	}

	resp := binDetailResponse{
		Bin:              v.Bin,
		Manifest:         v.Manifest,
		Template:         v.Template,
		TemplateManifest: v.TemplateManifest,
		CurrentOrder:     v.CurrentOrder,
		RecentOrders:     v.RecentOrders,
	}

	// Audit log
	resp.Audit, _ = h.engine.AuditService().ListForEntity("bin", id)

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

	// Each result carries its bin's history-free detail (LC12), flattened like
	// apiBinAction's answer, so the page repaints its rows from the answer
	// instead of reading every bin again. An id with no bin carries none.
	type bulkResult struct {
		ID    int64  `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
		*binView
	}

	svc := h.engine.BinService()
	// current re-reads a bin an action may have changed; on a failed re-read
	// the row read before the action is what the answer carries.
	current := func(b *domain.Bin) *binView {
		if fresh, err := svc.GetBin(b.ID); err == nil {
			b = fresh
		}
		v := h.binDetailView(b)
		return &v
	}
	results := make([]bulkResult, 0, len(req.IDs))
	for _, id := range req.IDs {
		b, err := svc.GetBin(id)
		if err != nil {
			results = append(results, bulkResult{ID: id, Error: "not found"})
			continue
		}
		if b.Locked && req.Action != "unlock" {
			v := h.binDetailView(b)
			results = append(results, bulkResult{ID: id, Error: fmt.Sprintf("locked by %s", b.LockedBy), binView: &v})
			continue
		}
		if err := h.executeBinAction(b, req.Action, req.Params); err != nil {
			results = append(results, bulkResult{ID: id, Error: err.Error(), binView: current(b)})
			continue
		}
		results = append(results, bulkResult{ID: id, OK: true, binView: current(b)})
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
