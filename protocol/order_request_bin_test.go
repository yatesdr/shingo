package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// A move that names its bin, across versions. The field travels Edge to Core
// and is additive: a Core that has never heard of it reads the move it always
// read and lifts whatever stands on the source, as before.
func TestOrderRequest_BinID_MixedVersion(t *testing.T) {
	req := OrderRequest{OrderUUID: "u-1", OrderType: OrderTypeMove, SourceNode: "SPOT", DeliveryNode: "SRC", BinID: 42}
	buf, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got OrderRequest
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.BinID != 42 || got.SourceNode != "SPOT" || got.DeliveryNode != "SRC" {
		t.Errorf("round-trip = %+v, want the bin and the move around it", got)
	}

	var oldCore struct {
		OrderUUID    string    `json:"order_uuid"`
		OrderType    OrderType `json:"order_type"`
		SourceNode   string    `json:"source_node"`
		DeliveryNode string    `json:"delivery_node"`
	}
	if err := json.Unmarshal(buf, &oldCore); err != nil {
		t.Fatalf("an old Core must ignore the field, not fail on it: %v", err)
	}
	if oldCore.OrderType != OrderTypeMove || oldCore.SourceNode != "SPOT" || oldCore.DeliveryNode != "SRC" {
		t.Errorf("old Core lost the move it understood: %+v", oldCore)
	}
}

// A move that names no bin sends nothing: the wire of every other order is
// unchanged.
func TestOrderRequest_BinID_OmittedWhenUnset(t *testing.T) {
	buf, err := json.Marshal(OrderRequest{OrderUUID: "u-2", OrderType: OrderTypeMove, SourceNode: "A", DeliveryNode: "B"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(buf), "bin_id") {
		t.Errorf("an unset bin was serialised anyway: %s", buf)
	}
}
