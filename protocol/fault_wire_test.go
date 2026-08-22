package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Cloned from TestOrderUpdate_QueueCode_MixedVersion, which is the precedent
// for adding fields to this payload: additive, omitempty both directions, no
// wire migration. Core and Edge deploy separately and a plant runs weeks behind
// main, so "new Core, old Edge" is the normal state of the world for a while
// after this lands, not an edge case.

func TestOrderUpdate_Fault_MixedVersion(t *testing.T) {
	since := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	deadline := since.Add(45 * time.Minute)

	out := OrderUpdate{
		OrderUUID: "U1", Status: "faulted", Detail: "Fault · cannot replan (60011)",
		FaultSince: &since, FaultDeadline: &deadline,
		FaultNotice: true, FaultNoticeAfterS: 60,
	}
	buf, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// new Edge decodes all four.
	var got OrderUpdate
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FaultSince == nil || !got.FaultSince.Equal(since) {
		t.Errorf("round-trip lost FaultSince: %+v", got.FaultSince)
	}
	if got.FaultDeadline == nil || !got.FaultDeadline.Equal(deadline) {
		t.Errorf("round-trip lost FaultDeadline: %+v", got.FaultDeadline)
	}
	if !got.FaultNotice || got.FaultNoticeAfterS != 60 {
		t.Errorf("round-trip lost the threshold pair: %+v", got)
	}

	// old Edge (struct without the fault fields) ignores them — no error, and
	// the Detail it already understood still arrives. This is the whole
	// compatibility claim: an Edge that has never heard of a fault clock still
	// gets a sentence to print.
	var oldEdge struct {
		OrderUUID string `json:"order_uuid"`
		Status    string `json:"status"`
		Detail    string `json:"detail"`
	}
	if err := json.Unmarshal(buf, &oldEdge); err != nil {
		t.Fatalf("old Edge decode should ignore the fault fields, got: %v", err)
	}
	if oldEdge.Detail != "Fault · cannot replan (60011)" {
		t.Errorf("old Edge lost Detail: %+v", oldEdge)
	}

	// old Core (no fault fields on the wire) → new Edge sees the zero values
	// and renders from Detail alone, exactly as it does today.
	oldWire := []byte(`{"order_uuid":"U2","status":"faulted","detail":"fleet state: FAILED"}`)
	var newEdge OrderUpdate
	if err := json.Unmarshal(oldWire, &newEdge); err != nil {
		t.Fatalf("new Edge decode old wire: %v", err)
	}
	if newEdge.FaultSince != nil || newEdge.FaultDeadline != nil {
		t.Errorf("old Core wire must leave the clock nil, got %+v / %+v",
			newEdge.FaultSince, newEdge.FaultDeadline)
	}
	if newEdge.FaultNotice || newEdge.FaultNoticeAfterS != 0 {
		t.Errorf("old Core wire must leave the threshold zero: %+v", newEdge)
	}
}

// A non-faulted update carries none of the four. The Edge derives its clear
// from the status (see the OrderUpdate doc comment and the queue_reason
// incident it cites), so an absent field must stay absent rather than becoming
// a zero the Edge could mistake for a pushed value.
func TestOrderUpdate_Fault_OmittedWhenNotFaulted(t *testing.T) {
	buf, err := json.Marshal(OrderUpdate{OrderUUID: "U1", Status: "in_transit"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, absent := range []string{"fault_since", "fault_deadline", "fault_notice", "fault_notice_after_s"} {
		if strings.Contains(string(buf), absent) {
			t.Errorf("%q must be omitted on a non-faulted update, got %s", absent, buf)
		}
	}
}

// The snapshot analogue: a boot reconcile restores a faulted order's clock
// rather than leaving the board with a badge and no sentence — which, for an
// order that is already stuck, may otherwise never arrive.
func TestOrderStatusSnapshot_Fault_MixedVersion(t *testing.T) {
	since := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	deadline := since.Add(45 * time.Minute)

	out := OrderStatusSnapshot{
		OrderUUID: "U1", Found: true, Status: "faulted",
		FaultSince: &since, FaultDeadline: &deadline,
		FaultNotice: true, FaultNoticeAfterS: 60,
	}
	buf, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got OrderStatusSnapshot
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FaultSince == nil || !got.FaultSince.Equal(since) || got.FaultNoticeAfterS != 60 {
		t.Errorf("snapshot round-trip lost the fault window: %+v", got)
	}

	oldWire := []byte(`{"order_uuid":"U2","found":true,"status":"faulted"}`)
	var newEdge OrderStatusSnapshot
	if err := json.Unmarshal(oldWire, &newEdge); err != nil {
		t.Fatalf("new Edge decode old snapshot: %v", err)
	}
	if newEdge.FaultSince != nil || newEdge.FaultNoticeAfterS != 0 {
		t.Errorf("old snapshot must decode to no clock: %+v", newEdge)
	}
}

// The projection carries them too, so an order Core heals down to an Edge that
// has no row for it explains its own fault on arrival.
func TestOrderProjection_Fault_RoundTrip(t *testing.T) {
	since := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	buf, err := json.Marshal(OrderProjection{
		OrderUUID: "U1", Status: "faulted",
		FaultSince: &since, FaultNoticeAfterS: 60,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got OrderProjection
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FaultSince == nil || !got.FaultSince.Equal(since) || got.FaultNoticeAfterS != 60 {
		t.Errorf("projection round-trip lost the fault window: %+v", got)
	}
}
