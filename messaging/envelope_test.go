package messaging

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDecodeEnvelope_OrderRequest(t *testing.T) {
	data := []byte(`{
		"msg_type": "order_request",
		"msg_id": "abc-123",
		"client_id": "line-1",
		"factory_id": "plant-alpha",
		"timestamp": "2026-02-17T12:00:00Z",
		"payload": {
			"order_uuid": "uuid-1",
			"order_type": "retrieve",
			"material_code": "PART-A",
			"quantity": 2.0,
			"delivery_node": "LINE1-IN",
			"pickup_node": "",
			"priority": 5,
			"retrieve_empty": false,
			"payload_desc": "Steel bracket"
		}
	}`)

	env, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.MsgType != "order_request" {
		t.Errorf("msg_type = %q, want %q", env.MsgType, "order_request")
	}
	if env.MsgID != "abc-123" {
		t.Errorf("msg_id = %q, want %q", env.MsgID, "abc-123")
	}
	if env.ClientID != "line-1" {
		t.Errorf("client_id = %q, want %q", env.ClientID, "line-1")
	}
	if env.FactoryID != "plant-alpha" {
		t.Errorf("factory_id = %q, want %q", env.FactoryID, "plant-alpha")
	}

	req, ok := env.Payload.(OrderRequest)
	if !ok {
		t.Fatalf("payload type = %T, want OrderRequest", env.Payload)
	}
	if req.OrderUUID != "uuid-1" {
		t.Errorf("order_uuid = %q, want %q", req.OrderUUID, "uuid-1")
	}
	if req.OrderType != "retrieve" {
		t.Errorf("order_type = %q, want %q", req.OrderType, "retrieve")
	}
	if req.MaterialCode != "PART-A" {
		t.Errorf("material_code = %q, want %q", req.MaterialCode, "PART-A")
	}
	if req.Quantity != 2.0 {
		t.Errorf("quantity = %f, want 2.0", req.Quantity)
	}
	if req.DeliveryNode != "LINE1-IN" {
		t.Errorf("delivery_node = %q, want %q", req.DeliveryNode, "LINE1-IN")
	}
	if req.Priority != 5 {
		t.Errorf("priority = %d, want 5", req.Priority)
	}
	if req.PayloadDesc != "Steel bracket" {
		t.Errorf("payload_desc = %q, want %q", req.PayloadDesc, "Steel bracket")
	}
}

func TestDecodeEnvelope_OrderCancel(t *testing.T) {
	data := []byte(`{
		"msg_type": "order_cancel",
		"msg_id": "msg-2",
		"client_id": "line-2",
		"factory_id": "plant-alpha",
		"timestamp": "2026-02-17T12:00:00Z",
		"payload": {"order_uuid": "uuid-2", "reason": "operator cancelled"}
	}`)

	env, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cancel, ok := env.Payload.(OrderCancel)
	if !ok {
		t.Fatalf("payload type = %T, want OrderCancel", env.Payload)
	}
	if cancel.OrderUUID != "uuid-2" {
		t.Errorf("order_uuid = %q, want %q", cancel.OrderUUID, "uuid-2")
	}
	if cancel.Reason != "operator cancelled" {
		t.Errorf("reason = %q, want %q", cancel.Reason, "operator cancelled")
	}
}

func TestDecodeEnvelope_DeliveryReceipt(t *testing.T) {
	data := []byte(`{
		"msg_type": "delivery_receipt",
		"msg_id": "msg-3",
		"client_id": "line-1",
		"factory_id": "plant-alpha",
		"timestamp": "2026-02-17T12:00:00Z",
		"payload": {"order_uuid": "uuid-3", "receipt_type": "confirmed", "final_count": 50.0}
	}`)

	env, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	receipt, ok := env.Payload.(DeliveryReceipt)
	if !ok {
		t.Fatalf("payload type = %T, want DeliveryReceipt", env.Payload)
	}
	if receipt.OrderUUID != "uuid-3" {
		t.Errorf("order_uuid = %q, want %q", receipt.OrderUUID, "uuid-3")
	}
	if receipt.ReceiptType != "confirmed" {
		t.Errorf("receipt_type = %q, want %q", receipt.ReceiptType, "confirmed")
	}
	if receipt.FinalCount != 50.0 {
		t.Errorf("final_count = %f, want 50.0", receipt.FinalCount)
	}
}

func TestDecodeEnvelope_RedirectRequest(t *testing.T) {
	data := []byte(`{
		"msg_type": "redirect_request",
		"msg_id": "msg-4",
		"client_id": "line-1",
		"factory_id": "plant-alpha",
		"timestamp": "2026-02-17T12:00:00Z",
		"payload": {"order_uuid": "uuid-4", "new_delivery_node": "LINE1-ALT"}
	}`)

	env, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	redirect, ok := env.Payload.(RedirectRequest)
	if !ok {
		t.Fatalf("payload type = %T, want RedirectRequest", env.Payload)
	}
	if redirect.OrderUUID != "uuid-4" {
		t.Errorf("order_uuid = %q, want %q", redirect.OrderUUID, "uuid-4")
	}
	if redirect.NewDeliveryNode != "LINE1-ALT" {
		t.Errorf("new_delivery_node = %q, want %q", redirect.NewDeliveryNode, "LINE1-ALT")
	}
}

