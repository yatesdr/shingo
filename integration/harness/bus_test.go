package harness

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/router"

	edgemessaging "shingoedge/store/messaging"

	coremessaging "shingocore/store/messaging"
)

// =============================================================================
// Skeleton self-tests
//
// These verify the Bus API compiles, constructs, and routes envelopes
// end-to-end. They use minimal in-process fake stores (not real DBs)
// because:
//   1. Edge SQLite + Core Postgres setup is a separate concern (testdb
//      exposure is the next round of work).
//   2. The Bus's contract — "pump moves payloads from outbox to ingestor
//      and acks" — can be verified in isolation. If Bus is broken, no
//      scenario test built on top of it will be reliable.
//
// When the next round adds real DB construction, scenario tests live
// alongside in this package and use the same Bus API.
// =============================================================================

// fakeEdgeStore is a minimal in-memory implementation of EdgeOutboxOps.
// Production tests will pass a real *shingoedge/store.DB instead.
type fakeEdgeStore struct {
	pending []edgemessaging.Message
	acked   []int64
	retries map[int64]int
	nextID  int64
}

func newFakeEdgeStore() *fakeEdgeStore {
	return &fakeEdgeStore{retries: map[int64]int{}}
}

func (s *fakeEdgeStore) Enqueue(payload []byte, msgType string) int64 {
	s.nextID++
	s.pending = append(s.pending, edgemessaging.Message{
		ID: s.nextID, Payload: payload, MsgType: msgType,
	})
	return s.nextID
}

func (s *fakeEdgeStore) ListPendingOutbox(limit int) ([]edgemessaging.Message, error) {
	if len(s.pending) <= limit {
		out := s.pending
		return out, nil
	}
	return s.pending[:limit], nil
}

