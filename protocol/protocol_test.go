package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	src := Address{Role: RoleEdge, Station: "plant-a.line-1"}
	dst := Address{Role: RoleCore}

	env, err := NewEnvelope(TypeOrderRequest, src, dst, &OrderRequest{
		OrderUUID: "test-uuid-123",
		OrderType: "retrieve",
		Quantity:  10,
	})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	if env.Version != Version {
		t.Errorf("version = %d, want %d", env.Version, Version)
	}
	if env.Type != TypeOrderRequest {
		t.Errorf("type = %q, want %q", env.Type, TypeOrderRequest)
	}
	if env.Src != src {
		t.Errorf("src = %+v, want %+v", env.Src, src)
	}
	if env.ID == "" {
		t.Error("ID should not be empty")
	}

	// Encode
	data, err := env.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Decode back
	var decoded Envelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Type != TypeOrderRequest {
		t.Errorf("decoded type = %q, want %q", decoded.Type, TypeOrderRequest)
	}
	if decoded.ID != env.ID {
		t.Errorf("decoded id = %q, want %q", decoded.ID, env.ID)
	}

	// Decode payload
	var req OrderRequest
	if err := decoded.DecodePayload(&req); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if req.OrderUUID != "test-uuid-123" {
		t.Errorf("order_uuid = %q, want %q", req.OrderUUID, "test-uuid-123")
	}
	if req.Quantity != 10 {
		t.Errorf("quantity = %d, want 10", req.Quantity)
	}
}

func TestNewReply(t *testing.T) {
	reply, err := NewReply(TypeOrderAck,
		Address{Role: RoleCore},
		Address{Role: RoleEdge, Station: "plant-a.line-1"},
		"orig-msg-id",
		&OrderAck{OrderUUID: "uuid-1", ShingoOrderID: 42},
	)
	if err != nil {
		t.Fatalf("NewReply: %v", err)
	}
	if reply.CorID != "orig-msg-id" {
		t.Errorf("cor = %q, want %q", reply.CorID, "orig-msg-id")
	}
	if reply.Type != TypeOrderAck {
		t.Errorf("type = %q, want %q", reply.Type, TypeOrderAck)
	}
}

func TestExpiry(t *testing.T) {
	env := &Envelope{ExpiresAt: time.Now().UTC().Add(-1 * time.Minute)}
	if !IsExpired(env) {
		t.Error("expected expired envelope to be detected")
	}

	env.ExpiresAt = time.Now().UTC().Add(10 * time.Minute)
	if IsExpired(env) {
		t.Error("expected future-expiry envelope to not be expired")
	}

	env.ExpiresAt = time.Time{}
	if IsExpired(env) {
		t.Error("expected zero-expiry envelope to not be expired")
	}
}

func TestExpiryHeader(t *testing.T) {
	hdr := &RawHeader{ExpiresAt: time.Now().UTC().Add(-1 * time.Second)}
	if !IsExpiredHeader(hdr) {
		t.Error("expected expired header to be detected")
	}

	hdr.ExpiresAt = time.Now().UTC().Add(5 * time.Minute)
	if IsExpiredHeader(hdr) {
		t.Error("expected future header to not be expired")
	}
}

func TestDefaultTTLFor(t *testing.T) {
	if ttl := DefaultTTLFor(TypeData); ttl != 5*time.Minute {
		t.Errorf("data TTL = %v, want 5m", ttl)
	}
	if ttl := DefaultTTLFor(TypeOrderDelivered); ttl != 60*time.Minute {
		t.Errorf("delivered TTL = %v, want 60m", ttl)
	}
	if ttl := DefaultTTLFor("unknown.type"); ttl != FallbackTTL {
		t.Errorf("unknown TTL = %v, want %v", ttl, FallbackTTL)
	}
}

func TestIngestorDispatch(t *testing.T) {
	handler := &testHandler{}
	ingestor := NewIngestor(handler, nil)

	// Build a valid data envelope with edge.register subject
	env, _ := NewDataEnvelope(SubjectEdgeRegister,
		Address{Role: RoleEdge, Station: "test-node"},
		Address{Role: RoleCore},
		&EdgeRegister{StationID: "test-node"},
	)
	data, _ := env.Encode()

	ingestor.HandleRaw(data)

	if !handler.dataCalled {
		t.Error("expected HandleData to be called")
	}
	if handler.dataPayload.Subject != SubjectEdgeRegister {
		t.Errorf("subject = %q, want %q", handler.dataPayload.Subject, SubjectEdgeRegister)
	}

	// Verify two-level decode of the body
	var reg EdgeRegister
	if err := json.Unmarshal(handler.dataPayload.Body, &reg); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if reg.StationID != "test-node" {
		t.Errorf("station_id = %q, want %q", reg.StationID, "test-node")
	}
}

