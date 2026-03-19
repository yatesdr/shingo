package www

import (
	"net/http"

	"shingo/protocol"
	"shingocore/engine"

	"github.com/google/uuid"
)

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
		StagingNode  string `json:"staging_node"`
		StagingNode2 string `json:"staging_node_2"`
		FullPickup   string `json:"full_pickup"`
		OutgoingDest string `json:"outgoing_dest"`
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
			dropoffStep(req.OutgoingDest),
		}
		uid := h.dispatchComplex(src, dst, req.PayloadCode, steps, req.Priority)
		results = append(results, map[string]any{"role": "sequential", "order_uuid": uid})

	case "two_robot":
		if req.StagingNode == "" {
			h.jsonError(w, "staging_node is required for two robot", http.StatusBadRequest)
			return
		}
		// Resupply
		resupplySteps := []protocol.ComplexOrderStep{
			pickupStepDirect(req.FullPickup),
			{Action: "dropoff", Node: req.StagingNode},
			{Action: "wait"},
			{Action: "pickup", Node: req.StagingNode},
			{Action: "dropoff", Node: req.Location},
		}
		uid1 := h.dispatchComplex(src, dst, req.PayloadCode, resupplySteps, req.Priority)
		results = append(results, map[string]any{"role": "resupply", "order_uuid": uid1})

		// Removal
		removalSteps := []protocol.ComplexOrderStep{
			{Action: "dropoff", Node: req.Location},
			{Action: "wait"},
			{Action: "pickup", Node: req.Location},
			dropoffStep(req.OutgoingDest),
		}
		uid2 := h.dispatchComplex(src, dst, req.PayloadCode, removalSteps, req.Priority)
		results = append(results, map[string]any{"role": "removal", "order_uuid": uid2})

	case "single_robot":
		if req.StagingNode == "" || req.StagingNode2 == "" {
			h.jsonError(w, "staging_node and staging_node_2 required for single robot", http.StatusBadRequest)
			return
		}
		steps := []protocol.ComplexOrderStep{
			pickupStepDirect(req.FullPickup),
			{Action: "dropoff", Node: req.StagingNode},
			{Action: "dropoff", Node: req.Location},
			{Action: "wait"},
			{Action: "pickup", Node: req.Location},
			{Action: "dropoff", Node: req.StagingNode2},
			{Action: "pickup", Node: req.StagingNode},
			{Action: "dropoff", Node: req.Location},
			{Action: "pickup", Node: req.StagingNode2},
			dropoffStep(req.OutgoingDest),
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
