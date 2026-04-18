//go:build docker

package messaging

import (
	"testing"

	"shingo/protocol"
)

// countingHandler is a MessageHandler that records call counts per
// method. It embeds NoOpHandler so only the methods we override
// participate in the test surface.
type countingHandler struct {
	protocol.NoOpHandler

	data         int
	orderRequest int
	orderCancel  int
}

func (c *countingHandler) HandleData(*protocol.Envelope, *protocol.Data)              { c.data++ }
func (c *countingHandler) HandleOrderRequest(*protocol.Envelope, *protocol.OrderRequest) {
	c.orderRequest++
}
func (c *countingHandler) HandleOrderCancel(*protocol.Envelope, *protocol.OrderCancel) {
	c.orderCancel++
}

// TestInboxDedup_HandleData_UngatedPassthrough verifies that data
// envelopes are NOT passed through RecordInboundMessage. Two calls
// with the same envelope ID must both reach the inner handler,
// matching the pre-decorator behaviour (CoreHandler.HandleData went
// straight to CoreDataService.Handle without a dedup guard).
//
// No DB is needed: the passthrough path never touches the inbox.
func TestInboxDedup_HandleData_UngatedPassthrough(t *testing.T) {
	inner := &countingHandler{}
	dedup := NewInboxDedup(inner, nil) // nil db: passthrough never reads it

	env := &protocol.Envelope{
		ID:   "data-msg-1",
		Type: protocol.TypeData,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	p := &protocol.Data{Subject: "test.subject"}

	dedup.HandleData(env, p)
	dedup.HandleData(env, p)

	if inner.data != 2 {
		t.Fatalf("expected HandleData to fire twice (ungated), got %d", inner.data)
	}
}

// TestInboxDedup_HandleOrderRequest_GatedByDedup verifies the gated
// path: the first call reaches the inner handler, the replay is
// dropped at the decorator. Uses testDB because the dedup gate
// writes to the inbox table.
func TestInboxDedup_HandleOrderRequest_GatedByDedup(t *testing.T) {
	db := testDB(t)
	inner := &countingHandler{}
	dedup := NewInboxDedup(inner, db)

	env := &protocol.Envelope{
		ID:   "order-msg-1",
		Type: protocol.TypeOrderRequest,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	p := &protocol.OrderRequest{OrderUUID: "uuid-dedup-1"}

	dedup.HandleOrderRequest(env, p)
	dedup.HandleOrderRequest(env, p)

	if inner.orderRequest != 1 {
		t.Fatalf("expected HandleOrderRequest to fire once (gated), got %d", inner.orderRequest)
	}
}

// TestInboxDedup_EmptyEnvelopeID_ProcessesEveryTime preserves the
// quirk of the pre-decorator shouldProcessInbound: an envelope
// without an ID skips the inbox write and always processes. This
// lets internally-synthesized envelopes (tests, cli harnesses)
// through without pretending they have uniqueness.
func TestInboxDedup_EmptyEnvelopeID_ProcessesEveryTime(t *testing.T) {
	inner := &countingHandler{}
	dedup := NewInboxDedup(inner, nil) // nil db: empty-ID path skips the write

	env := &protocol.Envelope{
		ID:   "",
		Type: protocol.TypeOrderCancel,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
	}
	p := &protocol.OrderCancel{OrderUUID: "uuid-no-env-id"}

	dedup.HandleOrderCancel(env, p)
	dedup.HandleOrderCancel(env, p)
	dedup.HandleOrderCancel(env, p)

	if inner.orderCancel != 3 {
		t.Fatalf("expected HandleOrderCancel to fire thrice (empty ID bypasses dedup), got %d", inner.orderCancel)
	}
}
