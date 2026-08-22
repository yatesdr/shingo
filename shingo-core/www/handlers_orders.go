package www

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/shared"
	"shingocore/domain"
	"shingocore/engine"
	"shingocore/fleet"
)

func (h *Handlers) handleOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := h.listOrdersForPage(r)
	if err != nil {
		log.Printf("orders page: list orders: %v", err)
	}

	faultLines, noticeCount := h.faultLinesFor(orders)
	data := map[string]any{
		"Page":               "orders",
		"Orders":             orders,
		"FilterStatus":       r.URL.Query().Get("status"),
		"QueueCodeCounts":    countQueueCodes(orders),
		"QueueCodeLabels":    queueCodeLabels(),
		"FaultLines":         faultLines,
		"FaultedNoticeCount": noticeCount,
	}
	h.render(w, r, "orders.html", data)
}

// handleOrdersRows renders just the table rows for the current filter.
//
// It exists so the board can refresh without reloading the page. The rows come
// from the same partial the page renders, so there is no second copy of the row
// markup in JS — which is what a "build the row client-side from /api/orders/N"
// refresh would have required.
//
// Same filter semantics as handleOrders, read from the same query params, so a
// refresh cannot quietly widen or narrow what the page is showing.
func (h *Handlers) handleOrdersRows(w http.ResponseWriter, r *http.Request) {
	orders, err := h.listOrdersForPage(r)
	if err != nil {
		log.Printf("orders rows: list orders: %v", err)
	}
	faultLines, noticeCount := h.faultLinesFor(orders)
	tmpl, ok := h.tmpls["orders.html"]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	// The notice count rides a header so the page can update its chip without a
	// second request or a JSON envelope wrapping HTML.
	w.Header().Set("X-Faulted-Notice-Count", strconv.Itoa(noticeCount))
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	shared.SetHTMLContentType(w)
	if err := tmpl.ExecuteTemplate(w, "orders-rows", map[string]any{
		"Orders":        orders,
		"FaultLines":    faultLines,
		"Authenticated": h.isAuthenticated(r),
	}); err != nil {
		log.Printf("orders rows: %v", err)
	}
}

// listOrdersForPage is the order list behind both the page and the row
// fragment. One function so the two cannot answer the same query differently.
func (h *Handlers) listOrdersForPage(r *http.Request) ([]*domain.Order, error) {
	status := r.URL.Query().Get("status")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	svc := h.engine.OrderService()
	switch {
	case status == "":
		return svc.ListActiveOrders()
	case status == "all":
		return svc.ListOrders("", limit)
	default:
		return svc.ListOrders(status, limit)
	}
}

// faultLinesFor builds the rendered fault line for each faulted order on the
// page, keyed by order id, and counts the ones over the notice threshold.
//
// Only NOTICE faults are counted. A replan is not actionable and a chip that
// fires on all 730 faults in a month teaches the floor to ignore the chip.
//
// One history read per faulted order. That is a handful on any real page: if a
// board ever lists hundreds of faulted orders at once, the fault is the plant's,
// not the query's.
func (h *Handlers) faultLinesFor(orders []*domain.Order) (map[int64]template.HTML, int) {
	lines := make(map[int64]template.HTML)
	notice := 0
	cfg := h.engine.AppConfig()
	grace, noticeAfter := cfg.RDS.FaultGrace, cfg.RDS.FaultNoticeAfter
	svc := h.engine.OrderService()
	now := clock.Now().UTC()

	for _, o := range orders {
		if o.Status != protocol.StatusFaulted {
			continue
		}
		var ref protocol.TermRef
		var since, deadline time.Time
		// A read failure costs the clock, not the row: the sentence still
		// renders and simply has no duration under it.
		if hrow, err := svc.LatestOrderHistoryForStatus(o.ID, protocol.StatusFaulted); err != nil {
			log.Printf("orders page: fault row for order %d: %v", o.ID, err)
		} else if hrow != nil {
			since = hrow.CreatedAt
			deadline = since.Add(grace)
			if hrow.Ref != nil {
				ref = *hrow.Ref
			}
		}
		line := protocol.BuildFaultLine(ref, since, deadline, now, noticeAfter)
		if line.Notice {
			notice++
		}
		// Safe: FaultLine.HTML escapes every interpolated value.
		lines[o.ID] = template.HTML(line.HTML())
	}
	return lines, notice
}

