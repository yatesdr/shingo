package www

import (
	"fmt"
	"log"
	"net/http"
	"strconv"

	"shingo/protocol"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/store"

	"github.com/google/uuid"
)

// --- Test Orders Page ---

func (h *Handlers) handleTestOrders(w http.ResponseWriter, r *http.Request) {
	nodes, _ := h.engine.DB().ListNodes()
	payloads, _ := h.engine.DB().ListPayloads()
	data := map[string]any{
		"Page":     "test-orders",
		"Nodes":    nodes,
		"Payloads": payloads,
	}
	h.render(w, r, "test-orders.html", data)
}

func (h *Handlers) apiTestOrdersList(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.DB().ListOrdersByStation("core-test", 50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}

func (h *Handlers) apiTestOrderDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	order, err := h.engine.DB().GetOrder(id)
	if err != nil {
		h.jsonError(w, "order not found", http.StatusNotFound)
		return
	}
	history, _ := h.engine.DB().ListOrderHistory(id)
	h.jsonOK(w, map[string]any{"order": order, "history": history})
}

func (h *Handlers) apiTestRobots(w http.ResponseWriter, r *http.Request) {
	rl, ok := h.engine.Fleet().(fleet.RobotLister)
	if !ok {
		h.jsonError(w, "fleet backend does not support robot listing", http.StatusNotImplemented)
		return
	}
	robots, err := rl.GetRobotsStatus()
	if err != nil {
		h.jsonError(w, "fleet error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, robots)
}

func (h *Handlers) apiTestScenePoints(w http.ResponseWriter, r *http.Request) {
	points, err := h.engine.DB().ListScenePoints()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, points)
}

// --- Kafka Test Orders ---

func (h *Handlers) apiTestOrderSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderType       string  `json:"order_type"`
		SourceNode      string  `json:"source_node"`
		DeliveryNode    string  `json:"delivery_node"`
		PayloadCode string  `json:"payload_code"`
		Quantity        int64   `json:"quantity"`
		Priority        int     `json:"priority"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.OrderType == "" {
		h.jsonError(w, "order_type is required", http.StatusBadRequest)
		return
	}
	if req.PayloadCode == "" {
		h.jsonError(w, "payload_code is required", http.StatusBadRequest)
		return
	}
	if req.Quantity <= 0 {
		req.Quantity = 1
	}

	cfg := h.engine.AppConfig()
	orderUUID := "test-" + uuid.New().String()[:8]

	src := protocol.Address{Role: protocol.RoleEdge, Station: "core-test"}
	dst := protocol.Address{Role: protocol.RoleCore, Station: cfg.Messaging.StationID}

	orderReq := &protocol.OrderRequest{
		OrderUUID:       orderUUID,
		OrderType:       req.OrderType,
		PayloadCode: req.PayloadCode,
		Quantity:        req.Quantity,
		DeliveryNode:    req.DeliveryNode,
		SourceNode:      req.SourceNode,
		Priority:        req.Priority,
		PayloadDesc:     "test order from shingo core",
	}

	env, err := protocol.NewEnvelope(protocol.TypeOrderRequest, src, dst, orderReq)
	if err != nil {
		h.jsonError(w, "build envelope: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := env.Encode()
	if err != nil {
		h.jsonError(w, "encode envelope: "+err.Error(), http.StatusInternalServerError)
		return
	}

	topic := cfg.Messaging.OrdersTopic
	log.Printf("test-orders: publishing %s to %s: %s", env.Type, topic, string(data))

	if err := h.engine.MsgClient().Publish(topic, data); err != nil {
		h.jsonError(w, "publish failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{
		"order_uuid":  orderUUID,
		"envelope_id": env.ID,
	})
}

func (h *Handlers) apiTestOrderCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderUUID string `json:"order_uuid"`
		Reason    string `json:"reason"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.OrderUUID == "" {
		h.jsonError(w, "order_uuid is required", http.StatusBadRequest)
		return
	}
	if req.Reason == "" {
		req.Reason = "cancelled via test page"
	}

	cfg := h.engine.AppConfig()
	src := protocol.Address{Role: protocol.RoleEdge, Station: "core-test"}
	dst := protocol.Address{Role: protocol.RoleCore, Station: cfg.Messaging.StationID}

	cancelReq := &protocol.OrderCancel{
		OrderUUID: req.OrderUUID,
		Reason:    req.Reason,
	}

	env, err := protocol.NewEnvelope(protocol.TypeOrderCancel, src, dst, cancelReq)
	if err != nil {
		h.jsonError(w, "build envelope: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := env.Encode()
	if err != nil {
		h.jsonError(w, "encode envelope: "+err.Error(), http.StatusInternalServerError)
		return
	}

	topic := cfg.Messaging.OrdersTopic
	log.Printf("test-orders: publishing %s to %s: %s", env.Type, topic, string(data))

	if err := h.engine.MsgClient().Publish(topic, data); err != nil {
		h.jsonError(w, "publish failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]string{"status": "cancel sent", "order_uuid": req.OrderUUID})
}

func (h *Handlers) apiTestOrderReceipt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderUUID   string  `json:"order_uuid"`
		ReceiptType string  `json:"receipt_type"`
		FinalCount  int64   `json:"final_count"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.OrderUUID == "" {
		h.jsonError(w, "order_uuid is required", http.StatusBadRequest)
		return
	}
	if req.ReceiptType == "" {
		req.ReceiptType = "full"
	}

	cfg := h.engine.AppConfig()
	src := protocol.Address{Role: protocol.RoleEdge, Station: "core-test"}
	dst := protocol.Address{Role: protocol.RoleCore, Station: cfg.Messaging.StationID}

	receiptReq := &protocol.OrderReceipt{
		OrderUUID:   req.OrderUUID,
		ReceiptType: req.ReceiptType,
		FinalCount:  req.FinalCount,
	}

	env, err := protocol.NewEnvelope(protocol.TypeOrderReceipt, src, dst, receiptReq)
	if err != nil {
		h.jsonError(w, "build envelope: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := env.Encode()
	if err != nil {
		h.jsonError(w, "encode envelope: "+err.Error(), http.StatusInternalServerError)
		return
	}

	topic := cfg.Messaging.OrdersTopic
	log.Printf("test-orders: publishing %s to %s: %s", env.Type, topic, string(data))

	if err := h.engine.MsgClient().Publish(topic, data); err != nil {
		h.jsonError(w, "publish failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]string{"status": "receipt sent", "order_uuid": req.OrderUUID})
}

// --- Direct Test Orders ---

func (h *Handlers) apiDirectOrderSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FromNodeID int64 `json:"from_node_id"`
		ToNodeID   int64 `json:"to_node_id"`
		Priority   int   `json:"priority"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	result, err := h.engine.CreateDirectOrder(engine.DirectOrderRequest{
		FromNodeID: req.FromNodeID,
		ToNodeID:   req.ToNodeID,
		StationID:  "core-direct",
		Priority:   req.Priority,
		Desc:       "direct test order from shingo core",
	})
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.jsonOK(w, map[string]any{
		"order_id":        result.OrderID,
		"vendor_order_id": result.VendorOrderID,
		"from":            result.FromNode,
		"to":              result.ToNode,
	})
}

func (h *Handlers) apiDirectOrdersList(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.DB().ListOrdersByStation("core-direct", 50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}

// apiDirectComplexOrderSubmit creates complex orders directly through the dispatcher (no Kafka).
func (h *Handlers) apiDirectComplexOrderSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CycleMode    string `json:"cycle_mode"`
		Location     string `json:"location"`
		InboundStaging       string `json:"inbound_staging"`
		OutboundStaging      string `json:"outbound_staging"`
		InboundSource        string `json:"inbound_source"`
		OutboundDestination  string `json:"outbound_destination"`
		PayloadCode  string `json:"payload_code"`
		Priority     int    `json:"priority"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.Location == "" {
		h.jsonError(w, "location is required", http.StatusBadRequest)
		return
	}
	if req.CycleMode == "" {
		req.CycleMode = "sequential"
	}

	src := protocol.Address{Role: protocol.RoleEdge, Station: "core-direct"}
	dst := protocol.Address{Role: protocol.RoleCore, Station: h.engine.AppConfig().Messaging.StationID}

	var results []map[string]any

	switch req.CycleMode {
	case "sequential":
		steps := []protocol.ComplexOrderStep{
			{Action: "dropoff", Node: req.Location},
			{Action: "wait"},
			{Action: "pickup", Node: req.Location},
			dropoffStep(req.OutboundDestination),
		}
		uid := h.dispatchComplex(src, dst, req.PayloadCode, steps, req.Priority)
		results = append(results, map[string]any{"role": "sequential", "order_uuid": uid})

	case "two_robot":
		if req.InboundStaging == "" {
			h.jsonError(w, "inbound_staging is required for two robot", http.StatusBadRequest)
			return
		}
		// Resupply
		resupplySteps := []protocol.ComplexOrderStep{
			pickupStepDirect(req.InboundSource),
			{Action: "dropoff", Node: req.InboundStaging},
			{Action: "wait"},
			{Action: "pickup", Node: req.InboundStaging},
			{Action: "dropoff", Node: req.Location},
		}
		uid1 := h.dispatchComplex(src, dst, req.PayloadCode, resupplySteps, req.Priority)
		results = append(results, map[string]any{"role": "resupply", "order_uuid": uid1})

		// Removal
		removalSteps := []protocol.ComplexOrderStep{
			{Action: "dropoff", Node: req.Location},
			{Action: "wait"},
			{Action: "pickup", Node: req.Location},
			dropoffStep(req.OutboundDestination),
		}
		uid2 := h.dispatchComplex(src, dst, req.PayloadCode, removalSteps, req.Priority)
		results = append(results, map[string]any{"role": "removal", "order_uuid": uid2})

	case "single_robot":
		if req.InboundStaging == "" || req.OutboundStaging == "" {
			h.jsonError(w, "inbound_staging and outbound_staging required for single robot", http.StatusBadRequest)
			return
		}
		steps := []protocol.ComplexOrderStep{
			pickupStepDirect(req.InboundSource),
			{Action: "dropoff", Node: req.InboundStaging},
			{Action: "dropoff", Node: req.Location},
			{Action: "wait"},
			{Action: "pickup", Node: req.Location},
			{Action: "dropoff", Node: req.OutboundStaging},
			{Action: "pickup", Node: req.InboundStaging},
			{Action: "dropoff", Node: req.Location},
			{Action: "pickup", Node: req.OutboundStaging},
			dropoffStep(req.OutboundDestination),
		}
		uid := h.dispatchComplex(src, dst, req.PayloadCode, steps, req.Priority)
		results = append(results, map[string]any{"role": "single_robot", "order_uuid": uid})

	default:
		h.jsonError(w, "invalid cycle_mode", http.StatusBadRequest)
		return
	}

	h.jsonOK(w, map[string]any{"cycle_mode": req.CycleMode, "orders": results})
}

// dispatchComplex builds a ComplexOrderRequest and calls the dispatcher directly.
func (h *Handlers) dispatchComplex(src, dst protocol.Address, payloadCode string, steps []protocol.ComplexOrderStep, priority int) string {
	orderUUID := "test-" + uuid.New().String()[:8]

	complexReq := &protocol.ComplexOrderRequest{
		OrderUUID:   orderUUID,
		PayloadCode: payloadCode,
		PayloadDesc: "test complex order",
		Quantity:    1,
		Priority:    priority,
		Steps:       steps,
	}

	env, _ := protocol.NewEnvelope(protocol.TypeComplexOrderRequest, src, dst, complexReq)
	h.engine.Dispatcher().HandleComplexOrderRequest(env, complexReq)
	return orderUUID
}

// publishComplex builds a ComplexOrderRequest and publishes it over Kafka.
func (h *Handlers) publishComplex(src, dst protocol.Address, payloadCode string, steps []protocol.ComplexOrderStep, priority int) (string, error) {
	orderUUID := "test-" + uuid.New().String()[:8]

	complexReq := &protocol.ComplexOrderRequest{
		OrderUUID:   orderUUID,
		PayloadCode: payloadCode,
		PayloadDesc: "test complex order via kafka",
		Quantity:    1,
		Priority:    priority,
		Steps:       steps,
	}

	env, err := protocol.NewEnvelope(protocol.TypeComplexOrderRequest, src, dst, complexReq)
	if err != nil {
		return "", fmt.Errorf("build envelope: %w", err)
	}

	data, err := env.Encode()
	if err != nil {
		return "", fmt.Errorf("encode envelope: %w", err)
	}

	topic := h.engine.AppConfig().Messaging.OrdersTopic
	log.Printf("test-orders: publishing %s to %s: %s", env.Type, topic, string(data))

	if err := h.engine.MsgClient().Publish(topic, data); err != nil {
		return "", fmt.Errorf("publish failed: %w", err)
	}

	return orderUUID, nil
}

// apiKafkaComplexOrderSubmit builds complex order steps and publishes via Kafka.
func (h *Handlers) apiKafkaComplexOrderSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CycleMode    string `json:"cycle_mode"`
		Location     string `json:"location"`
		InboundStaging       string `json:"inbound_staging"`
		OutboundStaging      string `json:"outbound_staging"`
		InboundSource        string `json:"inbound_source"`
		OutboundDestination  string `json:"outbound_destination"`
		PayloadCode  string `json:"payload_code"`
		Priority     int    `json:"priority"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.Location == "" {
		h.jsonError(w, "location is required", http.StatusBadRequest)
		return
	}
	if req.CycleMode == "" {
		req.CycleMode = "sequential"
	}

	cfg := h.engine.AppConfig()
	src := protocol.Address{Role: protocol.RoleEdge, Station: "core-test"}
	dst := protocol.Address{Role: protocol.RoleCore, Station: cfg.Messaging.StationID}

	var results []map[string]any

	switch req.CycleMode {
	case "sequential":
		steps := []protocol.ComplexOrderStep{
			{Action: "dropoff", Node: req.Location},
			{Action: "wait"},
			{Action: "pickup", Node: req.Location},
			dropoffStep(req.OutboundDestination),
		}
		uid, err := h.publishComplex(src, dst, req.PayloadCode, steps, req.Priority)
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, map[string]any{"role": "sequential", "order_uuid": uid})

	case "two_robot":
		if req.InboundStaging == "" {
			h.jsonError(w, "inbound_staging is required for two robot", http.StatusBadRequest)
			return
		}
		resupplySteps := []protocol.ComplexOrderStep{
			pickupStepDirect(req.InboundSource),
			{Action: "dropoff", Node: req.InboundStaging},
			{Action: "wait"},
			{Action: "pickup", Node: req.InboundStaging},
			{Action: "dropoff", Node: req.Location},
		}
		uid1, err := h.publishComplex(src, dst, req.PayloadCode, resupplySteps, req.Priority)
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, map[string]any{"role": "resupply", "order_uuid": uid1})

		removalSteps := []protocol.ComplexOrderStep{
			{Action: "dropoff", Node: req.Location},
			{Action: "wait"},
			{Action: "pickup", Node: req.Location},
			dropoffStep(req.OutboundDestination),
		}
		uid2, err := h.publishComplex(src, dst, req.PayloadCode, removalSteps, req.Priority)
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, map[string]any{"role": "removal", "order_uuid": uid2})

	case "single_robot":
		if req.InboundStaging == "" || req.OutboundStaging == "" {
			h.jsonError(w, "inbound_staging and outbound_staging required for single robot", http.StatusBadRequest)
			return
		}
		steps := []protocol.ComplexOrderStep{
			pickupStepDirect(req.InboundSource),
			{Action: "dropoff", Node: req.InboundStaging},
			{Action: "dropoff", Node: req.Location},
			{Action: "wait"},
			{Action: "pickup", Node: req.Location},
			{Action: "dropoff", Node: req.OutboundStaging},
			{Action: "pickup", Node: req.InboundStaging},
			{Action: "dropoff", Node: req.Location},
			{Action: "pickup", Node: req.OutboundStaging},
			dropoffStep(req.OutboundDestination),
		}
		uid, err := h.publishComplex(src, dst, req.PayloadCode, steps, req.Priority)
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, map[string]any{"role": "single_robot", "order_uuid": uid})

	default:
		h.jsonError(w, "invalid cycle_mode", http.StatusBadRequest)
		return
	}

	h.jsonOK(w, map[string]any{"cycle_mode": req.CycleMode, "orders": results})
}

