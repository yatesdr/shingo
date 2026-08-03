package www

import (
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"errors"
	"shingo/protocol"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/store/reservations"
)

func (h *Handlers) handleOrders(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	svc := h.engine.OrderService()
	var orders []*domain.Order
	var err error
	switch {
	case status == "":
		orders, err = svc.ListActiveOrders()
	case status == "all":
		orders, err = svc.ListOrders("", limit)
	default:
		orders, err = svc.ListOrders(status, limit)
	}
	if err != nil {
		log.Printf("orders page: list orders: %v", err)
	}

	data := map[string]any{
		"Page":            "orders",
		"Orders":          orders,
		"FilterStatus":    status,
		"QueueCodeCounts": countQueueCodes(orders),
		"QueueCodeLabels": queueCodeLabels(),
	}
	h.render(w, r, "orders.html", data)
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
	}

	result := enrichedOrder{Order: order, CanCancel: canCancelStatus(order.Status)}

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
	src := protocol.Address{Role: protocol.RoleCore, Station: "core-spot"}
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
	// "core-spot" downstream: the class is known HERE, and a magic-string read at
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

	complexReq := &protocol.ComplexOrderRequest{
		OrderUUID:   orderUUID,
		PayloadCode: payloadCode,
		PayloadDesc: desc,
		Quantity:    1,
		Priority:    priority,
		OriginClass: protocol.OriginClassNoDemand,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: sourceNode},
			{Action: "dropoff", Node: stagingNode},
			{Action: "wait"},
			{Action: "pickup", Node: stagingNode},
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

