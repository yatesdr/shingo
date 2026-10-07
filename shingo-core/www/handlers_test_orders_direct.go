// handlers_test_orders_direct.go — synthetic order endpoints that
// bypass Kafka and call the dispatcher in-process. Same operator-facing
// /test-orders page, but the "direct" tab — useful for verifying
// dispatcher behaviour without the Kafka round-trip.

package www

import (
	"net/http"
	"strings"

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

	// The engineers' half of the one bin-move door: name a node, take whatever
	// is free there. The refusal statuses are no longer decided here — the
	// engine classifies each one and this maps the kind to a number, so both
	// screens answer the same way to the same refusal.
	result, err := h.orchestration.CreateBinMove(engine.BinMoveRequest{
		Selection:    engine.BinSelectionAuto,
		SourceNodeID: req.FromNodeID,
		DestNodeID:   req.ToNodeID,
		StationID:    "core-direct",
		Priority:     req.Priority,
		Desc:         "direct test order from shingo core",
	})
	if err != nil {
		h.jsonError(w, err.Error(), binMoveStatus(err))
		return
	}

	// vendor_order_id is empty on a lane park — there is no fleet order yet — so
	// the wait is reported rather than left to be inferred from a blank field.
	h.jsonOK(w, map[string]any{
		"order_id":        result.OrderID,
		"vendor_order_id": result.VendorOrderID,
		"from":            result.FromNode,
		"to":              result.ToNode,
		"bin":             result.BinLabel,
		"queued":          result.Queued,
		"queue_reason":    result.QueueReason,
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
	var req complexSwapRequest
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
	// refused holds one line per leg Core did not create, from the order.error
	// the dispatcher sent (or, with none, from the missing row).
	var refused []string
	note := func(role, uid, problem string) {
		results = append(results, map[string]any{"role": role, "order_uuid": uid})
		if problem != "" {
			refused = append(refused, role+": "+problem)
		}
	}

	switch req.CycleMode {
	case protocol.SwapModeSequential:
		uid, problem := h.dispatchComplex(src, dst, req.PayloadCode, buildSwapSequentialSteps(req), req.Priority, "", "", "")
		note(string(protocol.SwapModeSequential), uid, problem)

	case protocol.SwapModeTwoRobot:
		if req.InboundStaging == "" {
			h.jsonError(w, "inbound_staging is required for two robot", http.StatusBadRequest)
			return
		}
		// BOTH UUIDS BEFORE EITHER ORDER, as the Edge's pair doors do: each leg
		// names the other in its own request. With only the removal carrying a
		// pointer, the supply reached intake as a solo order and went to the fleet
		// before its removal existed. The supply is still sent first.
		supplyUUID, removalUUID := uuid.New().String(), uuid.New().String()
		uid1, problem1 := h.dispatchComplex(src, dst, req.PayloadCode, buildSwapResupplySteps(req), req.Priority, req.Location, removalUUID, supplyUUID)
		note("resupply", uid1, problem1)

		// Removal
		uid2, problem2 := h.dispatchComplex(src, dst, req.PayloadCode, buildSwapRemovalSteps(req), req.Priority, req.Location, supplyUUID, removalUUID)
		note("removal", uid2, problem2)

	case protocol.SwapModeSingleRobot:
		if req.InboundStaging == "" || req.OutboundStaging == "" {
			h.jsonError(w, "inbound_staging and outbound_staging required for single robot", http.StatusBadRequest)
			return
		}
		uid, problem := h.dispatchComplex(src, dst, req.PayloadCode, buildSwapSingleRobotSteps(req), req.Priority, "", "", "")
		note(string(protocol.SwapModeSingleRobot), uid, problem)

	default:
		h.jsonError(w, "invalid cycle_mode", http.StatusBadRequest)
		return
	}

	if len(refused) > 0 {
		h.jsonError(w, "not created: "+strings.Join(refused, "; "), http.StatusConflict)
		return
	}
	h.jsonOK(w, map[string]any{"cycle_mode": req.CycleMode, "orders": results})
}

// dispatchComplex builds a ComplexOrderRequest and calls the dispatcher directly.
//
// processNode and siblingUUID are what make a pair of legs a swap. Both were
// omitted here, so this page produced two unrelated orders that happened to be
// about the same node — see the two-robot branch above for what that costs.
//
// problem is "" when Core created the order, else why it did not: the detail
// of the order.error the dispatcher sent, or, with none, "no order was created".
func (h *Handlers) dispatchComplex(src, dst protocol.Address, payloadCode string, steps []protocol.ComplexOrderStep, priority int, processNode, siblingUUID, orderUUID string) (uid, problem string) {
	if orderUUID == "" {
		orderUUID = uuid.New().String()
	}

	complexReq := &protocol.ComplexOrderRequest{
		OrderUUID:        orderUUID,
		PayloadCode:      payloadCode,
		PayloadDesc:      "test complex order",
		Quantity:         1,
		Priority:         priority,
		ProcessNode:      processNode,
		SiblingOrderUUID: siblingUUID,
		Steps:            steps,
	}

	env, _ := protocol.NewEnvelope(protocol.TypeComplexOrderRequest, src, dst, complexReq)
	d := h.engine.Dispatcher()
	refusals := d.RefusalsFor(env, func() { d.HandleComplexOrderRequest(env, complexReq) })
	// The row decides. An order that was created and then failed is reported
	// on the orders table like any other; only a missing row is a refusal here.
	if _, err := h.engine.OrderService().GetOrderByUUID(orderUUID); err == nil {
		return orderUUID, ""
	}
	if msg := refusalDetail(refusals, orderUUID); msg != "" {
		return orderUUID, msg
	}
	return orderUUID, "no order was created"
}

// refusalDetail is the detail of the first order.error sent for orderUUID, or "".
func refusalDetail(refusals []protocol.OrderError, orderUUID string) string {
	for _, e := range refusals {
		if e.OrderUUID == orderUUID {
			if e.Detail != "" {
				return e.Detail
			}
			return e.ErrorCode
		}
	}
	return ""
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
	d := h.engine.Dispatcher()
	refusals := d.RefusalsFor(env, func() { d.HandleOrderRelease(env, releaseReq) })
	if msg := refusalDetail(refusals, req.OrderUUID); msg != "" {
		status := http.StatusConflict
		if _, err := h.engine.OrderService().GetOrderByUUID(req.OrderUUID); err != nil {
			status, msg = http.StatusNotFound, "order not found"
		}
		h.jsonError(w, msg, status)
		return
	}

	h.jsonOK(w, map[string]string{"status": "released", "order_uuid": req.OrderUUID})
}