func TestIngestorFilter(t *testing.T) {
	handler := &testHandler{}
	// Filter that rejects everything
	ingestor := NewIngestor(handler, func(_ *RawHeader) bool { return false })

	env, _ := NewDataEnvelope(SubjectEdgeRegister,
		Address{Role: RoleEdge, Station: "test-node"},
		Address{Role: RoleCore},
		&EdgeRegister{StationID: "test-node"},
	)
	data, _ := env.Encode()

	ingestor.HandleRaw(data)

	if handler.dataCalled {
		t.Error("expected handler to NOT be called when filter rejects")
	}
}

func TestIngestorDropsExpired(t *testing.T) {
	handler := &testHandler{}
	ingestor := NewIngestor(handler, nil)

	env, _ := NewDataEnvelope(SubjectEdgeRegister,
		Address{Role: RoleEdge, Station: "test-node"},
		Address{Role: RoleCore},
		&EdgeRegister{StationID: "test-node"},
	)
	// Force expiry in the past
	env.ExpiresAt = time.Now().UTC().Add(-1 * time.Minute)
	data, _ := env.Encode()

	ingestor.HandleRaw(data)

	if handler.dataCalled {
		t.Error("expected handler to NOT be called for expired message")
	}
}

func TestEdgeFilter(t *testing.T) {
	filter := func(hdr *RawHeader) bool {
		return hdr.Dst.Station == "plant-a.line-1" || hdr.Dst.Station == "*"
	}

	// Matching node
	if !filter(&RawHeader{Dst: Address{Station: "plant-a.line-1"}}) {
		t.Error("expected filter to accept matching node")
	}
	// Broadcast
	if !filter(&RawHeader{Dst: Address{Station: "*"}}) {
		t.Error("expected filter to accept broadcast")
	}
	// Other node
	if filter(&RawHeader{Dst: Address{Station: "plant-a.line-2"}}) {
		t.Error("expected filter to reject other node")
	}
}

func TestWireFormatKeys(t *testing.T) {
	env, _ := NewDataEnvelope(SubjectEdgeHeartbeat,
		Address{Role: RoleEdge, Station: "n1"},
		Address{Role: RoleCore},
		&EdgeHeartbeat{StationID: "n1", Uptime: 60},
	)
	data, _ := env.Encode()

	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify short keys are used
	expected := []string{"v", "type", "id", "src", "dst", "ts", "exp", "p"}
	for _, k := range expected {
		if _, ok := m[k]; !ok {
			t.Errorf("expected key %q in wire format", k)
		}
	}
	// Verify long keys are NOT present
	long := []string{"version", "payload", "timestamp", "expires_at", "source", "destination"}
	for _, k := range long {
		if _, ok := m[k]; ok {
			t.Errorf("unexpected long key %q in wire format", k)
		}
	}
}

func TestDataEnvelopeRoundTrip(t *testing.T) {
	src := Address{Role: RoleEdge, Station: "plant-a.line-1"}
	dst := Address{Role: RoleCore}

	env, err := NewDataEnvelope(SubjectEdgeRegister, src, dst, &EdgeRegister{
		StationID: "plant-a.line-1",
		Version:   "1.0.0",
	})
	if err != nil {
		t.Fatalf("NewDataEnvelope: %v", err)
	}

	if env.Type != TypeData {
		t.Errorf("type = %q, want %q", env.Type, TypeData)
	}

	// Encode and decode
	raw, err := env.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var decoded Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Level 1: decode Data
	var d Data
	if err := decoded.DecodePayload(&d); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if d.Subject != SubjectEdgeRegister {
		t.Errorf("subject = %q, want %q", d.Subject, SubjectEdgeRegister)
	}

	// Level 2: decode body
	var reg EdgeRegister
	if err := json.Unmarshal(d.Body, &reg); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if reg.StationID != "plant-a.line-1" {
		t.Errorf("station_id = %q, want %q", reg.StationID, "plant-a.line-1")
	}
	if reg.Version != "1.0.0" {
		t.Errorf("version = %q, want %q", reg.Version, "1.0.0")
	}
}

