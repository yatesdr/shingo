//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
)

func TestReplySender_SendAck(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	rs := newReplySender(db, "test.topic", "core-station", nil)

	env := &protocol.Envelope{
		ID:  "corr-123",
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-1"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
	rs.SendAck(env, "order-uuid-1", 42, "STOR-A")

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("outbox messages = %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.MsgType != "order.ack" {
		t.Errorf("event_type = %q, want %q", msg.MsgType, "order.ack")
	}
	if msg.StationID != "line-1" {
		t.Errorf("station_id = %q, want %q", msg.StationID, "line-1")
	}

	var replyEnv protocol.Envelope
	if err := json.Unmarshal(msg.Payload, &replyEnv); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var ack protocol.OrderAck
	if err := json.Unmarshal(replyEnv.Payload, &ack); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}
	if ack.OrderUUID != "order-uuid-1" {
		t.Errorf("order_uuid = %q, want %q", ack.OrderUUID, "order-uuid-1")
	}
	if ack.ShingoOrderID != 42 {
		t.Errorf("shingo_order_id = %d, want 42", ack.ShingoOrderID)
	}
	if ack.SourceNode != "STOR-A" {
		t.Errorf("source_node = %q, want %q", ack.SourceNode, "STOR-A")
	}
}

func TestReplySender_SendError(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	rs := newReplySender(db, "test.topic", "core-station", nil)

	env := &protocol.Envelope{
		ID:  "corr-456",
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-2"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
	rs.SendError(env, "order-uuid-2", "fleet_failed", "robot stuck")

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("outbox messages = %d, want 1", len(msgs))
	}

	var replyEnv protocol.Envelope
	if err := json.Unmarshal(msgs[0].Payload, &replyEnv); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var p protocol.OrderError
	if err := json.Unmarshal(replyEnv.Payload, &p); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if p.OrderUUID != "order-uuid-2" {
		t.Errorf("order_uuid = %q, want %q", p.OrderUUID, "order-uuid-2")
	}
	if p.ErrorCode != "fleet_failed" {
		t.Errorf("error_code = %q, want %q", p.ErrorCode, "fleet_failed")
	}
	if p.Detail != "robot stuck" {
		t.Errorf("detail = %q, want %q", p.Detail, "robot stuck")
	}
}

func TestReplySender_SendCancelled(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	rs := newReplySender(db, "test.topic", "core-station", nil)

	env := &protocol.Envelope{
		ID:  "corr-789",
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-3"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
	rs.SendCancelled(env, "order-uuid-3", "operator cancelled")

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("outbox messages = %d, want 1", len(msgs))
	}
	if msgs[0].MsgType != "order.cancelled" {
		t.Errorf("event_type = %q, want %q", msgs[0].MsgType, "order.cancelled")
	}

	var replyEnv protocol.Envelope
	if err := json.Unmarshal(msgs[0].Payload, &replyEnv); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var p protocol.OrderCancelled
	if err := json.Unmarshal(replyEnv.Payload, &p); err != nil {
		t.Fatalf("unmarshal cancelled: %v", err)
	}
	if p.OrderUUID != "order-uuid-3" {
		t.Errorf("order_uuid = %q, want %q", p.OrderUUID, "order-uuid-3")
	}
	if p.Reason != "operator cancelled" {
		t.Errorf("reason = %q, want %q", p.Reason, "operator cancelled")
	}
}

func TestReplySender_SendUpdate(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	rs := newReplySender(db, "test.topic", "core-station", nil)

	env := &protocol.Envelope{
		ID:  "corr-update",
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-4"},
		Dst: protocol.Address{Role: protocol.RoleCore},
	}
	rs.SendUpdate(env, "order-uuid-4", "queued", "awaiting inventory")

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("outbox messages = %d, want 1", len(msgs))
	}
	if msgs[0].MsgType != "order.update" {
		t.Errorf("event_type = %q, want %q", msgs[0].MsgType, "order.update")
	}

	var replyEnv protocol.Envelope
	if err := json.Unmarshal(msgs[0].Payload, &replyEnv); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var p protocol.OrderUpdate
	if err := json.Unmarshal(replyEnv.Payload, &p); err != nil {
		t.Fatalf("unmarshal update: %v", err)
	}
	if p.OrderUUID != "order-uuid-4" {
		t.Errorf("order_uuid = %q, want %q", p.OrderUUID, "order-uuid-4")
	}
	if p.Status != "queued" {
		t.Errorf("status = %q, want %q", p.Status, "queued")
	}
	if p.Detail != "awaiting inventory" {
		t.Errorf("detail = %q, want %q", p.Detail, "awaiting inventory")
	}
}

func TestReplySender_SendReply_EnvelopeConstruction(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	rs := newReplySender(db, "custom.topic", "core-station-99", nil)

	err := rs.SendReply(protocol.TypeOrderAck, "order.custom", "edge-station", "env-id-1", &protocol.OrderAck{
		OrderUUID:     "test-uuid",
		ShingoOrderID: 99,
	})
	if err != nil {
		t.Fatalf("SendReply: %v", err)
	}

	msgs, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("outbox messages = %d, want 1", len(msgs))
	}
	msg := msgs[0]
	if msg.Topic != "custom.topic" {
		t.Errorf("topic = %q, want %q", msg.Topic, "custom.topic")
	}
	if msg.MsgType != "order.custom" {
		t.Errorf("event_type = %q, want %q", msg.MsgType, "order.custom")
	}
	if msg.StationID != "edge-station" {
		t.Errorf("station_id = %q, want %q", msg.StationID, "edge-station")
	}
}