// countQueueCodes tallies the active orders by their structured queue_code so the
// orders page can show, at a glance, WHY the queued set is stuck — e.g. "3
// waiting for material, 2 waiting for a slot". The code is the analytic dimension
// behind the free-text queue_reason sentence; grouping here (rather than a SQL
// GROUP BY) reuses the order list already loaded for the page. Empty/blank codes
// (non-queued or pre-schema rows) bucket as "" and are omitted from the display.
func countQueueCodes(orders []*domain.Order) map[string]int {
	counts := make(map[string]int)
	for _, o := range orders {
		if !protocol.IsAcquiring(o.Status) {
			continue
		}
		counts[o.QueueCode]++
	}
	return counts
}

// queueCodeLabels maps each queue code to a short display label for the orders
// page summary. Single source for the operator-facing wording so a new code is
// added in both the formatter and here together.
func queueCodeLabels() map[string]string {
	return map[string]string{
		string(protocol.QueueWaitingForMaterial): "Waiting for material",
		string(protocol.QueueWaitingForSlot):     "Waiting for a slot",
		string(protocol.QueueStorageRearranging): "Rearranging storage",
		string(protocol.QueueWaitingForPartner):  "Waiting for partner robot",
		string(protocol.QueueFleetUnavailable):   "Robot system not responding",
	}
}

func (h *Handlers) handleOrderDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	// There is no separate order detail page any more. It rendered the same
	// manifest as the row-click modal, so maintaining a second surface —
	// with its own layout, its own controls and its own way of going stale
	// — bought nothing. /orders?open=N opens that order's modal on load, so
	// an order is still linkable and bookmarkable; this redirect keeps old
	// links, bookmarks and the "View Order" links on the mission pages
	// landing on the order rather than 404ing.
	http.Redirect(w, r, "/orders?open="+strconv.FormatInt(id, 10), http.StatusMovedPermanently)
}

func (h *Handlers) apiTerminateOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderID int64 `json:"order_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	actor := h.getUsername(r)
	if err := h.orchestration.TerminateOrder(req.OrderID, actor); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

// apiHardReleaseOrder is W3's door: advance a dwelling order past its wait when
// the mechanism that should have done it is wedged.
//
// SAME GUARDS AS ITS NEIGHBOURS, deliberately — it sits in the protected group
// beside /orders/terminate and /robots/force-complete, which are the comparable
// "an engineer has decided" verbs. It is not a new privilege class, and the
// actor is recorded because a hard release of a STATION-owned wait overrides a
// cell that may still be occupied.
//
// The station HMI must never offer this. The board offers Release only for waits
// the station owns — a read now that ownership is carried, not a guess — and
// this door exists for the other kind.
func (h *Handlers) apiHardReleaseOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderID int64 `json:"order_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	actor := h.getUsername(r)
	if err := h.orchestration.HardReleaseOrder(req.OrderID, actor); err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiListOrders(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	orders, err := h.engine.OrderService().ListOrders(status, limit)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}

func (h *Handlers) apiGetOrder(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	order, err := h.engine.OrderService().GetOrder(id)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}
	h.jsonOK(w, order)
}