func TestDataTTLForSubjects(t *testing.T) {
	tests := []struct {
		subject string
		want    time.Duration
	}{
		{SubjectEdgeHeartbeat, 90 * time.Second},
		{SubjectEdgeHeartbeatAck, 90 * time.Second},
		{SubjectEdgeRegister, 5 * time.Minute},
		{SubjectEdgeRegistered, 5 * time.Minute},
		{"inventory.query", 5 * time.Minute}, // unknown subject falls back to TypeData default
	}
	for _, tt := range tests {
		if got := DataTTLFor(tt.subject); got != tt.want {
			t.Errorf("DataTTLFor(%q) = %v, want %v", tt.subject, got, tt.want)
		}
	}
}

func TestNewDataReply(t *testing.T) {
	reply, err := NewDataReply(SubjectEdgeRegistered,
		Address{Role: RoleCore, Station: "core"},
		Address{Role: RoleEdge, Station: "plant-a.line-1"},
		"orig-msg-id",
		&EdgeRegistered{StationID: "plant-a.line-1", Message: "registered"},
	)
	if err != nil {
		t.Fatalf("NewDataReply: %v", err)
	}
	if reply.Type != TypeData {
		t.Errorf("type = %q, want %q", reply.Type, TypeData)
	}
	if reply.CorID != "orig-msg-id" {
		t.Errorf("cor = %q, want %q", reply.CorID, "orig-msg-id")
	}

	// Decode and verify subject
	var d Data
	if err := reply.DecodePayload(&d); err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if d.Subject != SubjectEdgeRegistered {
		t.Errorf("subject = %q, want %q", d.Subject, SubjectEdgeRegistered)
	}
}

func TestDataWireFormat(t *testing.T) {
	env, _ := NewDataEnvelope(SubjectEdgeHeartbeat,
		Address{Role: RoleEdge, Station: "plant-a.line-1"},
		Address{Role: RoleCore},
		&EdgeHeartbeat{StationID: "plant-a.line-1", Uptime: 3600, Orders: 2},
	)
	raw, _ := env.Encode()

	// Parse the full wire JSON
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}

	// Verify type is "data"
	var typ string
	json.Unmarshal(wire["type"], &typ)
	if typ != "data" {
		t.Errorf("wire type = %q, want %q", typ, "data")
	}

	// Verify payload has "subject" and "data" keys
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(wire["p"], &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := payload["subject"]; !ok {
		t.Error("expected 'subject' key in payload")
	}
	if _, ok := payload["data"]; !ok {
		t.Error("expected 'data' key in payload")
	}

	// Verify subject value
	var subject string
	json.Unmarshal(payload["subject"], &subject)
	if subject != SubjectEdgeHeartbeat {
		t.Errorf("subject = %q, want %q", subject, SubjectEdgeHeartbeat)
	}

	// Verify inner data can be decoded
	var hb EdgeHeartbeat
	if err := json.Unmarshal(payload["data"], &hb); err != nil {
		t.Fatalf("unmarshal heartbeat data: %v", err)
	}
	if hb.Uptime != 3600 {
		t.Errorf("uptime = %d, want 3600", hb.Uptime)
	}
	if hb.Orders != 2 {
		t.Errorf("orders = %d, want 2", hb.Orders)
	}
}

