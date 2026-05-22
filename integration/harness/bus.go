package harness

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/outbox"

	edgemessaging "shingoedge/store/messaging"

	coremessaging "shingocore/store/messaging"
)

// Bus routes protocol envelopes between an Edge and a Core that the
// caller has already wired up. The Bus owns the in-memory publishers and
// the pump methods; it does NOT own the engines, dispatchers, or DBs —
// callers construct those however they want (production constructors,
// test fixtures, etc.) and hand them to NewBus.
//
// Why caller-constructs: Edge's testEngineDB and Core's testdb.Open live
// in their respective `_test.go` files and `internal/` packages, neither
// reachable from this module. Rather than refactor those exposures, the
// Bus accepts whatever the caller stands up. Future scenario tests can
// use whichever setup pattern matches their needs (production
// constructors with empty config, or a future shingo-{edge,core}/
// testharness package, etc.).
type Bus struct {
	t *testing.T

	edge EdgeSide
	core CoreSide

	// Outbox adapters wrap the typed DB methods into the outbox.Store
	// interface that pump expects.
	edgeOut *edgeOutboxAdapter
	coreOut *coreOutboxAdapter

	// In-memory publishers. edgePub.target = core ingestor (Edge → Core
	// delivery) and corePub.target = edge ingestor (the reverse).
	edgePub *memPublisher
	corePub *memPublisher
}

// EdgeSide collects the Edge-process objects the Bus needs to route
// messages. Caller is responsible for constructing each field.
type EdgeSide struct {
	// EdgeStore exposes the methods the Bus needs from the Edge DB.
	// Pass *shingoedge/store.DB directly — it satisfies this interface
	// structurally.
	EdgeStore EdgeOutboxOps

	// EdgeIngestor receives messages from Core. Caller wraps an
	// EdgeHandler with protocol.NewIngestor(handler, nil).
	EdgeIngestor *protocol.Ingestor
}

// CoreSide is the Core-process counterpart to EdgeSide.
type CoreSide struct {
	// CoreStore exposes the outbox methods the Bus needs. Pass
	// *shingocore/store.DB directly.
	CoreStore CoreOutboxOps

	// CoreIngestor receives messages from Edge. Caller wraps a
	// CoreHandler with protocol.NewIngestor(handler, nil).
	CoreIngestor *protocol.Ingestor
}

// EdgeOutboxOps is the subset of Edge *store.DB methods the Bus needs.
// Defined here so the integration module doesn't have to import
// shingoedge/messaging's unexported edgeOutboxStore adapter.
type EdgeOutboxOps interface {
	ListPendingOutbox(limit int) ([]edgemessaging.Message, error)
	AckOutbox(id int64) error
	IncrementOutboxRetries(id int64) error
	MarkOutboxExhausted(id int64, reason string) error
	PurgeOldOutbox(olderThan time.Duration) (int64, error)
}

// CoreOutboxOps is the Core-side counterpart. Core's outbox returns
// *messaging.OutboxMessage (pointer slice) where Edge returns Message
// (value slice) — the divergence is why each side gets its own
// adapter rather than a shared interface.
type CoreOutboxOps interface {
	ListPendingOutbox(limit int) ([]*coremessaging.OutboxMessage, error)
	AckOutbox(id int64) error
	IncrementOutboxRetries(id int64) error
	MarkOutboxExhausted(id int64, reason string) error
	PurgeOldOutbox(olderThan time.Duration) (int64, error)
}

// NewBus wires the routing between two pre-constructed sides. Caller
// must populate EdgeStore + EdgeIngestor + CoreStore + CoreIngestor
// before calling. NewBus does not start any goroutines and does not
// block.
func NewBus(t *testing.T, edge EdgeSide, core CoreSide) *Bus {
	t.Helper()
	if edge.EdgeStore == nil || edge.EdgeIngestor == nil {
		t.Fatal("harness: EdgeSide.EdgeStore and .EdgeIngestor must be set")
	}
	if core.CoreStore == nil || core.CoreIngestor == nil {
		t.Fatal("harness: CoreSide.CoreStore and .CoreIngestor must be set")
	}
	b := &Bus{
		t:    t,
		edge: edge,
		core: core,
	}
	b.edgeOut = &edgeOutboxAdapter{ops: edge.EdgeStore}
	b.coreOut = &coreOutboxAdapter{ops: core.CoreStore}
	// edgePub publishes Edge's outbox messages INTO Core's ingestor.
	// corePub does the reverse.
	b.edgePub = newMemPublisher(core.CoreIngestor)
	b.corePub = newMemPublisher(edge.EdgeIngestor)
	return b
}

