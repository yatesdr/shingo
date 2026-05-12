// handlers_test_orders_direct.go — synthetic order endpoints that
// bypass Kafka and call the dispatcher in-process. Same operator-facing
// /test-orders page, but the "direct" tab — useful for verifying
// dispatcher behaviour without the Kafka round-trip.

package www

import (
	"net/http"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/engine"
)

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

	result, err := h.orchestration.CreateDirectOrder(engine.DirectOrderRequest{
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

// apiDirectOrderReceipt confirms delivery for a direct order (bypasses Kafka).
func (h *Handlers) apiDirectOrderReceipt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderUUID   string `json:"order_uuid"`
		ReceiptType string `json:"receipt_type"`
		FinalCount  int64  `json:"final_count"`
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

	order, err := h.engine.OrderService().GetOrderByUUID(req.OrderUUID)
	if err != nil {
		h.jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	ok, err := h.engine.Dispatcher().Lifecycle().ConfirmReceipt(order, order.StationID, req.ReceiptType, req.FinalCount)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		h.jsonError(w, "order already completed", http.StatusBadRequest)
		return
	}

	h.jsonOK(w, map[string]string{"status": "confirmed", "order_uuid": req.OrderUUID})
}

func (h *Handlers) apiDirectOrdersList(w http.ResponseWriter, r *http.Request) {
	orders, err := h.engine.OrderService().ListOrdersByStation("core-direct", 50)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, orders)
}

// apiDirectComplexOrderSubmit creates complex orders directly through the dispatcher (no Kafka).
func (h *Handlers) apiDirectComplexOrderSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CycleMode    protocol.SwapMode `json:"cycle_mode"`
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
		req.CycleMode = protocol.SwapModeSequential
	}

	src := protocol.Address{Role: protocol.RoleEdge, Station: "core-direct"}
	dst := protocol.Address{Role: protocol.RoleCore, Station: h.engine.AppConfig().Messaging.StationID}

	var results []map[string]any

	switch req.CycleMode {
	case protocol.SwapModeSequential:
		steps := []protocol.ComplexOrderStep{
			{Action: "dropoff", Node: req.Location},
			{Action: "wait"},
			{Action: "pickup", Node: req.Location},
			dropoffStep(req.OutboundDestination),
		}
		uid := h.dispatchComplex(src, dst, req.PayloadCode, steps, req.Priority)
		results = append(results, map[string]any{"role": string(protocol.SwapModeSequential), "order_uuid": uid})

	case protocol.SwapModeTwoRobot:
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

	case protocol.SwapModeSingleRobot:
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
		results = append(results, map[string]any{"role": string(protocol.SwapModeSingleRobot), "order_uuid": uid})

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