func TestSignAndVerify(t *testing.T) {
	key := []byte("test-secret-key-1234")

	env, _ := NewEnvelope(TypeOrderRequest,
		Address{Role: RoleEdge, Station: "line-1"},
		Address{Role: RoleCore},
		&OrderRequest{OrderUUID: "uuid-sign-test", OrderType: "retrieve"},
	)
	data, _ := env.Encode()

	// Sign
	signed, err := Sign(data, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Verify with correct key
	unwrapped, err := VerifyAndUnwrap(signed, key)
	if err != nil {
		t.Fatalf("VerifyAndUnwrap: %v", err)
	}

	// Should be identical to original
	if string(unwrapped) != string(data) {
		t.Error("unwrapped data does not match original")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	key := []byte("correct-key")
	wrongKey := []byte("wrong-key")

	env, _ := NewEnvelope(TypeOrderRequest,
		Address{Role: RoleEdge, Station: "line-1"},
		Address{Role: RoleCore},
		&OrderRequest{OrderUUID: "uuid-1"},
	)
	data, _ := env.Encode()
	signed, _ := Sign(data, key)

	_, err := VerifyAndUnwrap(signed, wrongKey)
	if err != ErrInvalidSignature {
		t.Errorf("expected ErrInvalidSignature, got %v", err)
	}
}

func TestVerifyRejectsUnsignedWhenKeySet(t *testing.T) {
	key := []byte("my-key")

	// Plain envelope JSON (not wrapped in signed format)
	env, _ := NewEnvelope(TypeOrderRequest,
		Address{Role: RoleEdge, Station: "line-1"},
		Address{Role: RoleCore},
		&OrderRequest{OrderUUID: "uuid-1"},
	)
	data, _ := env.Encode()

	_, err := VerifyAndUnwrap(data, key)
	if err != ErrInvalidSignature {
		t.Errorf("expected ErrInvalidSignature for unsigned message, got %v", err)
	}
}

func TestVerifyPassthroughWhenNoKey(t *testing.T) {
	data := []byte(`{"v":1,"type":"order.request"}`)

	// nil key = signing disabled, should pass through
	out, err := VerifyAndUnwrap(data, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(data) {
		t.Error("data should pass through unchanged when key is nil")
	}

	// empty key = same behavior
	out, err = VerifyAndUnwrap(data, []byte{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(data) {
		t.Error("data should pass through unchanged when key is empty")
	}
}

func TestIngestorWithSigning(t *testing.T) {
	key := []byte("ingestor-test-key")
	handler := &testHandler{}
	ingestor := NewIngestor(handler, nil)
	ingestor.SigningKey = key

	// Create and sign a valid message
	env, _ := NewDataEnvelope(SubjectEdgeRegister,
		Address{Role: RoleEdge, Station: "test-node"},
		Address{Role: RoleCore},
		&EdgeRegister{StationID: "test-node"},
	)
	data, _ := env.Encode()
	signed, _ := Sign(data, key)

	ingestor.HandleRaw(signed)
	if !handler.dataCalled {
		t.Error("expected HandleData to be called for valid signed message")
	}
}

func TestIngestorRejectsUnsignedWhenKeySet(t *testing.T) {
	key := []byte("ingestor-test-key")
	handler := &testHandler{}
	ingestor := NewIngestor(handler, nil)
	ingestor.SigningKey = key

	// Send unsigned message
	env, _ := NewDataEnvelope(SubjectEdgeRegister,
		Address{Role: RoleEdge, Station: "test-node"},
		Address{Role: RoleCore},
		&EdgeRegister{StationID: "test-node"},
	)
	data, _ := env.Encode()

	ingestor.HandleRaw(data)
	if handler.dataCalled {
		t.Error("expected handler NOT to be called for unsigned message when signing is enabled")
	}
}

// testHandler tracks which methods were called.
type testHandler struct {
	NoOpHandler
	dataCalled  bool
	dataPayload Data
}

func (h *testHandler) HandleData(env *Envelope, p *Data) {
	h.dataCalled = true
	h.dataPayload = *p
}

func TestIsTerminal(t *testing.T) {
	for _, s := range []Status{StatusConfirmed, StatusCancelled, StatusFailed} {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = false, want true", s)
		}
	}
	for _, s := range []Status{StatusPending, StatusDelivered, StatusInTransit, StatusStaged, StatusQueued} {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = true, want false", s)
		}
	}
}

func TestValidForwardTransitions(t *testing.T) {
	tests := []struct{ from, to Status }{
		{StatusPending, StatusSubmitted},
		{StatusPending, StatusSourcing},
		{StatusSubmitted, StatusAcknowledged},
		{StatusAcknowledged, StatusInTransit},
		{StatusInTransit, StatusDelivered},
		{StatusInTransit, StatusStaged},
		{StatusStaged, StatusInTransit},
		{StatusDelivered, StatusConfirmed},
		{StatusDelivered, StatusCancelled},
		{StatusQueued, StatusInTransit},
		{StatusDispatched, StatusInTransit},
		{StatusSourcing, StatusQueued},
	}
	for _, tt := range tests {
		if !IsValidTransition(tt.from, tt.to) {
			t.Errorf("IsValidTransition(%q, %q) = false, want true", tt.from, tt.to)
		}
	}
}

func TestInvalidBackwardTransitions(t *testing.T) {
	tests := []struct{ from, to Status }{
		{StatusDelivered, StatusInTransit},
		{StatusInTransit, StatusAcknowledged},
		{StatusAcknowledged, StatusSubmitted},
		{StatusConfirmed, StatusDelivered},
		{StatusCancelled, StatusPending},
	}
	for _, tt := range tests {
		if IsValidTransition(tt.from, tt.to) {
			t.Errorf("IsValidTransition(%q, %q) = true, want false", tt.from, tt.to)
		}
	}
}

func TestTerminalStatesCannotTransition(t *testing.T) {
	for _, from := range []Status{StatusConfirmed, StatusCancelled, StatusFailed} {
		for _, to := range []Status{StatusPending, StatusDelivered, StatusConfirmed} {
			if IsValidTransition(from, to) {
				t.Errorf("IsValidTransition(%q, %q) = true, want false (terminal state)", from, to)
			}
		}
	}
}

func TestUnknownStatusNotValidTransition(t *testing.T) {
	if IsValidTransition("unknown", StatusDelivered) {
		t.Error("expected unknown 'from' status to be invalid")
	}
	if IsValidTransition(StatusPending, "unknown") {
		t.Error("expected unknown 'to' status to be invalid")
	}
}

// TestIsTerminalDerivedFromTable asserts that every key in validTransitions
// is non-terminal — IsTerminal is now derived from "no key in the map" so
// any status with at least one outgoing edge MUST report as non-terminal.
func TestIsTerminalDerivedFromTable(t *testing.T) {
	for status := range validTransitions {
		if IsTerminal(status) {
			t.Errorf("status %q has outgoing edges in validTransitions but IsTerminal returned true", status)
		}
	}
}

// TestEveryKeyHasOutgoingEdge enforces the invariant that no key in
// validTransitions has an empty []string{} value. An empty edge list would
// register as "has key but no transitions" — IsTerminal would falsely say
// false (because the key exists), but no transition would ever validate.
func TestEveryKeyHasOutgoingEdge(t *testing.T) {
	for status, edges := range validTransitions {
		if len(edges) == 0 {
			t.Errorf("status %q has empty outgoing edge list — either remove the key (making it terminal) or add transitions", status)
		}
	}
}

// TestReshufflingTransitions verifies the StatusReshuffling lifecycle.
// Per compound.go evidence: parent goes Pending → Reshuffling →
// {Confirmed | Failed | Cancelled}; children go through their own
// lifecycle and never hold Reshuffling.
func TestReshufflingTransitions(t *testing.T) {
	if !IsValidTransition(StatusPending, StatusReshuffling) {
		t.Error("Pending → Reshuffling must be valid (compound parent entry)")
	}
	for _, to := range []Status{StatusConfirmed, StatusFailed, StatusCancelled} {
		if !IsValidTransition(StatusReshuffling, to) {
			t.Errorf("Reshuffling → %s must be valid (compound parent terminal)", to)
		}
	}
	for _, to := range []Status{StatusSourcing, StatusInTransit, StatusDelivered, StatusStaged} {
		if IsValidTransition(StatusReshuffling, to) {
			t.Errorf("Reshuffling → %s must NOT be valid (parent never enters in-flight)", to)
		}
	}
}

// TestAllStatusesCovered asserts every status returned by AllStatuses() is
// either a key in validTransitions (non-terminal) or has no key (terminal).
func TestAllStatusesCovered(t *testing.T) {
	for _, st := range AllStatuses() {
		_, hasKey := validTransitions[st]
		if !hasKey && !IsTerminal(st) {
			t.Errorf("status %q has no key in validTransitions but IsTerminal returned false", st)
		}
		if hasKey && IsTerminal(st) {
			t.Errorf("status %q has a key in validTransitions but IsTerminal returned true", st)
		}
	}
}

// TestAllValidTransitionsIsCopy verifies that AllValidTransitions returns
// a deep copy — mutating the returned map must not affect the canonical
// table.
func TestAllValidTransitionsIsCopy(t *testing.T) {
	dup := AllValidTransitions()
	dup[StatusPending] = []Status{"some-bogus-status"}
	if !IsValidTransition(StatusPending, StatusSourcing) {
		t.Error("AllValidTransitions returned a reference, not a copy — canonical table was mutated")
	}
}