func (s *fakeEdgeStore) AckOutbox(id int64) error {
	s.acked = append(s.acked, id)
	for i := range s.pending {
		if s.pending[i].ID == id {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *fakeEdgeStore) IncrementOutboxRetries(id int64) error {
	s.retries[id]++
	return nil
}

func (s *fakeEdgeStore) MarkOutboxExhausted(id int64, reason string) error {
	s.retries[id] = 999
	return nil
}

func (s *fakeEdgeStore) PurgeOldOutbox(olderThan time.Duration) (int64, error) {
	return 0, nil
}

// fakeCoreStore mirrors fakeEdgeStore for the Core message shape
// (*OutboxMessage pointer slice, with Topic field).
type fakeCoreStore struct {
	pending []*coremessaging.OutboxMessage
	acked   []int64
	retries map[int64]int
	nextID  int64
}

func newFakeCoreStore() *fakeCoreStore {
	return &fakeCoreStore{retries: map[int64]int{}}
}

func (s *fakeCoreStore) Enqueue(topic string, payload []byte, msgType string) int64 {
	s.nextID++
	s.pending = append(s.pending, &coremessaging.OutboxMessage{
		ID: s.nextID, Topic: topic, Payload: payload, MsgType: msgType,
	})
	return s.nextID
}

func (s *fakeCoreStore) ListPendingOutbox(limit int) ([]*coremessaging.OutboxMessage, error) {
	if len(s.pending) <= limit {
		return s.pending, nil
	}
	return s.pending[:limit], nil
}

func (s *fakeCoreStore) AckOutbox(id int64) error {
	s.acked = append(s.acked, id)
	for i := range s.pending {
		if s.pending[i].ID == id {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *fakeCoreStore) IncrementOutboxRetries(id int64) error {
	s.retries[id]++
	return nil
}

func (s *fakeCoreStore) MarkOutboxExhausted(id int64, reason string) error {
	s.retries[id] = 999
	return nil
}

func (s *fakeCoreStore) PurgeOldOutbox(olderThan time.Duration) (int64, error) {
	return 0, nil
}

// recordingHandler captures per-Type dispatch for the Bus tests. The
// router registrations in newRecordingIngestor wire these methods.
type recordingHandler struct {
	releaseSeen []*protocol.OrderRelease
	requestSeen []*protocol.OrderRequest
}

func (h *recordingHandler) HandleOrderRelease(env *protocol.Envelope, p *protocol.OrderRelease) {
	h.releaseSeen = append(h.releaseSeen, p)
}

func (h *recordingHandler) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	h.requestSeen = append(h.requestSeen, p)
}

// newRecordingIngestor builds an Ingestor + Router pair that wires the
// recordingHandler's two captured types and ignores the rest. Replaces
// the pre-3.4h router.NewMessageHandlerIngestor helper — these tests
// only need OrderRequest and OrderRelease, so registering 17 types via
// a god-interface was always overkill.
func newRecordingIngestor(h *recordingHandler) *protocol.Ingestor {
	ing := protocol.NewIngestor(nil)
	r := router.New[string]()
	router.Register(r, protocol.TypeOrderRequest, h.HandleOrderRequest)
	router.Register(r, protocol.TypeOrderRelease, h.HandleOrderRelease)
	ing.Dispatch = func(env *protocol.Envelope) {
		r.Dispatch(env, env.Type)
	}
	return ing
}

func enqueueOrderRelease(t *testing.T, store *fakeEdgeStore, uuid string, uop *int) {
	t.Helper()
	env, err := protocol.NewEnvelope(
		protocol.TypeOrderRelease,
		protocol.Address{Role: protocol.RoleEdge, Station: "test.station"},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.OrderRelease{OrderUUID: uuid, RemainingUOP: uop},
	)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	payload, err := env.Encode()
	if err != nil {
		t.Fatalf("Encode envelope: %v", err)
	}
	store.Enqueue(payload, protocol.TypeOrderRelease)
}

// TestBus_PumpEdgeOutbox_DeliversEnvelopeToCoreIngestor is the
// foundational round-trip: enqueue an OrderRelease on Edge's fake
// store, pump, assert Core's recording handler observed it with the
// right fields. The envelope is JSON-marshaled in the enqueue and
// JSON-unmarshaled by the ingestor — same path production uses.
//
// If this test goes red, no scenario test will work.
func TestBus_PumpEdgeOutbox_DeliversEnvelopeToCoreIngestor(t *testing.T) {
	edgeStore := newFakeEdgeStore()
	coreStore := newFakeCoreStore()
	coreHandler := &recordingHandler{}
	edgeHandler := &recordingHandler{}

	bus := NewBus(t,
		EdgeSide{
			EdgeStore:    edgeStore,
			EdgeIngestor: newRecordingIngestor(edgeHandler),
		},
		CoreSide{
			CoreStore:    coreStore,
			CoreIngestor: newRecordingIngestor(coreHandler),
		},
	)

	// Enqueue an OrderRelease on Edge → expect Core to see it.
	uop := 300
	enqueueOrderRelease(t, edgeStore, "round-trip-1", &uop)

	delivered := bus.PumpEdgeOutbox()
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}

	if len(coreHandler.releaseSeen) != 1 {
		t.Fatalf("Core saw %d OrderRelease envelopes, want 1", len(coreHandler.releaseSeen))
	}
	got := coreHandler.releaseSeen[0]
	if got.OrderUUID != "round-trip-1" {
		t.Errorf("OrderUUID = %q, want round-trip-1", got.OrderUUID)
	}
	if got.RemainingUOP == nil {
		t.Fatal("RemainingUOP = nil; want pointer to 300")
	}
	if *got.RemainingUOP != 300 {
		t.Errorf("*RemainingUOP = %d, want 300", *got.RemainingUOP)
	}

	// Edge's outbox should be drained.
	pending, _ := edgeStore.ListPendingOutbox(10)
	if len(pending) != 0 {
		t.Errorf("edge pending after pump = %d, want 0", len(pending))
	}
	if len(edgeStore.acked) != 1 {
		t.Errorf("edge acked = %d, want 1", len(edgeStore.acked))
	}
}

// TestBus_PumpAll_Settles drains both directions until empty and asserts
// the deadlock guard isn't hit on a normal flow.
func TestBus_PumpAll_Settles(t *testing.T) {
	edgeStore := newFakeEdgeStore()
	coreStore := newFakeCoreStore()
	bus := NewBus(t,
		EdgeSide{EdgeStore: edgeStore, EdgeIngestor: newRecordingIngestor(&recordingHandler{})},
		CoreSide{CoreStore: coreStore, CoreIngestor: newRecordingIngestor(&recordingHandler{})},
	)

	uop := 0
	enqueueOrderRelease(t, edgeStore, "settle-1", &uop)
	enqueueOrderRelease(t, edgeStore, "settle-2", &uop)
	enqueueOrderRelease(t, edgeStore, "settle-3", &uop)

	total := bus.PumpAll()
	if total != 3 {
		t.Errorf("PumpAll total = %d, want 3", total)
	}
}

// TestBus_FailNext_IncrementsRetries verifies the failure-injection
// knob: a single Publish failure causes IncrementOutboxRetries on the
// Edge store, the message stays pending, and the next pump succeeds.
func TestBus_FailNext_IncrementsRetries(t *testing.T) {
	edgeStore := newFakeEdgeStore()
	coreStore := newFakeCoreStore()
	coreHandler := &recordingHandler{}
	bus := NewBus(t,
		EdgeSide{EdgeStore: edgeStore, EdgeIngestor: newRecordingIngestor(&recordingHandler{})},
		CoreSide{CoreStore: coreStore, CoreIngestor: newRecordingIngestor(coreHandler)},
	)

	uop := 0
	enqueueOrderRelease(t, edgeStore, "retry-1", &uop)

	bus.EdgeFailNext(errFakeKafkaDown)
	delivered := bus.PumpEdgeOutbox()
	if delivered != 1 {
		t.Fatalf("delivered (count of pumped) = %d, want 1", delivered)
	}
	// But Core shouldn't have actually received it.
	if len(coreHandler.releaseSeen) != 0 {
		t.Errorf("Core saw %d on failed publish, want 0", len(coreHandler.releaseSeen))
	}
	// Retry counter should have incremented.
	if edgeStore.retries[1] != 1 {
		t.Errorf("retries[1] = %d, want 1 after failed publish", edgeStore.retries[1])
	}
	// Message should still be pending.
	pending, _ := edgeStore.ListPendingOutbox(10)
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1 (failed message stays in outbox)", len(pending))
	}

	// Second pump succeeds.
	delivered = bus.PumpEdgeOutbox()
	if delivered != 1 {
		t.Errorf("second pump delivered = %d, want 1", delivered)
	}
	if len(coreHandler.releaseSeen) != 1 {
		t.Errorf("Core saw %d after retry, want 1", len(coreHandler.releaseSeen))
	}
}

// errFakeKafkaDown is a sentinel for FailNext tests.
var errFakeKafkaDown = &fakeError{"fake kafka down"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }
