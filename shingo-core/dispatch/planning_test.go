package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
)

// TestExtractRemainingUOP_NilEnvelope verifies that a nil envelope returns nil.
func TestExtractRemainingUOP_NilEnvelope(t *testing.T) {
	t.Parallel()
	got := extractRemainingUOP(nil)
	if got != nil {
		t.Errorf("expected nil, got %v", *got)
	}
}

// TestExtractRemainingUOP_EmptyPayload verifies that an empty payload returns nil.
func TestExtractRemainingUOP_EmptyPayload(t *testing.T) {
	t.Parallel()
	env := &protocol.Envelope{}
	got := extractRemainingUOP(env)
	if got != nil {
		t.Errorf("expected nil, got %v", *got)
	}
}

// TestExtractRemainingUOP_NoField verifies that an envelope without remaining_uop returns nil.
func TestExtractRemainingUOP_NoField(t *testing.T) {
	t.Parallel()
	body, _ := json.Marshal(&protocol.OrderRequest{
		OrderUUID: "test",
		OrderType: "move",
		Quantity:  1,
	})
	env := orderRequestEnvelope(t, body)

	got := extractRemainingUOP(env)
	if got != nil {
		t.Errorf("expected nil (field not set), got %v", *got)
	}
}

// TestExtractRemainingUOP_Zero verifies extraction of remaining_uop=0 (full depletion).
func TestExtractRemainingUOP_Zero(t *testing.T) {
	t.Parallel()
	zero := 0
	body, _ := json.Marshal(&protocol.OrderRequest{
		OrderUUID:    "test-zero",
		OrderType:    "move",
		Quantity:     1,
		RemainingUOP: &zero,
	})
	env := orderRequestEnvelope(t, body)

	got := extractRemainingUOP(env)
	if got == nil {
		t.Fatal("expected non-nil for remaining_uop=0")
	}
	if *got != 0 {
		t.Errorf("remaining_uop = %d, want 0", *got)
	}
}

// TestExtractRemainingUOP_Positive verifies extraction of remaining_uop>0 (partial consumption).
func TestExtractRemainingUOP_Positive(t *testing.T) {
	t.Parallel()
	partial := 42
	body, _ := json.Marshal(&protocol.OrderRequest{
		OrderUUID:    "test-partial",
		OrderType:    "move",
		Quantity:     1,
		RemainingUOP: &partial,
	})
	env := orderRequestEnvelope(t, body)

	got := extractRemainingUOP(env)
	if got == nil {
		t.Fatal("expected non-nil for remaining_uop=42")
	}
	if *got != 42 {
		t.Errorf("remaining_uop = %d, want 42", *got)
	}
}

// TestExtractRemainingUOP_MalformedJSON verifies that malformed JSON returns nil gracefully.
func TestExtractRemainingUOP_MalformedJSON(t *testing.T) {
	t.Parallel()
	env := &protocol.Envelope{Payload: []byte(`{invalid`)}
	got := extractRemainingUOP(env)
	if got != nil {
		t.Errorf("expected nil for malformed JSON, got %v", *got)
	}
}

// orderRequestEnvelope is an order request's envelope as the Edge builds it:
// protocol.NewEnvelope marshals the OrderRequest straight into the payload.
func orderRequestEnvelope(t *testing.T, body []byte) *protocol.Envelope {
	t.Helper()
	env, err := protocol.NewEnvelope(protocol.TypeOrderRequest, protocol.Address{Station: "edge.test"},
		protocol.Address{Station: "core"}, json.RawMessage(body))
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	return env
}

// TestExtractRemainingUOP_ADataWrapperIsNotAnOrderRequest: the shape this used
// to read (an order request inside a data-channel wrapper) is not one the Edge
// sends, and its count is not taken for an order request's.
func TestExtractRemainingUOP_ADataWrapperIsNotAnOrderRequest(t *testing.T) {
	t.Parallel()
	n := 7
	body, _ := json.Marshal(&protocol.OrderRequest{OrderUUID: "wrapped", RemainingUOP: &n})
	payload, _ := json.Marshal(protocol.Data{Body: body})
	if got := extractRemainingUOP(&protocol.Envelope{Payload: payload}); got != nil {
		t.Errorf("a Data-wrapped body read as the request's count: %d", *got)
	}
}
