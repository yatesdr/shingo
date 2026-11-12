package protocol

import (
	"encoding/json"
	"reflect"
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

// The press an order serves, on both request types: one field name, one tag.
// A plain order carries it as the multi-step order always has, so Core reads
// it from either the same way; unset, the wire is what it was.
func TestOrderRequest_ProcessNode_OneTagOnBothRequests(t *testing.T) {
	plain, _ := reflect.TypeFor[OrderRequest]().FieldByName("ProcessNode")
	complexReq, _ := reflect.TypeFor[ComplexOrderRequest]().FieldByName("ProcessNode")
	if plain.Tag != complexReq.Tag || plain.Tag.Get("json") != "process_node,omitempty" {
		t.Fatalf("tags differ: OrderRequest %q, ComplexOrderRequest %q", plain.Tag, complexReq.Tag)
	}
	buf, err := json.Marshal(OrderRequest{OrderUUID: "u-3", OrderType: OrderTypeRetrieveEmpty, DeliveryNode: "POS", ProcessNode: "PRESS"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got OrderRequest
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ProcessNode != "PRESS" || got.DeliveryNode != "POS" {
		t.Errorf("round-trip = %+v, want the press beside the delivery", got)
	}
	unset, err := json.Marshal(OrderRequest{OrderUUID: "u-4", OrderType: OrderTypeRetrieve, DeliveryNode: "POS"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(unset), "process_node") {
		t.Errorf("an unset press was serialised anyway: %s", unset)
	}
}
