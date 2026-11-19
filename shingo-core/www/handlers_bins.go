package www

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/engine"
	"shingocore/service"
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
	// Process is the process the bin belongs to where it is: the one whose setup
	// names its node, or, in transit, the one its carrying order serves
	// (processLabels). Empty in a supermarket slot no process names.
	Process string
}

// carrierNodePrefix names the per-robot synthetic nodes a bin rides on.
// Duplicated from store/bins.CarrierNodePrefix rather than imported: depguard
// forbids www reaching the store. Kept byte-identical; the store constant is the
// definition and carries the reasoning.
const carrierNodePrefix = "_ROBOT:"

// inFlight reports whether a bin is between nodes, at _TRANSIT or on a robot's
// deck: a row handleBins draws InTransit.
func inFlight(b *domain.Bin) bool {
	return b.NodeName == domain.TransitNodeName || strings.HasPrefix(b.NodeName, carrierNodePrefix)
}

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
	labels := h.processLabelsFor()
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
		var transit *domain.Order
		if row.InTransit {
			// The claim first, then the bin's last order. A stranded bin has no
			// claim left — that release is what stranded it — so its history is
			// the only way back to what it was carrying and where it was going.
			if transit = h.transitOrderFor(b); transit != nil {
				row.TransitPayload = transit.PayloadCode
				row.TransitSource = transit.SourceNode
				row.TransitDest = transit.DeliveryNode
				row.TransitOrderID = transit.ID
				row.TransitOrderStatus = string(transit.Status)
			}
		}
		row.Process = labels.bin(b, transit)
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

	// The Bin Types section: a tab per family, its keys from the one families
	// read, the pickers' options from the one scene_points read
	// (bins_family_keys.go).
	familyCodes, familyKeys := h.carrierFamilies()
	sceneKeys := h.readSceneKeys()
	btFamilies := binTypeFamilies(binTypes, familyKeys, sceneKeys)

	data := map[string]any{
		"Page":                "bins",
		"Bins":                rows,
		"BinTypes":            binTypes,
		"BinTypeFamilies":     btFamilies,
		"BinTypeCount":        binTypeCount(btFamilies),
		"KeyScene":            sceneKeys,
		"CarrierFamilies":     familyCodes,
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
		// A REFUSAL IS LOGGED, one line: who, what, which bin, why. It was a
		// 400 the person saw once and nothing kept (SPR ALN_011, 2026-11-12:
		// the morning's "committed to order" refusals were reconstructed from
		// memory). Not audited: the audit log records changes, and nothing
		// changed.
		actor := h.getUsername(r)
		if actor == "" {
			actor = protocol.AuditActorUI
		}
		log.Printf("bin action refused: actor=%s action=%s bin=%d (%s): %v", actor, req.Action, b.ID, b.Label, err)
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
	answer := binActionAnswer{Status: "ok", binView: h.binDetailView(fresh, h.binRowFactsFor(fresh.ID))}
	if req.Action == "move" {
		answer.Warning = h.binMoveWarning(fresh)
	}
	h.jsonOK(w, answer)
}

// binActionAnswer is apiBinAction's answer: the bare ok plus the bin's
// history-free detail, flattened, so it reads like GET /api/bins/detail?history=0.
// Warning is a sentence the page shows beside a done action (a move's carrier
// check, binMoveWarning); absent when there is none.
type binActionAnswer struct {
	Status  string `json:"status"`
	Warning string `json:"warning,omitempty"`
	binView
}

func derefInt64(p *int64) int64 {
	if p != nil {
		return *p
	}
	return 0
}

// --- Bin detail API ---

// binDetailResponse is GET /api/bins/detail's whole answer: the bin's view and
// its history. It embeds binView, flattened, so the two answers cannot drift.
type binDetailResponse struct {
	binView
	Audit []*domain.AuditEntry `json:"audit"`
}

// binView is what the bin pop-up and the table row draw (LC12): the detail
// answer without the history. The bin's audit has no limit, so it is read only
// when someone opens the Journal.
type binView struct {
	Bin      *domain.Bin      `json:"bin"`
	Manifest *domain.Manifest `json:"manifest"`
	Template *domain.Payload  `json:"template,omitempty"`
	// TemplateManifest carries the payload template's lines so the page can
	// show a count per manifest line, derived from bin.uop_remaining by
	// protocol.ManifestItem's count rule. The bin manifest stores no count of
	// its own, so without this the page has a part list and no numbers.
	TemplateManifest []*domain.PayloadManifestItem `json:"template_manifest,omitempty"`
	CurrentOrder     *domain.Order                 `json:"current_order,omitempty"`
	RecentOrders     []*domain.Order               `json:"recent_orders"`
	// LoadRefusal is the sentence a LOAD of this bin would be refused with
	// (service.LoadRefusalFor), absent when a LOAD would land. The page
	// disables Load Payload and shows it. Read from the bin row and
	// CurrentOrder, both read already.
	LoadRefusal string `json:"load_refusal,omitempty"`
	// Process and HasNotes are the two things a table row shows that the bin
	// record does not carry (binRowFacts). The page paints its Process cell
	// and notes chip from them, as handleBins' row does. Absent is no process
	// and no notes.
	Process  string `json:"process,omitempty"`
	HasNotes bool   `json:"has_notes,omitempty"`
}