// PumpEdgeOutbox drains Edge's outbox once, delivering each message to
// Core's ingestor synchronously. Returns count delivered.
//
// Use when the test cares about ordering: e.g. "after the operator's
// release, exactly one OrderRelease envelope reaches Core." For
// "let everything settle" assertions, prefer PumpAll.
func (b *Bus) PumpEdgeOutbox() int {
	b.t.Helper()
	return pump(b.edgeOut, b.edgePub, "edge→core")
}

// PumpCoreOutbox is the symmetric drain for Core → Edge. Returns count
// delivered.
func (b *Bus) PumpCoreOutbox() int {
	b.t.Helper()
	return pump(b.coreOut, b.corePub, "core→edge")
}

// PumpAll drains both directions until both outboxes are empty in the
// same iteration. Catches reply chains: if Core enqueues a reply while
// processing an inbound message, that reply gets delivered in the next
// iteration. Returns total messages delivered. Panics on deadlock (the
// guard is 100 iterations).
func (b *Bus) PumpAll() int {
	b.t.Helper()
	return pumpAll(b.edgeOut, b.edgePub, b.coreOut, b.corePub, 100)
}

// EdgeFailNext arms the Edge → Core publisher to fail its next
// Publish call once. Used by tests that verify retry behavior on
// the Edge outbox without needing real Kafka.
func (b *Bus) EdgeFailNext(err error) {
	b.t.Helper()
	b.edgePub.FailNext(err)
}

// CoreFailNext is the symmetric failure-injection knob for the
// Core → Edge direction.
func (b *Bus) CoreFailNext(err error) {
	b.t.Helper()
	b.corePub.FailNext(err)
}

// =============================================================================
// Outbox adapters
// =============================================================================
//
// Both Edge and Core expose `ListPendingOutbox` / `AckOutbox` / etc. on
// their respective *store.DB types, but with different message struct
// types (Edge: messaging.Message value, Core: *messaging.OutboxMessage
// pointer). The adapters translate each side's specific shape into the
// unified outbox.Store interface that the pump function consumes.
//
// Production has the same pattern (shingoedge/messaging.edgeOutboxStore
// and shingocore/messaging's equivalent) — but those adapters are
// unexported. We re-implement the trivial mapping rather than promote
// the production types, keeping the production API surface unchanged.

type edgeOutboxAdapter struct {
	ops EdgeOutboxOps
}

func (a *edgeOutboxAdapter) ListPendingOutbox(limit int) ([]outbox.Message, error) {
	msgs, err := a.ops.ListPendingOutbox(limit)
	if err != nil {
		return nil, err
	}
	result := make([]outbox.Message, len(msgs))
	for i, m := range msgs {
		result[i] = outbox.Message{
			ID:      m.ID,
			Topic:   "", // edge uses fixed topic from config; harness ignores topic
			Payload: m.Payload,
			MsgType: m.MsgType,
			Retries: m.Retries,
		}
	}
	return result, nil
}

func (a *edgeOutboxAdapter) AckOutbox(id int64) error {
	return a.ops.AckOutbox(id)
}

func (a *edgeOutboxAdapter) IncrementOutboxRetries(id int64) error {
	return a.ops.IncrementOutboxRetries(id)
}

func (a *edgeOutboxAdapter) MarkOutboxExhausted(id int64, reason string) error {
	return a.ops.MarkOutboxExhausted(id, reason)
}

func (a *edgeOutboxAdapter) PurgeOldOutbox(olderThan time.Duration) (int, error) {
	n, err := a.ops.PurgeOldOutbox(olderThan)
	return int(n), err
}

type coreOutboxAdapter struct {
	ops CoreOutboxOps
}

func (a *coreOutboxAdapter) ListPendingOutbox(limit int) ([]outbox.Message, error) {
	msgs, err := a.ops.ListPendingOutbox(limit)
	if err != nil {
		return nil, err
	}
	result := make([]outbox.Message, len(msgs))
	for i, m := range msgs {
		result[i] = outbox.Message{
			ID:      m.ID,
			Topic:   m.Topic,
			Payload: m.Payload,
			MsgType: m.MsgType,
			Retries: m.Retries,
		}
	}
	return result, nil
}

func (a *coreOutboxAdapter) AckOutbox(id int64) error {
	return a.ops.AckOutbox(id)
}

func (a *coreOutboxAdapter) IncrementOutboxRetries(id int64) error {
	return a.ops.IncrementOutboxRetries(id)
}

func (a *coreOutboxAdapter) MarkOutboxExhausted(id int64, reason string) error {
	return a.ops.MarkOutboxExhausted(id, reason)
}

func (a *coreOutboxAdapter) PurgeOldOutbox(olderThan time.Duration) (int, error) {
	n, err := a.ops.PurgeOldOutbox(olderThan)
	return int(n), err
}
