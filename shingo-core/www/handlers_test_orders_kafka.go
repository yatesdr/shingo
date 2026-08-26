// handlers_test_orders_kafka.go — synthetic order endpoints that
// exercise the system via the real Kafka topic (envelope encode +
// MsgClient publish). Used by the operator-facing /test-orders page
// to drive simple orders, cancels, receipts, and complex orders
// through the same wire path real Edge traffic uses.
//
// THESE DELIBERATELY CARRY NO ORIGIN and therefore land `orphan`, unlike the
// /orders spot page (handlers_orders.go) whose six create sites all stamp
// `no_demand`. The difference is what each page IS: a spot order is an operator
// doing something, while these publish from station "core-test" as an EDGE, and
// an Edge order with no origin is precisely what orphan means. Stamping them
// no_demand would leave nothing able to reproduce the skew case on a dev box.

package www

import (
	"fmt"
	"log"
	"net/http"

	"github.com/google/uuid"

	"shingo/protocol"
)

// --- Kafka Test Orders ---

func (h *Handlers) apiTestOrderSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderType    string `json:"order_type"`
		SourceNode   string `json:"source_node"`
		DeliveryNode string `json:"delivery_node"`
		PayloadCode  string `json:"payload_code"`
		Quantity     int64  `json:"quantity"`
		Priority     int    `json:"priority"`
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
	orderUUID := uuid.New().String()

	src := protocol.Address{Role: protocol.RoleEdge, Station: "core-test"}
	dst := protocol.Address{Role: protocol.RoleCore, Station: cfg.Messaging.StationID}

	orderReq := &protocol.OrderRequest{
		OrderUUID:    orderUUID,
		OrderType:    protocol.OrderType(req.OrderType),
		PayloadCode:  req.PayloadCode,
		Quantity:     req.Quantity,
		DeliveryNode: req.DeliveryNode,
		SourceNode:   req.SourceNode,
		Priority:     req.Priority,
		PayloadDesc:  "test order from shingo core",
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

// publishComplex builds a ComplexOrderRequest and publishes it over Kafka.
// publishComplex is the Kafka twin of dispatchComplex and had the same two
// omissions. Arrival order is not guaranteed on this route, which is exactly
// why the forward pointer is written in the INSERT and the back-link is keyed
// on the uuid rather than a resolved id.
func (h *Handlers) publishComplex(src, dst protocol.Address, payloadCode string, steps []protocol.ComplexOrderStep, priority int, processNode, siblingUUID string) (string, error) {
	orderUUID := uuid.New().String()

	complexReq := &protocol.ComplexOrderRequest{
		OrderUUID:        orderUUID,
		PayloadCode:      payloadCode,
		PayloadDesc:      "test complex order via kafka",
		Quantity:         1,
		Priority:         priority,
		ProcessNode:      processNode,
		SiblingOrderUUID: siblingUUID,
		Steps:            steps,
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

	cfg := h.engine.AppConfig()
	src := protocol.Address{Role: protocol.RoleEdge, Station: "core-test"}
	dst := protocol.Address{Role: protocol.RoleCore, Station: cfg.Messaging.StationID}

	var results []map[string]any

	switch req.CycleMode {
	case protocol.SwapModeSequential:
		uid, err := h.publishComplex(src, dst, req.PayloadCode, buildSwapSequentialSteps(req), req.Priority, "", "")
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, map[string]any{"role": string(protocol.SwapModeSequential), "order_uuid": uid})

	case protocol.SwapModeTwoRobot:
		if req.InboundStaging == "" {
			h.jsonError(w, "inbound_staging is required for two robot", http.StatusBadRequest)
			return
		}
		uid1, err := h.publishComplex(src, dst, req.PayloadCode, buildSwapResupplySteps(req), req.Priority, req.Location, "")
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, map[string]any{"role": "resupply", "order_uuid": uid1})

		uid2, err := h.publishComplex(src, dst, req.PayloadCode, buildSwapRemovalSteps(req), req.Priority, req.Location, uid1)
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, map[string]any{"role": "removal", "order_uuid": uid2})

	case protocol.SwapModeSingleRobot:
		if req.InboundStaging == "" || req.OutboundStaging == "" {
			h.jsonError(w, "inbound_staging and outbound_staging required for single robot", http.StatusBadRequest)
			return
		}
		uid, err := h.publishComplex(src, dst, req.PayloadCode, buildSwapSingleRobotSteps(req), req.Priority, "", "")
		if err != nil {
			h.jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		results = append(results, map[string]any{"role": string(protocol.SwapModeSingleRobot), "order_uuid": uid})

	default:
		h.jsonError(w, "invalid cycle_mode", http.StatusBadRequest)
		return
	}

	h.jsonOK(w, map[string]any{"cycle_mode": req.CycleMode, "orders": results})
}
