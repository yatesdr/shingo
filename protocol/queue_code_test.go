package protocol

import (
	"encoding/json"
	"testing"
)

// TestOrderUpdate_QueueCode_MixedVersion proves the queue_code field on
// OrderUpdate is safe across mixed Core/Edge versions:
//   - old Core (no queue_code) → new Edge: the field is absent on the wire and
//     deserializes to "" (Edge falls back to the sentence in QueueReason).
//   - new Core (queue_code set) → old Edge: the extra field is ignored by old
//     Edge's decoder (JSON ignores unknown fields) — verified by decoding into
//     a struct that omits it.
//
// Additive field, omitempty both directions: no wire migration required.
func TestOrderUpdate_QueueCode_MixedVersion(t *testing.T) {
	// new Core sends both sentence + code.
	out := OrderUpdate{OrderUUID: "U1", Status: "queued", QueueReason: "Waiting for material: P1", QueueCode: "waiting_for_material"}
	buf, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// new Edge decodes both.
	var got OrderUpdate
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.QueueCode != "waiting_for_material" || got.QueueReason != "Waiting for material: P1" {
		t.Errorf("round-trip lost fields: %+v", got)
	}

	// old Edge (struct without QueueCode) ignores the new field — no error.
	var oldEdge struct {
		OrderUUID   string `json:"order_uuid"`
		Status      string `json:"status"`
		QueueReason string `json:"queue_reason"`
	}
	if err := json.Unmarshal(buf, &oldEdge); err != nil {
		t.Fatalf("old Edge decode should ignore queue_code, got: %v", err)
	}
	if oldEdge.QueueReason != "Waiting for material: P1" {
		t.Errorf("old Edge lost QueueReason: %+v", oldEdge)
	}

	// old Core (no queue_code on the wire) → new Edge sees "".
	oldWire := []byte(`{"order_uuid":"U2","status":"queued","queue_reason":"no bin"}`)
	var newEdge OrderUpdate
	if err := json.Unmarshal(oldWire, &newEdge); err != nil {
		t.Fatalf("new Edge decode old wire: %v", err)
	}
	if newEdge.QueueCode != "" {
		t.Errorf("old Core wire should leave QueueCode empty, got %q", newEdge.QueueCode)
	}
	if newEdge.QueueReason != "no bin" {
		t.Errorf("new Edge lost QueueReason from old wire: %+v", newEdge)
	}
}

// TestOrderStatusSnapshot_QueueCode_MixedVersion is the snapshot analogue: the
// boot-reconcile snapshot carries queue_code additively, so a resync doesn't
// lose the code, and an old Core's snapshot (no queue_code) decodes to "".
func TestOrderStatusSnapshot_QueueCode_MixedVersion(t *testing.T) {
	out := OrderStatusSnapshot{OrderUUID: "U1", Found: true, Status: "queued",
		QueueReason: "Waiting for a slot at ASRS", QueueCode: "waiting_for_slot"}
	buf, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got OrderStatusSnapshot
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.QueueCode != "waiting_for_slot" {
		t.Errorf("snapshot round-trip lost QueueCode: %+v", got)
	}

	// old Core snapshot → new Edge sees "" for the code (falls back to sentence).
	oldWire := []byte(`{"order_uuid":"U2","found":true,"status":"queued","queue_reason":"no slot"}`)
	var newEdge OrderStatusSnapshot
	if err := json.Unmarshal(oldWire, &newEdge); err != nil {
		t.Fatalf("new Edge decode old snapshot: %v", err)
	}
	if newEdge.QueueCode != "" || newEdge.QueueReason != "no slot" {
		t.Errorf("old snapshot decode wrong: %+v", newEdge)
	}
}
