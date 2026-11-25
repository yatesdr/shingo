//go:build docker

package messaging

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/messaging/middleware"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

type countingBackend struct {
	cancels int
	creates int
}

func (m *countingBackend) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	m.creates++
	return fleet.TransportOrderResult{}, fmt.Errorf("mock: not connected")
}
func (m *countingBackend) CancelOrder(vendorOrderID string) error {
	m.cancels++
	return nil
}
func (m *countingBackend) SetOrderPriority(vendorOrderID string, priority int) error {
	return nil
}
func (m *countingBackend) Ping() error                             { return nil }
func (m *countingBackend) Name() string                            { return "counting" }
func (m *countingBackend) MapState(vendorState string) string      { return vendorState }
func (m *countingBackend) IsTerminalState(vendorState string) bool { return false }
func (m *countingBackend) Reconfigure(cfg fleet.ReconfigureParams) {}
func (m *countingBackend) ReleaseOrder(vendorOrderID string, blocks []fleet.OrderBlock, complete bool) error {
	return nil
}

type noopEmitter struct{}

func (noopEmitter) EmitOrderReceived(orderID int64, edgeUUID, stationID string, orderType protocol.OrderType, payloadCode, deliveryNode string) {
}
func (noopEmitter) EmitOrderDispatched(orderID int64, vendorOrderID, sourceNode, destNode string) {}
func (noopEmitter) EmitOrderFailed(orderID int64, edgeUUID, stationID, errorCode, detail string, recovers *int64) {
}
func (noopEmitter) EmitOrderSkipped(orderID int64, edgeUUID, stationID, errorCode, detail string) {}
func (noopEmitter) EmitOrderCancelled(orderID int64, edgeUUID, stationID, reason, previousStatus string, recovers *int64) {
}
func (noopEmitter) EmitOrderCompleted(orderID int64, edgeUUID, stationID string, recovers *int64) {}
func (noopEmitter) EmitOrderResumed(orderID int64, edgeUUID, stationID string)                    {}
func (noopEmitter) ProjectOrder(stationID string, projection protocol.OrderProjection)            {}