func (h *Handlers) submitSpotRetrieveSpecific(w http.ResponseWriter, binLabel, deliveryNode, desc string, priority int, orderUUID string) {
	if binLabel == "" {
		h.jsonError(w, "bin_label is required", http.StatusBadRequest)
		return
	}
	if deliveryNode == "" {
		h.jsonError(w, "delivery node is required", http.StatusBadRequest)
		return
	}

	bin, err := h.engine.BinService().GetByLabel(binLabel)
	if err != nil {
		h.jsonError(w, "bin not found: "+binLabel, http.StatusBadRequest)
		return
	}
	if bin.ClaimedBy != nil {
		h.jsonError(w, "bin is already claimed by order #"+strconv.FormatInt(*bin.ClaimedBy, 10), http.StatusConflict)
		return
	}
	// A bin is held in two stages: a soft reservation taken at planning, then a
	// hard claim taken immediately before dispatch. For the whole window between
	// them an in-flight order's bin still has claimed_by NULL, so reading only
	// the claim showed a bin somebody already has as free. The request then got
	// as far as the reservation below, failed there, and left its order row
	// behind in pending with nothing to dispatch, fail or clean it up.
	//
	// The engineer's door has always checked both and skips such a bin while
	// scanning. Same question, so the same answer: somebody else has it.
	if bin.HasPendingReservation {
		h.jsonError(w, "bin "+bin.Label+" is already spoken for by another order — pick another bin or wait for that one to finish", http.StatusConflict)
		return
	}
	if bin.NodeID == nil {
		h.jsonError(w, "bin has no assigned node", http.StatusBadRequest)
		return
	}

	sourceNode, err := h.engine.NodeService().GetNode(*bin.NodeID)
	if err != nil {
		h.jsonError(w, "source node not found", http.StatusInternalServerError)
		return
	}
	destNode, err := h.engine.NodeService().GetByName(deliveryNode)
	if err != nil {
		h.jsonError(w, "delivery node not found: "+deliveryNode, http.StatusBadRequest)
		return
	}

	// Asking to move a bin to where it already is. The engineer's door has always
	// refused this, five other places in the system refuse it, and the wire
	// protocol reserves a terminal code for it — this door was the exception.
	//
	// It runs BEFORE the occupancy gate on purpose. The gate does catch most of
	// these incidentally, because the bin is at the destination and so the
	// destination reads as occupied — but it then tells the operator the spot is
	// taken, which sends them to go clear a node whose only occupant is the bin
	// they were trying to move. The specific answer has to win over the generic
	// one.
	//
	// And the gate does not catch all of them: it defers on lane nodes, so a bin
	// sitting in a lane went straight to the fleet with nothing stopping it.
	//
	// Scoped to this door only. It must never move into the shared writer or the
	// dispatch tail: a complex order's first and last step are legitimately the
	// same node — a robot lifts a bin off a position, takes it away, and brings a
	// different one back — so a check placed there would break changeovers.
	if sourceNode.ID == destNode.ID {
		h.jsonError(w, "bin "+bin.Label+" is already at "+destNode.Name, http.StatusBadRequest)
		return
	}

	// Only STORAGE destinations were gated, via the slot reservation further
	// down. A lineside one was not, so a bin could be sent to a line node that
	// already held one and the two would contend for the same physical spot.
	// This is the check the wire path and the scanner already consult; the
	// difference was that this door did not ask.
	//
	// Before the insert and before any claim, so a refusal leaves the source bin
	// exactly as it was. Rejecting later would strand a pending order and leave
	// a reservation the next move would be told to wait behind.
	if preview := h.engine.Dispatcher().PreviewDropoffCapacity(destNode.Name); preview.Blocked {
		h.jsonError(w, preview.Reason, http.StatusConflict)
		return
	}

	order := &domain.Order{
		EdgeUUID:     orderUUID,
		StationID:    "core-spot",
		OrderType:    protocol.OrderTypeMove,
		Status:       protocol.StatusPending,
		Quantity:     1,
		SourceNode:   sourceNode.Name,
		DeliveryNode: destNode.Name,
		Priority:     priority,
		PayloadDesc:  desc,
		BinID:        &bin.ID,
		OriginClass:  protocol.OriginClassNoDemand,
	}
	orders := h.engine.OrderService()
	if err := orders.Create(order); err != nil {
		h.jsonError(w, "failed to create order: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// The status column already says pending — the INSERT set it. This is for
	// the HISTORY row, which is written by transitions rather than by the
	// insert, so an order created directly at pending has no entry saying it
	// ever started and its timeline begins at whatever happened next.
	//
	// The engineer's bin-move door has always made this call for the same
	// reason. Skipping it here meant the one order class a person creates by
	// hand was the class whose record did not say when it was created — on the
	// surface an operator would go to when asking what happened. Logged rather
	// than returned, as on the other door: the order is real and dispatchable
	// either way, and failing the request over a missing audit line would be
	// the worse trade.
	if err := h.engine.Dispatcher().Lifecycle().MarkPending(order, desc); err != nil {
		log.Printf("www: mark spot bin-move %d pending: %v", order.ID, err)
	}

	// Rule 1: soft-acquire the bin (a pending reservation), then hard-claim it at
	// dispatch via ConfirmForDispatch (which also claims a storage dropoff slot).
	// Rollback below releases the reservation if dispatch fails.
	if err := h.engine.BinManifest().ReserveForDispatch(bin.ID, order.ID); err != nil {
		// The order row exists by now, so a failure here has to fail it too or it
		// sits pending forever with nothing to dispatch, fail or clean it up.
		if ferr := orders.FailAtomic(order.ID, "bin taken by another order before reservation"); ferr != nil {
			log.Printf("www: fail spot bin-move %d after losing the bin: %v", order.ID, ferr)
		}
		// Somebody else got the bin in the moment between the check above and
		// this call. That is a race with another person, not a fault, and the
		// operator can act on it — so it reads like the claimed-bin answer they
		// already get, rather than as a server error.
		if errors.Is(err, reservations.ErrReservationConflict) {
			h.jsonError(w, "bin "+bin.Label+" was taken a moment ago — try again", http.StatusConflict)
			return
		}
		h.jsonError(w, "failed to reserve bin: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := h.engine.Dispatcher().ConfirmForDispatch(order, bin.ID, sourceNode, destNode); err != nil {
		if rerr := orders.ReleaseReservation(order.ID, bin.ID); rerr != nil {
			log.Printf("www: release reservation for bin %d after confirm failure: %v", bin.ID, rerr)
		}
		h.jsonError(w, "failed to claim bin at dispatch: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := h.engine.Dispatcher().DispatchDirect(order, sourceNode, destNode); err != nil {
		// Coupled rollback: clear the hard claim AND release the reservation, so a
		// failed dispatch can't leak a confirmed reservation.
		if rerr := orders.ReleaseClaimForBin(bin.ID, order.ID); rerr != nil {
			log.Printf("www: release claim for bin %d after dispatch failure: %v", bin.ID, rerr)
		}
		// The claim is rolled back and the order is failed, so nothing is
		// coming. Saying so beats handing back a 200 whose body has to be read
		// to find out the bin is not moving.
		h.jsonError(w, "order creation failed: the fleet did not accept it ("+err.Error()+")", http.StatusInternalServerError)
		return
	}

	h.readBackManualOrder(w, orderUUID)
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