func (h *Handlers) apiGetOrderEnriched(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseIDParam(w, r, "id")
	if !ok {
		return
	}
	svc := h.engine.OrderService()
	order, err := svc.GetOrder(id)
	if err != nil {
		h.jsonError(w, "not found", http.StatusNotFound)
		return
	}

	type enrichedOrder struct {
		Order        *domain.Order            `json:"order"`
		History      []*domain.OrderHistory   `json:"history,omitempty"`
		Bin          *domain.Bin              `json:"bin,omitempty"`
		BinManifest  *domain.Manifest         `json:"bin_manifest,omitempty"`
		SourceNode   *domain.Node             `json:"source_node,omitempty"`
		DeliveryNode *domain.Node             `json:"delivery_node,omitempty"`
		Children     []*domain.Order          `json:"children,omitempty"`
		Parent       *domain.Order            `json:"parent,omitempty"`
		VendorDetail *fleet.VendorOrderDetail `json:"vendor_detail,omitempty"`
		Robot        *fleet.RobotStatus       `json:"robot,omitempty"`
		// CanCancel drives the manifest's Terminate button. Computed here
		// rather than re-derived in JS so the client-rendered controls use
		// the same gate as the server-rendered list rows — a status list
		// duplicated into JS is exactly how the old template denylists
		// drifted from the engine.
		CanCancel bool `json:"can_cancel"`
		// CanHardRelease drives the Hard Release button, and is computed here for
		// the same reason CanCancel is: the control may only appear where the
		// handler would accept it. It is TRUE only for a wait CORE owns — a
		// station-owned wait belongs to the station's board, and the handler
		// refuses it too.
		CanHardRelease bool `json:"can_hard_release"`
	}

	result := enrichedOrder{
		Order:          order,
		CanCancel:      canCancelStatus(order.Status),
		CanHardRelease: canHardReleaseOrder(order),
	}

	result.History, _ = svc.ListOrderHistory(id)

	if order.BinID != nil {
		result.Bin, _ = h.engine.BinService().GetBin(*order.BinID)
		result.BinManifest, _ = h.engine.BinService().GetManifest(*order.BinID)
	}
	if order.SourceNode != "" {
		result.SourceNode, _ = h.engine.NodeService().GetByName(order.SourceNode)
	}
	if order.DeliveryNode != "" {
		result.DeliveryNode, _ = h.engine.NodeService().GetByName(order.DeliveryNode)
	}
	if order.ParentOrderID != nil {
		result.Parent, _ = svc.GetOrder(*order.ParentOrderID)
	}

	children, _ := svc.ListChildOrders(id)
	if len(children) > 0 {
		result.Children = children
	}

	if order.VendorOrderID != "" {
		if vc, ok := h.engine.Fleet().(fleet.VendorCommander); ok {
			result.VendorDetail, _ = vc.GetVendorOrderDetail(order.VendorOrderID)
		}
	}
	if order.RobotID != "" {
		if rs, ok := h.engine.GetCachedRobotStatus(order.RobotID); ok {
			result.Robot = &rs
		}
	}

	h.jsonOK(w, result)
}