func TestCoreHandlerDeduplicatesRedirectByEnvelopeID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	line := &nodes.Node{Name: "LINE-1", Enabled: true}
	dest := &nodes.Node{Name: "LINE-2", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line), "create line node")
	testutil.MustNoErr(t, db.CreateNode(dest), "create destination node")

	// PRE-DISPATCH, and that is now the only redirectable state — mid-transit
	// redirect is circuit-broken at the handler (dispatch/dispatcher.go), so a
	// dispatched order would be refused and this test would be measuring the
	// refusal rather than the dedup it is named for.
	order := &orders.Order{
		EdgeUUID:     "uuid-redir",
		StationUID:   "edge.1",
		OrderType:    dispatch.OrderTypeMove,
		Status:       dispatch.StatusQueued,
		Quantity:     1,
		SourceNode:   line.Name,
		DeliveryNode: line.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")
	backend := &countingBackend{}
	dispatcher := dispatch.NewDispatcher(db, backend, noopEmitter{}, "core", "dispatch", nil)
	handler := NewCoreHandler(db, nil, "core", "dispatch", dispatcher)
	dedupMW := middleware.NewInboxDedup(db, nil)

	env := &protocol.Envelope{
		ID:   "msg-redirect-1",
		Type: protocol.TypeOrderRedirect,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	req := &protocol.OrderRedirect{OrderUUID: order.EdgeUUID, NewDeliveryNode: dest.Name}

	dispatchOnce := func() {
		dedupMW(env, env.Type, func() { handler.HandleOrderRedirect(env, req) })
	}
	dispatchOnce()
	dispatchOnce()

	// The observable is the fleet CREATE, not a cancel: a pre-dispatch redirect has
	// no live vendor order to terminate. Two identical envelopes, one create.
	if backend.creates != 1 {
		t.Fatalf("expected the redirect to reach the fleet once, got %d creates", backend.creates)
	}
	if backend.cancels != 0 {
		t.Fatalf("a pre-dispatch redirect must terminate nothing, got %d cancels", backend.cancels)
	}
	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.DeliveryNode != dest.Name {
		t.Fatalf("expected delivery node %q, got %q", dest.Name, got.DeliveryNode)
	}
}

func TestCoreHandlerDeduplicatesOrderRequestByEnvelopeID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	dest := &nodes.Node{Name: "LINE-REQ", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create destination node")
	payload := &payloads.Payload{Code: "PART-A", Description: "Part A", UOPCapacity: 10}
	testutil.MustNoErr(t, db.CreatePayload(payload), "create payload")

	backend := &countingBackend{}
	dispatcher := dispatch.NewDispatcher(db, backend, noopEmitter{}, "core", "dispatch", nil)
	handler := NewCoreHandler(db, nil, "core", "dispatch", dispatcher)
	dedupMW := middleware.NewInboxDedup(db, nil)

	env := &protocol.Envelope{
		ID:   "msg-request-1",
		Type: protocol.TypeOrderRequest,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	req := &protocol.OrderRequest{
		OrderUUID:    "uuid-request-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  payload.Code,
		DeliveryNode: dest.Name,
		Quantity:     1,
	}

	dispatchOnce := func() {
		dedupMW(env, env.Type, func() { handler.HandleOrderRequest(env, req) })
	}
	dispatchOnce()
	dispatchOnce()

	orders, err := db.ListOrders("", 10)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected 1 order after duplicate request replay, got %d", len(orders))
	}
}

func TestCoreHandlerDeduplicationPersistsAcrossHandlerRestart(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	dest := &nodes.Node{Name: "LINE-RESTART", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create destination node")
	payload := &payloads.Payload{Code: "PART-R", Description: "Part R", UOPCapacity: 10}
	testutil.MustNoErr(t, db.CreatePayload(payload), "create payload")

	env := &protocol.Envelope{
		ID:   "msg-restart-1",
		Type: protocol.TypeOrderRequest,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	req := &protocol.OrderRequest{
		OrderUUID:    "uuid-request-restart",
		OrderType:    dispatch.OrderTypeRetrieve,
		PayloadCode:  payload.Code,
		DeliveryNode: dest.Name,
		Quantity:     1,
	}

	dedupMW := middleware.NewInboxDedup(db, nil)

	first := NewCoreHandler(db, nil, "core", "dispatch", dispatch.NewDispatcher(db, &countingBackend{}, noopEmitter{}, "core", "dispatch", nil))
	dedupMW(env, env.Type, func() { first.HandleOrderRequest(env, req) })

	second := NewCoreHandler(db, nil, "core", "dispatch", dispatch.NewDispatcher(db, &countingBackend{}, noopEmitter{}, "core", "dispatch", nil))
	dedupMW(env, env.Type, func() { second.HandleOrderRequest(env, req) })

	orders, err := db.ListOrders("", 10)
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected 1 order after handler restart replay, got %d", len(orders))
	}
}

func TestCoreHandlerDeduplicatesReceiptAcrossHandlerRestart(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	order := &orders.Order{
		EdgeUUID:     "uuid-receipt-restart",
		StationUID:   "edge.1",
		OrderType:    dispatch.OrderTypeRetrieve,
		Status:       dispatch.StatusDelivered,
		DeliveryNode: "LINE-1",
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	env := &protocol.Envelope{
		ID:   "msg-receipt-restart-1",
		Type: protocol.TypeOrderReceipt,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
	req := &protocol.OrderReceipt{OrderUUID: order.EdgeUUID, ReceiptType: "delivered", FinalCount: 1}

	dedupMW := middleware.NewInboxDedup(db, nil)

	first := NewCoreHandler(db, nil, "core", "dispatch", dispatch.NewDispatcher(db, &countingBackend{}, noopEmitter{}, "core", "dispatch", nil))
	dedupMW(env, env.Type, func() { first.HandleOrderReceipt(env, req) })

	second := NewCoreHandler(db, nil, "core", "dispatch", dispatch.NewDispatcher(db, &countingBackend{}, noopEmitter{}, "core", "dispatch", nil))
	dedupMW(env, env.Type, func() { second.HandleOrderReceipt(env, req) })

	got, err := db.GetOrder(order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if got.CompletedAt == nil {
		t.Fatalf("expected completed_at to be set")
	}
	// ONE COMPLETION ROW, not one row. The claim here is about the DEDUP: a
	// replayed receipt must not complete the order twice. Counting every row
	// answered that question by accident and stopped doing so when orders.Create
	// began writing a birth row — the order is now born `delivered`, with an
	// entry saying so, and its completion is the second. Counting the completions
	// asks the question the test is named for.
	history, err := db.ListOrderHistory(order.ID)
	if err != nil {
		t.Fatalf("list order history: %v", err)
	}
	completions := 0
	for _, h := range history {
		if h.Status == dispatch.StatusConfirmed {
			completions++
		}
	}
	if completions != 1 {
		var got []string
		for _, h := range history {
			got = append(got, string(h.Status))
		}
		t.Fatalf("expected 1 completion row after the replayed receipt (deduped), got %d; history is %v",
			completions, got)
	}
}
func (noopEmitter) EmitOrderFaulted(orderID int64, edgeUUID, stationID, reason string)           {}
func (noopEmitter) EmitOrderFaultedRecovered(orderID int64, edgeUUID, stationID, robotID string) {}
func (noopEmitter) EmitBinHeld(int64)                                                            {}

// TestCoreHandler_AnOrderCutInHalfIsCreatedOnceAfterRedelivery (docket B11).
// Core reads an inbound message's inbox row, runs its handler, writes the row,
// then commits its offset (middleware.NewInboxDedup, Client.readLoop). A message
// cut inside the handler is delivered again, because its offset never moved.
// Until 2026-11-25 the row was written before the handler and the dedup dropped
// that redelivery as a duplicate: the order the station sent never existed at
// Core. Each case cuts a first Core there, then hands a second Core (the
// restart) what the first did not commit.
//
//   - A stop with the message in hand. main stopped the engine, then closed the
//     consumer, whose Close waited for no handler, then closed the database
//     under the handler.
//   - A crash before the order's INSERT. The cut run leaves no inbox row, so the
//     redelivery is handled. A crash after the INSERT is the order request's own
//     repeat (dispatch's TestRunTwice_OrderRequest).
func TestCoreHandler_AnOrderCutInHalfIsCreatedOnceAfterRedelivery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// cut runs the message through the first Core and cuts it there, and
		// reports whether the first Core committed the message's offset.
		cut func(t *testing.T, first *store.DB, deliver func(on *store.DB, work func())) bool
	}{
		{name: "a stop with the message in hand", cut: cutByAStop},
		{name: "a crash before the order's INSERT", cut: cutByACrash},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, cfg := testdb.OpenWithConfig(t)
			dest := &nodes.Node{Name: "LINE-CUT", Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(dest), "create destination node")
			payload := &payloads.Payload{Code: "PART-C", Description: "Part C", UOPCapacity: 10}
			testutil.MustNoErr(t, db.CreatePayload(payload), "create payload")
			env := &protocol.Envelope{
				ID:   "msg-cut-1",
				Type: protocol.TypeOrderRequest,
				Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
				Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
			}
			req := &protocol.OrderRequest{
				OrderUUID:    "uuid-cut-1",
				OrderType:    dispatch.OrderTypeRetrieve,
				PayloadCode:  payload.Code,
				DeliveryNode: dest.Name,
				Quantity:     1,
			}
			// deliver is one Core's inbound path for this message, on that Core's
			// pool: the dedup, then the order request's handler. work runs between
			// the two, where the handler's own work begins.
			deliver := func(on *store.DB, work func()) {
				h := NewCoreHandler(on, nil, "core", "dispatch",
					dispatch.NewDispatcher(on, &countingBackend{}, noopEmitter{}, "core", "dispatch", nil))
				middleware.NewInboxDedup(on, nil)(env, env.Type, func() {
					work()
					h.HandleOrderRequest(env, req)
				})
			}
			// The first Core's own pool on the same database, closed at its stop
			// as main closes Core's.
			first, err := store.OpenWithoutMigrate(cfg)
			testutil.MustNoErr(t, err, "open the first Core's pool")
			t.Cleanup(func() { first.Close() })

			if committed := tc.cut(t, first, deliver); !committed {
				// The restart: Kafka delivers again what was not committed.
				deliver(db, func() {})
			}

			got, err := db.ListOrders("", 10)
			testutil.MustNoErr(t, err, "list orders")
			if len(got) != 1 {
				t.Fatalf("%d order(s) for the station's request %s after the cut and the redelivery, want 1",
					len(got), req.OrderUUID)
			}
		})
	}
}

// cutByAStop runs the message through the first Core's consumer and stops that
// Core while the handler is at work: the stop comes once the inbox row is
// written, and the handler's work outlasts it by a moment. The stop is main's
// order for these two: the consumer's Close, then the pool's.
func cutByAStop(t *testing.T, first *store.DB, deliver func(on *store.DB, work func())) bool {
	const topic = "shingo.orders"
	part := &partition{msgs: []kafka.Message{{Topic: topic, Value: []byte("msg-cut-1")}}}
	r := part.reader()
	c := NewClient(&config.MessagingConfig{})
	c.kafka = &kafkaState{readers: map[string]kafkaReader{topic: r}}

	inHand, stopping, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		c.readLoop(topic, r, func(string, []byte) {
			deliver(first, func() {
				close(inHand)
				<-stopping
				time.Sleep(50 * time.Millisecond) // the rest of the handler's work
			})
		})
	}()
	select {
	case <-inHand:
	case <-time.After(5 * time.Second):
		t.Fatal("the first Core's consumer never handed the message to its handler")
	}
	close(stopping)
	c.Close()
	first.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the first Core's read loop did not end after its Close")
	}
	return part.committed() > 0
}