// transitOrder is the order behind an in-flight bin, from what the view
// already read: transitOrderFor's answer, without reading it again.
func (v binView) transitOrder() *domain.Order {
	switch {
	case !inFlight(v.Bin):
		return nil
	case v.Bin.ClaimedBy != nil:
		return v.CurrentOrder
	case len(v.RecentOrders) > 0:
		return v.RecentOrders[0]
	}
	return nil
}

// binRowFacts are a row's facts the bin record does not carry, read once for
// however many bins an answer draws: the process each bin belongs to and
// which bins have notes.
type binRowFacts struct {
	labels processLabels
	notes  map[int64]bool
}

// binRowFactsFor reads the row facts for these bins. A failed read draws an
// empty Process and no notes chip, never a guess (processLabelsFor).
func (h *Handlers) binRowFactsFor(binIDs ...int64) binRowFacts {
	notes, err := h.engine.BinService().HasNotes(binIDs)
	if err != nil {
		log.Printf("bin rows: read notes: %v", err)
	}
	return binRowFacts{labels: h.processLabelsFor(), notes: notes}
}

// withRowFacts sets the view's row facts from what it already read.
func (v binView) withRowFacts(facts binRowFacts) binView {
	v.Process = facts.labels.bin(v.Bin, v.transitOrder())
	v.HasNotes = facts.notes[v.Bin.ID]
	return v
}

// binRowDetail is binView cut to what a table row draws (bins-row.js): the
// bin, the order behind it when it is in flight, and the row facts. It reads
// what handleBins reads for a row, so the page can re-read every row at once
// for the price of drawing the page. Not an answer for the pop-up, which
// draws the rest.
func (h *Handlers) binRowDetail(b *domain.Bin, facts binRowFacts) binView {
	v := binView{Bin: b, RecentOrders: []*domain.Order{}}
	if inFlight(b) {
		if o := h.transitOrderFor(b); o != nil {
			if b.ClaimedBy != nil {
				v.CurrentOrder = o
			} else {
				v.RecentOrders = []*domain.Order{o}
			}
		}
	}
	return v.withRowFacts(facts)
}

// binDetailView reads everything the detail answer carries except the audit.
func (h *Handlers) binDetailView(b *domain.Bin, facts binRowFacts) binView {
	v := binView{Bin: b}

	// Parse manifest
	if m, err := b.ParseManifest(); err == nil {
		v.Manifest = m
	}

	// Payload template, and its manifest lines — the page derives each part's
	// count from uop_remaining and these lines, so they travel with the bin
	// rather than costing the page a second round trip.
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
	// Whether a LOAD would be refused, from the two reads above.
	v.LoadRefusal = service.LoadRefusalFor(b, v.CurrentOrder)

	// Recent orders
	v.RecentOrders, _ = h.engine.OrderService().ListByBin(b.ID, 20)
	if v.RecentOrders == nil {
		v.RecentOrders = []*domain.Order{}
	}
	return v.withRowFacts(facts)
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

	v := h.binDetailView(b, h.binRowFactsFor(b.ID))
	if r.URL.Query().Get("history") == "0" {
		h.jsonOK(w, v)
		return
	}

	resp := binDetailResponse{binView: v}
	resp.Audit, _ = h.engine.AuditService().ListForEntity("bin", id)
	h.jsonOK(w, resp)
}

