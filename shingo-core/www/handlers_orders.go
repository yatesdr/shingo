package www

import (
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/fleet"
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
		"Page":          "orders",
		"Orders":       orders,
		"FilterStatus": status,
	}
	h.render(w, r, "orders.html", data)
}

func (h *Handlers) handleOrderDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid order id", http.StatusBadRequest)
		return
	}

	svc := h.engine.OrderService()
	order, err := svc.GetOrder(id)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}

	history, err := svc.ListOrderHistory(id)
	if err != nil {
		log.Printf("order detail: list history for order %d: %v", id, err)
	}

	data := map[string]any{
		"Page":          "orders",
		"Order":         order,
		"History": history,
	}
	h.render(w, r, "orders.html", data)
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
		Order        *domain.Order             `json:"order"`
		History      []*domain.OrderHistory    `json:"history,omitempty"`
		Bin          *domain.Bin               `json:"bin,omitempty"`
		BinManifest  *domain.Manifest       `json:"bin_manifest,omitempty"`
		SourceNode   *domain.Node              `json:"source_node,omitempty"`
		DeliveryNode *domain.Node              `json:"delivery_node,omitempty"`
		Children     []*domain.Order           `json:"children,omitempty"`
		Parent       *domain.Order             `json:"parent,omitempty"`
		VendorDetail *fleet.VendorOrderDetail `json:"vendor_detail,omitempty"`
		Robot        *fleet.RobotStatus       `json:"robot,omitempty"`
	}

	result := enrichedOrder{Order: order}

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

func (h *Handlers) apiSpotOrderSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderType     string `json:"order_type"`
		SourceNode    string `json:"source_node"`
		DeliveryNode  string `json:"delivery_node"`
		StagingNode   string `json:"staging_node"`
		Priority      int    `json:"priority"`
		Description   string `json:"description"`
		PayloadCode string `json:"payload_code"`
		BinLabel      string `json:"bin_label"`
		Quantity      int    `json:"quantity"`
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

	orderUUID := fmt.Sprintf("spot-%s", uuid.New().String()[:8])
	src := protocol.Address{Role: protocol.RoleCore, Station: "core-spot"}
	dst := protocol.Address{Role: protocol.RoleCore}

	switch req.OrderType {
	case "staged":
		h.submitSpotComplexOrder(w, req.SourceNode, req.StagingNode, req.DeliveryNode,
			req.PayloadCode, req.Description, req.Priority, orderUUID, src, dst)
		return
	case "send_to":
		h.submitSpotSendTo(w, req.DeliveryNode, req.Description, req.Priority, orderUUID)
		return
	case "retrieve_specific":
		h.submitSpotRetrieveSpecific(w, req.BinLabel, req.DeliveryNode, req.Description, req.Priority, orderUUID)
		return
	case "swap":
		h.submitSpotSwap(w, req.DeliveryNode, req.PayloadCode, req.Description, req.Priority)
		return
	}

	// Transport types: move, retrieve, retrieve_empty, store
	actualType := req.OrderType
	retrieveEmpty := false
	if req.OrderType == "retrieve_empty" {
		actualType = "retrieve"
		retrieveEmpty = true
	}

	// Batch retrieve: create N independent orders
	if req.Quantity > 20 {
		req.Quantity = 20
	}
	if req.Quantity > 1 && (req.OrderType == "retrieve" || req.OrderType == "retrieve_empty") {
		var firstOrderID int64
		var firstStatus string
		for i := 1; i <= req.Quantity; i++ {
			batchUUID := fmt.Sprintf("%s-%d", orderUUID, i)
			orderReq := &protocol.OrderRequest{
				OrderUUID:     batchUUID,
				OrderType:     protocol.OrderType(actualType),
				PayloadCode: req.PayloadCode,
				PayloadDesc:   req.Description,
				Quantity:      1,
				SourceNode:    req.SourceNode,
				DeliveryNode:  req.DeliveryNode,
				Priority:      req.Priority,
				RetrieveEmpty: retrieveEmpty,
			}
			env, err := protocol.NewEnvelope(protocol.TypeOrderRequest, src, dst, orderReq)
			if err != nil {
				log.Printf("spot batch order %d/%d envelope error: %v", i, req.Quantity, err)
				continue
			}
			h.engine.Dispatcher().HandleOrderRequest(env, orderReq)
			if i == 1 {
				if o, err := h.engine.OrderService().GetOrderByUUID(batchUUID); err == nil {
					firstOrderID = o.ID
					firstStatus = string(o.Status)
				}
			}
		}
		h.jsonOK(w, map[string]any{
			"order_id": firstOrderID,
			"status":   firstStatus,
			"count":    req.Quantity,
		})
		return
	}

	orderReq := &protocol.OrderRequest{
		OrderUUID:     orderUUID,
		OrderType:     protocol.OrderType(actualType),
		PayloadCode: req.PayloadCode,
		PayloadDesc:   req.Description,
		Quantity:      1,
		SourceNode:    req.SourceNode,
		DeliveryNode:  req.DeliveryNode,
		Priority:      req.Priority,
		RetrieveEmpty: retrieveEmpty,
	}

	env, err := protocol.NewEnvelope(protocol.TypeOrderRequest, src, dst, orderReq)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.engine.Dispatcher().HandleOrderRequest(env, orderReq)
	h.readBackSpotOrder(w, orderUUID)
}