// cutByACrash ends the first Core inside the handler, before the order's
// INSERT: the goroutine ends there, as the process would, and nothing is
// committed.
func cutByACrash(t *testing.T, first *store.DB, deliver func(on *store.DB, work func())) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		deliver(first, runtime.Goexit)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the first Core did not end inside its handler")
	}
	return false
}

// partition is one Kafka partition and its consumer group's committed offset. A
// reader starts at the committed offset; a commit through a closed reader moves
// nothing, and a fetch from one ends, as kafka-go v0.4.50's Reader answers
// io.ErrClosedPipe and io.EOF after Close.
type partition struct {
	mu   sync.Mutex
	msgs []kafka.Message
	next int64 // the committed offset: the first message a new reader is given
}

func (p *partition) committed() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.next
}

func (p *partition) reader() *partitionReader {
	return &partitionReader{p: p, at: p.committed(), closed: make(chan struct{})}
}

type partitionReader struct {
	p      *partition
	at     int64
	closed chan struct{}
	once   sync.Once
}

func (r *partitionReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	r.p.mu.Lock()
	if r.at < int64(len(r.p.msgs)) {
		m := r.p.msgs[r.at]
		m.Offset = r.at
		r.at++
		r.p.mu.Unlock()
		return m, nil
	}
	r.p.mu.Unlock()
	select {
	case <-r.closed:
		return kafka.Message{}, io.EOF
	case <-ctx.Done():
		return kafka.Message{}, ctx.Err()
	}
}

func (r *partitionReader) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	select {
	case <-r.closed:
		return io.ErrClosedPipe
	default:
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	for _, m := range msgs {
		if m.Offset+1 > r.p.next {
			r.p.next = m.Offset + 1
		}
	}
	return nil
}

func (r *partitionReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}