func (h *Handlers) apiSetOrderPriority(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderID  int64 `json:"order_id"`
		Priority int   `json:"priority"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if _, err := h.engine.OrderService().SetPriority(req.OrderID, req.Priority); err != nil {
		if err.Error() == "order not found" {
			h.jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonSuccess(w)
}

func (h *Handlers) apiManualOrderSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderType    string `json:"order_type"`
		SourceNode   string `json:"source_node"`
		DeliveryNode string `json:"delivery_node"`
		StagingNode  string `json:"staging_node"`
		Priority     int    `json:"priority"`
		Description  string `json:"description"`
		PayloadCode  string `json:"payload_code"`
		BinLabel     string `json:"bin_label"`
		Quantity     int    `json:"quantity"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if req.OrderType == "" {
		h.jsonError(w, "order_type is required", http.StatusBadRequest)
		return
	}
	if req.Quantity <= 0 {
		req.Quantity = 1
	}

	orderUUID := uuid.New().String()
	src := protocol.Address{Role: protocol.RoleCore, Station: "core-operator"}
	dst := protocol.Address{Role: protocol.RoleCore}

	// EVERY SPOT ORDER ON THIS PAGE IS no_demand, and each of the six create
	// sites below says so itself. Four of them synthesize a protocol request and
	// hand it to the real intake path, which classifies from the envelope — an
	// unstamped request would arrive indistinguishable from an old Edge's and
	// land ORPHAN, putting an operator's deliberate action into the bucket that
	// exists to mean "we lost a demand link." The other two write a domain.Order
	// directly and never reach a classifier at all.
	//
	// Stamped at the create site rather than inferred from StationID ==
	// "core-operator" downstream: the class is known HERE, and a magic-string read at
	// intake is the inference this whole column exists to make unnecessary.

	switch req.OrderType {
	case "staged":
		h.submitSpotComplexOrder(w, req.SourceNode, req.StagingNode, req.DeliveryNode,
			req.PayloadCode, req.Description, req.Priority, orderUUID, src, dst)
		return
	case "retrieve_specific":
		h.submitSpotRetrieveSpecific(w, req.BinLabel, req.DeliveryNode, req.Description, req.Priority, orderUUID)
		return
	}

	// Transport types: move, retrieve, retrieve_empty. retrieve_empty is now a
	// first-class OrderType — pass it through as-is. (Pre-cleanup this site
	// translated retrieve_empty → retrieve + RetrieveEmpty bool; lifecycle_service
	// still normalizes the bool form for old edge callers, but core's own forms
	// emit the typed value directly.)
	orderType := protocol.OrderType(req.OrderType)

	// Batch retrieve: create N independent orders
	if req.Quantity > 20 {
		req.Quantity = 20
	}
	if req.Quantity > 1 && (orderType == protocol.OrderTypeRetrieve || orderType == protocol.OrderTypeRetrieveEmpty) {
		var firstOrderID int64
		var firstStatus string
		created := 0
		for i := 1; i <= req.Quantity; i++ {
			batchUUID := fmt.Sprintf("%s-%d", orderUUID, i)
			orderReq := &protocol.OrderRequest{
				OrderUUID:    batchUUID,
				OrderType:    orderType,
				PayloadCode:  req.PayloadCode,
				PayloadDesc:  req.Description,
				Quantity:     1,
				SourceNode:   req.SourceNode,
				DeliveryNode: req.DeliveryNode,
				Priority:     req.Priority,
				OriginClass:  protocol.OriginClassNoDemand,
			}
			env, err := protocol.NewEnvelope(protocol.TypeOrderRequest, src, dst, orderReq)
			if err != nil {
				log.Printf("spot batch order %d/%d envelope error: %v", i, req.Quantity, err)
				continue
			}
			h.engine.Dispatcher().HandleOrderRequest(env, orderReq)
			// Read every one back, not just the first. `count` used to echo
			// the number ASKED for, so a batch where every envelope failed
			// still answered 200 with "20 orders created" — and an operator
			// who asked for twenty and got none was told they got twenty.
			o, err := h.engine.OrderService().GetOrderByUUID(batchUUID)
			if err != nil || protocol.IsTerminal(o.Status) {
				continue
			}
			created++
			if firstOrderID == 0 {
				firstOrderID = o.ID
				firstStatus = string(o.Status)
			}
		}
		if created == 0 {
			h.jsonError(w, fmt.Sprintf("order creation failed: none of the %d orders could be created", req.Quantity), http.StatusInternalServerError)
			return
		}
		h.jsonOK(w, map[string]any{
			"order_id": firstOrderID,
			"status":   firstStatus,
			"count":    created,
		})
		return
	}

	orderReq := &protocol.OrderRequest{
		OrderUUID:    orderUUID,
		OrderType:    orderType,
		PayloadCode:  req.PayloadCode,
		PayloadDesc:  req.Description,
		Quantity:     1,
		SourceNode:   req.SourceNode,
		DeliveryNode: req.DeliveryNode,
		Priority:     req.Priority,
		OriginClass:  protocol.OriginClassNoDemand,
	}

	env, err := protocol.NewEnvelope(protocol.TypeOrderRequest, src, dst, orderReq)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.Dispatcher().HandleOrderRequest(env, orderReq)
	h.readBackManualOrder(w, orderUUID)
}