func TestDecodeEnvelope_UnknownType(t *testing.T) {
	data := []byte(`{
		"msg_type": "bogus",
		"msg_id": "msg-x",
		"client_id": "line-1",
		"factory_id": "plant-alpha",
		"timestamp": "2026-02-17T12:00:00Z",
		"payload": {}
	}`)

	_, err := DecodeEnvelope(data)
	if err == nil {
		t.Fatal("expected error for unknown msg_type")
	}
}

func TestDecodeEnvelope_InvalidJSON(t *testing.T) {
	_, err := DecodeEnvelope([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDecodeEnvelope_InvalidPayload(t *testing.T) {
	data := []byte(`{
		"msg_type": "order_request",
		"msg_id": "msg-y",
		"client_id": "line-1",
		"factory_id": "plant-alpha",
		"timestamp": "2026-02-17T12:00:00Z",
		"payload": "not an object"
	}`)

	_, err := DecodeEnvelope(data)
	if err == nil {
		t.Fatal("expected error for invalid payload")
	}
}

func TestNewEnvelope(t *testing.T) {
	payload := AckReply{OrderUUID: "uuid-5", WarpathOrderID: 42, SourceNode: "STORAGE-A1"}
	env := NewEnvelope("ack", "line-1", "plant-alpha", payload)

	if env.MsgType != "ack" {
		t.Errorf("msg_type = %q, want %q", env.MsgType, "ack")
	}
	if env.ClientID != "line-1" {
		t.Errorf("client_id = %q, want %q", env.ClientID, "line-1")
	}
	if env.FactoryID != "plant-alpha" {
		t.Errorf("factory_id = %q, want %q", env.FactoryID, "plant-alpha")
	}
	if env.MsgID == "" {
		t.Error("msg_id should not be empty")
	}
	if env.Timestamp.IsZero() {
		t.Error("timestamp should not be zero")
	}

	ack, ok := env.Payload.(AckReply)
	if !ok {
		t.Fatalf("payload type = %T, want AckReply", env.Payload)
	}
	if ack.WarpathOrderID != 42 {
		t.Errorf("warpath_order_id = %d, want 42", ack.WarpathOrderID)
	}
}

func TestEnvelopeEncode(t *testing.T) {
	env := NewEnvelope("update", "line-1", "plant-alpha", UpdateReply{
		OrderUUID: "uuid-6",
		Status:    "in_transit",
		Detail:    "robot moving",
	})

	data, err := env.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal encoded: %v", err)
	}

	if decoded["msg_type"] != "update" {
		t.Errorf("msg_type = %v, want %q", decoded["msg_type"], "update")
	}
	payload, ok := decoded["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map", decoded["payload"])
	}
	if payload["status"] != "in_transit" {
		t.Errorf("status = %v, want %q", payload["status"], "in_transit")
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	original := NewEnvelope("order_request", "line-3", "factory-1", OrderRequest{
		OrderUUID:    "uuid-rt",
		OrderType:    "store",
		MaterialCode: "PART-B",
		Quantity:     1.5,
		DeliveryNode: "LINE3-IN",
		Priority:     3,
	})

	data, err := original.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.MsgType != original.MsgType {
		t.Errorf("msg_type = %q, want %q", decoded.MsgType, original.MsgType)
	}
	if decoded.ClientID != original.ClientID {
		t.Errorf("client_id = %q, want %q", decoded.ClientID, original.ClientID)
	}

	req, ok := decoded.Payload.(OrderRequest)
	if !ok {
		t.Fatalf("payload type = %T, want OrderRequest", decoded.Payload)
	}
	if req.MaterialCode != "PART-B" {
		t.Errorf("material_code = %q, want %q", req.MaterialCode, "PART-B")
	}
	if req.Quantity != 1.5 {
		t.Errorf("quantity = %f, want 1.5", req.Quantity)
	}
}

func TestDispatchTopic(t *testing.T) {
	topic := DispatchTopic("warpath/dispatch", "line-1")
	if topic != "warpath/dispatch/line-1" {
		t.Errorf("topic = %q, want %q", topic, "warpath/dispatch/line-1")
	}
}

func TestEnvelopeTimestampParsing(t *testing.T) {
	ts := "2026-02-17T12:30:45Z"
	data := []byte(`{
		"msg_type": "order_cancel",
		"msg_id": "msg-ts",
		"client_id": "line-1",
		"factory_id": "plant-alpha",
		"timestamp": "` + ts + `",
		"payload": {"order_uuid": "uuid-ts", "reason": "test"}
	}`)

	env, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	expected, _ := time.Parse(time.RFC3339, ts)
	if !env.Timestamp.Equal(expected) {
		t.Errorf("timestamp = %v, want %v", env.Timestamp, expected)
	}
}