// apiDirectOrderRelease releases a staged order directly through the dispatcher.
func (h *Handlers) apiDirectOrderRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderUUID string `json:"order_uuid"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.OrderUUID == "" {
		h.jsonError(w, "order_uuid is required", http.StatusBadRequest)
		return
	}

	src := protocol.Address{Role: protocol.RoleEdge, Station: "core-direct"}
	dst := protocol.Address{Role: protocol.RoleCore, Station: h.engine.AppConfig().Messaging.StationID}

	releaseReq := &protocol.OrderRelease{
		OrderUUID: req.OrderUUID,
	}

	env, _ := protocol.NewEnvelope(protocol.TypeOrderRelease, src, dst, releaseReq)
	h.engine.Dispatcher().HandleOrderRelease(env, releaseReq)

	h.jsonOK(w, map[string]string{"status": "released", "order_uuid": req.OrderUUID})
}

func pickupStepDirect(node string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: "pickup", Node: node}
	}
	return protocol.ComplexOrderStep{Action: "pickup"}
}

func dropoffStep(node string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: "dropoff", Node: node}
	}
	return protocol.ComplexOrderStep{Action: "dropoff"}
}

// --- Fleet Test Commands ---

func (h *Handlers) apiTestCommandSubmit(w http.ResponseWriter, r *http.Request) {
	commander, ok := h.engine.Fleet().(fleet.VendorCommander)
	if !ok {
		h.jsonError(w, "fleet backend does not support vendor commands", http.StatusNotImplemented)
		return
	}

	var req struct {
		CommandType   string `json:"command_type"`
		RobotID       string `json:"robot_id"`
		Location      string `json:"location"`
		ConfigID      string `json:"config_id"`
		DispatchType  string `json:"dispatch_type"`
		MapName       string `json:"map_name"`
		OrderID       string `json:"order_id"`
		ContainerName string `json:"container_name"`
		GoodsID       string `json:"goods_id"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}
	if req.CommandType == "" {
		h.jsonError(w, "command_type is required", http.StatusBadRequest)
		return
	}

	// Validate required fields per command type
	switch req.CommandType {
	case "terminate":
		if req.OrderID == "" {
			h.jsonError(w, "order_id is required", http.StatusBadRequest)
			return
		}
	case "switch_map":
		if req.MapName == "" {
			h.jsonError(w, "map_name is required", http.StatusBadRequest)
			return
		}
		if req.RobotID == "" {
			h.jsonError(w, "robot_id is required", http.StatusBadRequest)
			return
		}
	case "bind_goods":
		if req.ContainerName == "" || req.GoodsID == "" {
			h.jsonError(w, "container_name and goods_id are required", http.StatusBadRequest)
			return
		}
	case "unbind_goods":
		if req.GoodsID == "" {
			h.jsonError(w, "goods_id is required", http.StatusBadRequest)
			return
		}
	case "unbind_container":
		if req.ContainerName == "" {
			h.jsonError(w, "container_name is required", http.StatusBadRequest)
			return
		}
	case "move":
		if req.Location == "" {
			h.jsonError(w, "location is required for move command", http.StatusBadRequest)
			return
		}
		// robot_id is optional for move — fleet auto-assigns
	case "jack", "unjack":
		if req.ConfigID == "" {
			h.jsonError(w, "config_id is required for jack/unjack commands", http.StatusBadRequest)
			return
		}
		fallthrough
	default:
		if req.RobotID == "" && req.CommandType != "terminate" {
			h.jsonError(w, "robot_id is required", http.StatusBadRequest)
			return
		}
	}

	result, err := commander.ExecuteVendorCommand(fleet.VendorCommand{
		Type:          req.CommandType,
		RobotID:       req.RobotID,
		Location:      req.Location,
		ConfigID:      req.ConfigID,
		DispatchType:  req.DispatchType,
		MapName:       req.MapName,
		OrderID:       req.OrderID,
		ContainerName: req.ContainerName,
		GoodsID:       req.GoodsID,
	})

	tc := &store.TestCommand{
		CommandType: req.CommandType,
		RobotID:     req.RobotID,
		Location:    req.Location,
		ConfigID:    req.ConfigID,
	}
	if result != nil {
		tc.VendorOrderID = result.VendorOrderID
		tc.VendorState = result.State
		tc.Detail = result.Detail
	}

	if dbErr := h.engine.DB().CreateTestCommand(tc); dbErr != nil {
		log.Printf("test-commands: db save error: %v", dbErr)
	}
	if result != nil && result.State == "COMPLETED" {
		h.engine.DB().CompleteTestCommand(tc.ID)
	}

	if err != nil {
		log.Printf("test-commands: %s failed: %v", req.CommandType, err)
		h.jsonError(w, "vendor command failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("test-commands: %s succeeded: robot=%s", req.CommandType, req.RobotID)
	h.jsonOK(w, map[string]any{
		"id":              tc.ID,
		"vendor_order_id": result.VendorOrderID,
		"status":          result.State,
	})
}

func (h *Handlers) apiTestCommandStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		h.jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}

	tc, err := h.engine.DB().GetTestCommand(id)
	if err != nil {
		h.jsonError(w, "command not found", http.StatusNotFound)
		return
	}

	var vendorDetail any
	if tc.CompletedAt == nil && tc.VendorOrderID != "" {
		if commander, ok := h.engine.Fleet().(fleet.VendorCommander); ok {
			detail, err := commander.GetVendorOrderDetail(tc.VendorOrderID)
			if err == nil {
				vendorDetail = detail.Raw
				if detail.State != tc.VendorState {
					h.engine.DB().UpdateTestCommandStatus(id, detail.State, "")
					tc.VendorState = detail.State
				}
				if detail.IsTerminal {
					h.engine.DB().CompleteTestCommand(id)
				}
			}
		}
	}

	h.jsonOK(w, map[string]any{
		"command":       tc,
		"vendor_detail": vendorDetail,
	})
}

func (h *Handlers) apiTestCommandsList(w http.ResponseWriter, r *http.Request) {
	cmds, err := h.engine.DB().ListTestCommands(50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, cmds)
}