// apiBinRows answers GET /api/bins/rows: Bins-table rows, as binRowDetail
// reads them, for the page to repaint without a reload.
//
//   - ids=1,2 names bins. One that no longer exists is listed in gone, for the
//     page to drop its row.
//   - orders=7,8 adds the carts those orders carry. An order's claim and its
//     release change its carts' rows (the order chip, the route) and no bin
//     event says so.
//   - neither is every bin: the page's re-read after its live link dropped,
//     when it cannot know what it missed.
func (h *Handlers) apiBinRows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ids := queryIDs(q, "ids", maxBinRowsAsked)
	orderIDs := queryIDs(q, "orders", maxBinRowsAsked)
	svc := h.engine.BinService()

	var list []*domain.Bin
	gone := []int64{}
	if len(ids) == 0 && len(orderIDs) == 0 {
		all, err := svc.ListBins()
		if err != nil {
			h.jsonError(w, "could not read the bins: "+err.Error(), http.StatusInternalServerError)
			return
		}
		list = all
	} else {
		seen := make(map[int64]bool, len(ids))
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			b, err := svc.GetBin(id)
			if errors.Is(err, sql.ErrNoRows) {
				gone = append(gone, id)
				continue
			}
			if err != nil {
				h.jsonError(w, fmt.Sprintf("could not read bin %d: %v", id, err), http.StatusInternalServerError)
				return
			}
			list = append(list, b)
		}
		for _, oid := range orderIDs {
			carts, err := svc.CartsOfOrder(oid)
			if err != nil {
				h.jsonError(w, fmt.Sprintf("could not read the carts of order %d: %v", oid, err), http.StatusInternalServerError)
				return
			}
			for _, b := range carts {
				if !seen[b.ID] {
					seen[b.ID] = true
					list = append(list, b)
				}
			}
		}
	}

	binIDs := make([]int64, len(list))
	for i, b := range list {
		binIDs[i] = b.ID
	}
	facts := h.binRowFactsFor(binIDs...)
	rows := make([]binView, len(list))
	for i, b := range list {
		rows[i] = h.binRowDetail(b, facts)
	}
	h.jsonOK(w, map[string]any{"rows": rows, "gone": gone})
}

// maxBinRowsAsked bounds each list GET /api/bins/rows takes. The page asks for
// the bins and orders one burst of events named, which is a handful.
const maxBinRowsAsked = 200

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
	results := make([]bulkResult, 0, len(req.IDs))
	// drawn is the bin each result draws, nil for an id with no bin: the bin
	// as an action left it, re-read, or as read before when that re-read fails
	// or no action ran (a locked bin).
	drawn := make([]*domain.Bin, 0, len(req.IDs))
	for _, id := range req.IDs {
		b, err := svc.GetBin(id)
		if err != nil {
			results = append(results, bulkResult{ID: id, Error: "not found"})
			drawn = append(drawn, nil)
			continue
		}
		res := bulkResult{ID: id, OK: true}
		if b.Locked && req.Action != "unlock" {
			res = bulkResult{ID: id, Error: fmt.Sprintf("locked by %s", b.LockedBy)}
		} else {
			if err := h.executeBinAction(b, req.Action, req.Params); err != nil {
				res = bulkResult{ID: id, Error: err.Error()}
			}
			if fresh, err := svc.GetBin(b.ID); err == nil {
				b = fresh
			}
		}
		results = append(results, res)
		drawn = append(drawn, b)
	}

	// The row facts are read once, after every action ran, so a note an action
	// added is on its row.
	binIDs := make([]int64, 0, len(drawn))
	for _, b := range drawn {
		if b != nil {
			binIDs = append(binIDs, b.ID)
		}
	}
	facts := h.binRowFactsFor(binIDs...)
	for i, b := range drawn {
		if b != nil {
			v := h.binDetailView(b, facts)
			results[i].binView = &v
		}
	}
	h.jsonOK(w, map[string]any{"results": results})
}

// --- Request move API ---

// apiRequestBinMove is the bins page's "Request move": a plain move of this bin
// to the node picked in the detail panel's Destination list, carried by a robot.
//
// It files the move through the Orders page's manual-move door for a named bin
// (fileBinMove → CreateBinMove, by label), so the order is born by the one
// builder and every refusal — claimed, locked, a full or disabled destination,
// the carrier rule, a buried bin with nowhere to park its blockers — is that
// door's, with its sentence and status. The bins page names the bin by id; the
// only thing this handler adds is reading its label.
func (h *Handlers) apiRequestBinMove(w http.ResponseWriter, r *http.Request) {
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
	h.fileBinMove(w, engine.BinMoveRequest{
		Selection:  engine.BinSelectionByLabel,
		BinLabel:   b.Label,
		DestNodeID: req.DestinationNodeID,
		StationID:  "core-operator",
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

// carrierFamilies is the page's one read of the carrier families: their codes
// for the Bin Types form's Family list, and each family's keys by id for the
// family tabs' key pickers. A failed read leaves both empty: the box still
// takes a typed code, and no tab offers a key to save (binTypeFamilies).
func (h *Handlers) carrierFamilies() ([]string, map[int64]pageFamilyKeys) {
	families, err := h.engine.BinService().ListCarrierFamilies()
	if err != nil {
		log.Printf("bins: list carrier families: %v", err)
		return nil, nil
	}
	codes := make([]string, 0, len(families))
	keys := make(map[int64]pageFamilyKeys, len(families))
	for _, f := range families {
		codes = append(codes, f.Code)
		keys[f.ID] = pageFamilyKeys{lift: f.LiftBinTask, wait: f.WaitBinTask}
	}
	return codes, keys
}