func (h *Handlers) submitSpotComplexOrder(w http.ResponseWriter,
	sourceNode, stagingNode, deliveryNode, payloadCode, desc string,
	priority int, orderUUID string, src, dst protocol.Address) {

	if sourceNode == "" {
		h.jsonError(w, "source node is required for staged orders", http.StatusBadRequest)
		return
	}
	if stagingNode == "" {
		h.jsonError(w, "staging node is required for staged orders", http.StatusBadRequest)
		return
	}
	if deliveryNode == "" {
		h.jsonError(w, "delivery node is required for staged orders", http.StatusBadRequest)
		return
	}

	payloadCode = h.payloadAtSource(sourceNode, payloadCode)

	complexReq := &protocol.ComplexOrderRequest{
		OrderUUID:   orderUUID,
		PayloadCode: payloadCode,
		PayloadDesc: desc,
		Quantity:    1,
		Priority:    priority,
		OriginClass: protocol.OriginClassNoDemand,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: sourceNode},
			// DECLARED, because this form is the one place Core knows. The role
			// test cannot recognise a staging node — it is a station with no
			// parent, so isConcreteStorageDropoff rejects it and both destination
			// gates stand down, leaving the node reserved by nothing and checked
			// by nothing — so a second order takes it while the first robot is on
			// its way.
			//
			// Everywhere else the Edge has to declare it, because the staging
			// designation lives in the cell config Core does not have. HERE the
			// operator has just typed it into a field named staging_node and the
			// handler has already refused the request without one. There is no
			// inference in this: the request says which node is the staging node.
			{Action: "dropoff", Node: stagingNode, ExclusiveSlot: true},
			{Action: "wait"},
			{Action: "pickup", Node: stagingNode},
			// NOT declared. deliveryNode is where the material is going, and on
			// this form that is routinely a LINE node. Gating a line dropoff
			// re-creates the deadlock 2b05dce fixed.
			{Action: "dropoff", Node: deliveryNode},
		},
	}

	env, err := protocol.NewEnvelope(protocol.TypeComplexOrderRequest, src, dst, complexReq)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.Dispatcher().HandleComplexOrderRequest(env, complexReq)
	h.readBackManualOrder(w, orderUUID)
}

// payloadAtSource answers what is in the bin an order is about to pick up, so
// the screen does not have to ask.
//
// The staged form asked for a payload beside the source node, which is the same
// question twice: the source names the place, the place holds the bin, and the
// bin knows what it is. Two answers to one question can disagree, and the order
// carried the operator's. That answer is not decoration — dispatch reads it to
// pick the robot group and the advanced load sequence
// (robotGroupForPayload / loadSequenceForPayload in dispatch/dispatcher.go), so
// a mis-click sent the wrong robot to a real bin. The allocator already logs the
// disagreement when it finally sees both (allocator.go), which is late.
//
// A GROUP source is a different question and keeps the operator's answer. There
// the payload is not a description of a bin we can already see — it is the
// SELECTOR the resolver uses to choose one among the group's children
// (resolveStepNode consults payloadCode only for synthetic NGRPs). Overwriting
// it would be answering the question the operator was actually asked. So:
// derive when the source names one place, ask when it names a set.
//
// Anything unreadable — no such node, a lookup error, or more than one bin
// sitting there — returns what the caller sent and lets the rest of the door
// judge it. This resolves the name the same way intake does
// (GetNodeByDotName), so the door and the resolver cannot disagree about which
// node a name means.
func (h *Handlers) payloadAtSource(sourceNode, asked string) string {
	node, err := h.engine.NodeService().GetByDotName(sourceNode)
	if err != nil || node == nil || node.IsSynthetic {
		return asked
	}
	binsThere, err := h.engine.NodeService().ListBinsByNode(node.ID)
	if err != nil || len(binsThere) != 1 {
		return asked
	}
	found := binsThere[0].PayloadCode
	// Not silent: the screen no longer offers the field, but an API client
	// still can, and being overruled without a word is how a caller ends up
	// debugging the wrong thing.
	if asked != "" && asked != found {
		log.Printf("staged order at %s: bin %s holds %q, request asked for %q — using the bin's",
			sourceNode, binsThere[0].Label, found, asked)
	}
	return found
}

// readBackManualOrder answers a submission that is on its way.
//
// It used to answer both outcomes. Success and failure came back through here
// alike, so a rejected order was reported as 200 with {"status":"failed"} and
// the screen printed "Order #14 created (failed)" — created, in the same
// sentence as the word for what actually happened. Anything reading the HTTP
// status alone, which is most things, saw a success.
//
// The rule now is that a 2xx means an order exists and is on its way. A
// terminal row is an error response, not a success carrying bad news.
func (h *Handlers) readBackManualOrder(w http.ResponseWriter, orderUUID string) {
	order, err := h.engine.OrderService().GetOrderByUUID(orderUUID)
	if err != nil {
		h.jsonError(w, "order creation failed: it was submitted but could not be read back ("+err.Error()+")", http.StatusInternalServerError)
		return
	}
	// Intake can reject synchronously: the planner refuses and fails the row
	// before this line runs. The order exists, but nothing is carrying it.
	if protocol.IsTerminal(order.Status) {
		detail := order.ErrorDetail
		if detail == "" {
			detail = "the order was " + string(order.Status) + " as soon as it was submitted"
		}
		h.jsonError(w, "order creation failed: "+detail, http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{
		"order_id": order.ID,
		"status":   order.Status,
	})
}

// submitSpotRetrieveSpecific is the operator's half of the one bin-move door:
// name a bin, and wherever it is now is the source.
//
// Everything below the request shape is shared with the engineers' page. The
// two used to be separate functions that had drifted twelve ways, and each
// difference was a bug waiting its turn — the quantity, the same-node move, the
// lost race reported as a server fault, the missing creation history. They are
// one function now, and this is the adapter for one of its two input shapes.
func (h *Handlers) submitSpotRetrieveSpecific(w http.ResponseWriter, binLabel, deliveryNode, desc string, priority int, orderUUID string) {
	result, err := h.orchestration.CreateBinMove(engine.BinMoveRequest{
		Selection:    engine.BinSelectionByLabel,
		BinLabel:     binLabel,
		DestNodeName: deliveryNode,
		StationID:    "core-operator",
		EdgeUUID:     orderUUID,
		Priority:     priority,
		Desc:         desc,
	})
	if err != nil {
		h.jsonError(w, err.Error(), binMoveStatus(err))
		return
	}
	// A lane that would not take the move yet is not a refusal — the order is
	// real and the scanner drives it in when the lane clears. Reporting it as
	// `dispatched` would tell the operator a robot is coming that is not; the
	// status and the reason together are what the screen renders.
	if result.Queued {
		h.jsonOK(w, map[string]any{
			"order_id":     result.OrderID,
			"status":       protocol.StatusQueued,
			"queue_reason": result.QueueReason,
		})
		return
	}
	h.jsonOK(w, map[string]any{
		"order_id": result.OrderID,
		"status":   protocol.StatusDispatched,
	})
}

func (h *Handlers) apiListAvailableBins(w http.ResponseWriter, r *http.Request) {
	bins, err := h.engine.BinService().ListBins()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type availableBin struct {
		Label       string `json:"label"`
		NodeName    string `json:"node_name"`
		Zone        string `json:"zone"`
		PayloadCode string `json:"payload_code"`
	}

	// Build a map of node_id -> zone for quick lookup
	nodes, _ := h.engine.NodeService().ListNodes()
	nodeZone := make(map[int64]string, len(nodes))
	for _, n := range nodes {
		nodeZone[n.ID] = n.Zone
	}

	var result []availableBin
	for _, b := range bins {
		if b.ClaimedBy != nil || b.NodeID == nil {
			continue
		}
		zone := nodeZone[*b.NodeID]
		result = append(result, availableBin{
			Label:       b.Label,
			NodeName:    b.NodeName,
			Zone:        zone,
			PayloadCode: b.PayloadCode,
		})
	}

	h.jsonOK(w, result)
}