func (h *Handlers) submitSpotSendTo(w http.ResponseWriter, destination, desc string, priority int, orderUUID string) {
	if destination == "" {
		h.jsonError(w, "destination node is required", http.StatusBadRequest)
		return
	}

	destNode, err := h.engine.NodeService().GetByName(destination)
	if err != nil {
		h.jsonError(w, "destination node not found: "+destination, http.StatusBadRequest)
		return
	}

	order := &domain.Order{
		EdgeUUID:     orderUUID,
		StationID:    "core-spot",
		OrderType:    "send_to",
		Status:       "pending",
		Quantity:     1,
		DeliveryNode: destNode.Name,
		Priority:     priority,
		PayloadDesc:  desc,
	}
	orders := h.engine.OrderService()
	if err := orders.Create(order); err != nil {
		h.jsonError(w, "failed to create order: "+err.Error(), http.StatusInternalServerError)
		return
	}

	vendorOrderID := fmt.Sprintf("sg-%d-%s", order.ID, uuid.New().String()[:8])
	req := fleet.StagedOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: orderUUID,
		Blocks: []fleet.OrderBlock{
			{BlockID: "B1", Location: destNode.Name},
		},
		Priority: priority,
	}

	if _, err := h.engine.Fleet().CreateStagedOrder(req); err != nil {
		orders.UpdateStatus(order.ID, "failed", "fleet error: "+err.Error())
		h.readBackSpotOrder(w, orderUUID)
		return
	}

	orders.UpdateVendor(order.ID, vendorOrderID, "CREATED", "")
	orders.UpdateStatus(order.ID, "dispatched", fmt.Sprintf("send-to %s via %s (incomplete)", destNode.Name, vendorOrderID))
	h.readBackSpotOrder(w, orderUUID)
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
		OrderUUID:     orderUUID,
		PayloadCode: payloadCode,
		PayloadDesc:   desc,
		Quantity:      1,
		Priority:      priority,
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
	h.readBackSpotOrder(w, orderUUID)
}

func (h *Handlers) readBackSpotOrder(w http.ResponseWriter, orderUUID string) {
	order, err := h.engine.OrderService().GetOrderByUUID(orderUUID)
	if err != nil {
		h.jsonError(w, "order submitted but could not read back: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{
		"order_id":     order.ID,
		"status":       order.Status,
		"error_detail": order.ErrorDetail,
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

	order := &domain.Order{
		EdgeUUID:     orderUUID,
		StationID:    "core-spot",
		OrderType:    "move",
		Status:       "pending",
		Quantity:     1,
		SourceNode:   sourceNode.Name,
		DeliveryNode: destNode.Name,
		Priority:     priority,
		PayloadDesc:  desc,
		BinID:        &bin.ID,
	}
	orders := h.engine.OrderService()
	if err := orders.Create(order); err != nil {
		h.jsonError(w, "failed to create order: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := orders.ClaimBin(bin.ID, order.ID); err != nil {
		h.jsonError(w, "failed to claim bin: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if _, err := h.engine.Dispatcher().DispatchDirect(order, sourceNode, destNode); err != nil {
		orders.UnclaimBin(bin.ID)
		h.readBackSpotOrder(w, orderUUID)
		return
	}

	h.readBackSpotOrder(w, orderUUID)
}

func (h *Handlers) submitSpotSwap(w http.ResponseWriter, targetNode, payloadCode, desc string, priority int) {
	if targetNode == "" {
		h.jsonError(w, "target node is required", http.StatusBadRequest)
		return
	}
	if payloadCode == "" {
		h.jsonError(w, "payload is required", http.StatusBadRequest)
		return
	}

	if _, err := h.engine.NodeService().GetByName(targetNode); err != nil {
		h.jsonError(w, "target node not found: "+targetNode, http.StatusBadRequest)
		return
	}

	baseUUID := uuid.New().String()[:8]
	storeUUID := fmt.Sprintf("spot-swap-s-%s", baseUUID)
	retrieveUUID := fmt.Sprintf("spot-swap-r-%s", baseUUID)

	src := protocol.Address{Role: protocol.RoleCore, Station: "core-spot"}
	dst := protocol.Address{Role: protocol.RoleCore}

	// Store order: pickup from target node
	storeReq := &protocol.OrderRequest{
		OrderUUID: storeUUID,
		OrderType: "store",
		Quantity:  1,
		SourceNode: targetNode,
		Priority:   priority,
		PayloadDesc: desc,
	}
	storeEnv, err := protocol.NewEnvelope(protocol.TypeOrderRequest, src, dst, storeReq)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.engine.Dispatcher().HandleOrderRequest(storeEnv, storeReq)

	// Retrieve order: deliver to target node with payload
	retrieveReq := &protocol.OrderRequest{
		OrderUUID:     retrieveUUID,
		OrderType:     "retrieve",
		PayloadCode: payloadCode,
		Quantity:      1,
		DeliveryNode:  targetNode,
		Priority:      priority,
		PayloadDesc:   desc,
	}
	retrieveEnv, err := protocol.NewEnvelope(protocol.TypeOrderRequest, src, dst, retrieveReq)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.engine.Dispatcher().HandleOrderRequest(retrieveEnv, retrieveReq)

	// Read back both orders
	svc := h.engine.OrderService()
	resp := map[string]any{}
	if o, err := svc.GetOrderByUUID(storeUUID); err == nil {
		resp["store_order_id"] = o.ID
		resp["store_status"] = o.Status
	}
	if o, err := svc.GetOrderByUUID(retrieveUUID); err == nil {
		resp["retrieve_order_id"] = o.ID
		resp["retrieve_status"] = o.Status
	}
	h.jsonOK(w, resp)
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
