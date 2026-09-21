package messaging

import (
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingocore/store"
)

// CoreHandler handles the order-channel inbound protocol messages and
// owns the stale-edge detection loop. TypeData / Subject dispatch is
// owned by CoreDataService, registered against a SubjectRouter at the
// composition root; CoreHandler implements the coreDataResponder
// interface (dbg, replyData, sendData) so CoreDataService methods can
// publish reply envelopes through the outbox.
//
// dispatcher is the narrow consumer-side Dispatcher interface (see
// dispatcher.go in this package). *dispatch.Dispatcher satisfies it
// structurally, so engine wiring is unchanged; the indirection is
// what lets core_handler_test.go stub a fake.
type CoreHandler struct {
	db            *store.DB
	client        *Client
	stationID     string
	dispatchTopic string
	dispatcher    Dispatcher
	DebugLog      func(string, ...any)

	// StaleEdgeThreshold controls how long an edge can skip heartbeats
	// before the stale-detection loop marks it stale and announces it.
	// Composition root sets this after construction from MessagingConfig;
	// zero falls back to DefaultStaleEdgeThreshold.
	StaleEdgeThreshold time.Duration

	// Background goroutine for stale edge detection
	stopOnce sync.Once
	stopCh   chan struct{}
}

// DefaultStaleEdgeThreshold is the fallback used when the caller leaves
// StaleEdgeThreshold unset. 15 minutes matches the operations guidance
// in docs/bin-loader-unloader-architecture.md — long enough to ride out
// flaky links, short enough that a hard crash is reported while somebody
// can still act on it.
//
// EXPORTED BECAUSE IT IS NOW ANSWERING TWO QUESTIONS, and they must not be
// allowed to drift apart. The demand reconciler asks the same thing this loop
// asks — "has this station been quiet long enough that we should stop believing
// anything about it?" — and it asks it of the heartbeat timestamp directly
// rather than of the flag this loop sets. Two numbers for one question would
// mean a window in which this loop has already declared a station gone while
// the sweep is still deciding what its silence licenses.
const DefaultStaleEdgeThreshold = 15 * time.Minute

// NewCoreHandler creates a handler for the order-channel inbound
// messages and stale-edge management.
//
// dispatcher is accepted as the narrow Dispatcher interface. Callers
// (engine) pass the concrete *dispatch.Dispatcher; structural typing
// handles the rest.
//
// CoreDataService is constructed separately at the composition root —
// build it with messaging.NewCoreDataService(db, coreHandler) and
// register its HandleX methods on a router.SubjectRouter directly. This
// keeps the dispatch table grep-able from cmd/shingocore/main.go rather
// than buried in a constructor.
func NewCoreHandler(db *store.DB, client *Client, stationID, dispatchTopic string, dispatcher Dispatcher) *CoreHandler {
	return &CoreHandler{
		db:            db,
		client:        client,
		stationID:     stationID,
		dispatchTopic: dispatchTopic,
		dispatcher:    dispatcher,
		stopCh:        make(chan struct{}),
	}
}

func (h *CoreHandler) dbg(format string, args ...any) {
	if fn := h.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// coreAddr returns the core-side protocol address.
func (h *CoreHandler) coreAddr() protocol.Address {
	return protocol.Address{Role: protocol.RoleCore, Station: h.stationID}
}

func (h *CoreHandler) enqueueEnvelope(msgType, stationID string, env interface{ Encode() ([]byte, error) }) error {
	data, err := env.Encode()
	if err != nil {
		return err
	}
	return h.db.EnqueueOutbox(h.dispatchTopic, data, msgType, stationID)
}

// replyData builds and publishes a data reply envelope to the requesting edge station.
func (h *CoreHandler) replyData(env *protocol.Envelope, subject string, payload any) {
	dst := protocol.Address{Role: protocol.RoleEdge, Station: env.Src.Station}
	reply, err := protocol.NewDataReply(subject, h.coreAddr(), dst, env.ID, payload)
	if err != nil {
		log.Printf("core_handler: build reply %s: %v", subject, err)
		return
	}
	msgType := "data.reply." + subject
	if err := h.enqueueEnvelope(msgType, env.Src.Station, reply); err != nil {
		log.Printf("core_handler: enqueue reply %s: %v", subject, err)
	}
}

// sendData builds and publishes a data envelope (not a reply) to a specific station.
func (h *CoreHandler) sendData(subject, stationID string, payload any) {
	dst := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	env, err := protocol.NewDataEnvelope(subject, h.coreAddr(), dst, payload)
	if err != nil {
		log.Printf("core_handler: build %s for %s: %v", subject, stationID, err)
		return
	}
	msgType := "data." + subject
	if err := h.enqueueEnvelope(msgType, stationID, env); err != nil {
		log.Printf("core_handler: enqueue %s for %s: %v", subject, stationID, err)
	}
}

// Start begins the stale-edge detection goroutine.
func (h *CoreHandler) Start() {
	go h.staleEdgeLoop()
}

// Stop halts the stale-edge detection goroutine.
func (h *CoreHandler) Stop() {
	h.stopOnce.Do(func() { close(h.stopCh) })
}

// Order message handlers delegate to the dispatcher. Inbox
// deduplication is performed by the inbox-dedup router middleware
// (NewInboxDedup in messaging/middleware/dedup.go) scoped to the 8
// order-channel envelope types via UseFor in the composition root —
// these methods assume the envelope has already cleared the dedup
// guard.

func (h *CoreHandler) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	log.Printf("core_handler: order request from %s: uuid=%s type=%s", env.Src.Station, p.OrderUUID, p.OrderType)
	h.dbg("-> order_request from=%s uuid=%s type=%s", env.Src.Station, p.OrderUUID, p.OrderType)
	h.dispatcher.HandleOrderRequest(env, p)
}

func (h *CoreHandler) HandleOrderCancel(env *protocol.Envelope, p *protocol.OrderCancel) {
	log.Printf("core_handler: order cancel from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dbg("-> order_cancel from=%s uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderCancel(env, p)
}

func (h *CoreHandler) HandleOrderReceipt(env *protocol.Envelope, p *protocol.OrderReceipt) {
	log.Printf("core_handler: delivery receipt from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dbg("-> order_receipt from=%s uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderReceipt(env, p)
}

func (h *CoreHandler) HandleOrderRedirect(env *protocol.Envelope, p *protocol.OrderRedirect) {
	log.Printf("core_handler: redirect from %s: uuid=%s -> %s", env.Src.Station, p.OrderUUID, p.NewDeliveryNode)
	h.dbg("-> order_redirect from=%s uuid=%s new_dest=%s", env.Src.Station, p.OrderUUID, p.NewDeliveryNode)
	h.dispatcher.HandleOrderRedirect(env, p)
}

func (h *CoreHandler) HandleComplexOrderRequest(env *protocol.Envelope, p *protocol.ComplexOrderRequest) {
	log.Printf("core_handler: complex order from %s: uuid=%s steps=%d", env.Src.Station, p.OrderUUID, len(p.Steps))
	h.dbg("-> complex_order from=%s uuid=%s steps=%d", env.Src.Station, p.OrderUUID, len(p.Steps))
	h.dispatcher.HandleComplexOrderRequest(env, p)
}

func (h *CoreHandler) HandleOrderRelease(env *protocol.Envelope, p *protocol.OrderRelease) {
	log.Printf("core_handler: order release from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dbg("-> order_release from=%s uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderRelease(env, p)
}

func (h *CoreHandler) HandleOrderIngest(env *protocol.Envelope, p *protocol.OrderIngestRequest) {
	log.Printf("core_handler: order ingest from %s: uuid=%s payload=%s bin=%s", env.Src.Station, p.OrderUUID, p.PayloadCode, p.BinLabel)
	h.dbg("-> order_ingest from=%s uuid=%s payload=%s bin=%s", env.Src.Station, p.OrderUUID, p.PayloadCode, p.BinLabel)
	h.dispatcher.HandleOrderIngest(env, p)
}

func (h *CoreHandler) staleEdgeLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.SweepStaleEdges()
		}
	}
}

// SweepStaleEdges is ONE PASS of stale-edge detection: mark every active edge
// that has missed its heartbeat window, and tell each one that it was marked.
//
// SPLIT FROM THE LOOP SO A TEST CAN DRIVE IT, rather than waiting out a
// sixty-second ticker — the same shape Engine.reconcileDemandEpisodes already
// has in this repo, and for the same reason.
//
// It is EXPORTED because the tests that matter most for this pass are not in
// this package. What a stale station does to a demand episode is an engine
// question — episodes, bindings and the reconciling sweep all live there — and
// the only alternative is an engine test that reimplements this body, which is
// a copy that by construction cannot notice this body changing. That copy
// existed for a while — an engine helper documented as "the stale-edge reaper's
// database half, verbatim" — and it is exactly how a registry wipe nobody
// wanted sat under a green suite.
//
// ── DETECTING IS THE WHOLE JOB. CORE HOLDS WHAT IT HAD ──────────────────────
//
// This pass used to also empty the station's demand_registry, on the reasoning
// that demand signals should stop being routed to a dead station. That
// reasoning was true when it was written and stopped being true on 2026-08-19,
// when the kanban demand-signal path it protected was deleted: the produce leg
// was 100% discarded on arrival and the consume leg had no rows at either
// plant, so nothing has read the registry to route anything since.
//
// What the wipe still did was destroy Core's own record. demand_registry is
// DERIVED BY CORE FROM CORE'S OWN LOADER AGGREGATE — the Edge pushes no claim
// config over the wire — so an Edge going quiet is not evidence that any of it
// changed. Deleting it on the strength of that silence manufactures a config
// withdrawal nobody performed: the monitor's next reconciling pass sees a
// binding that no longer exists and closes the open demand `threshold_removed`,
// while the orders that demand created are still driving robots at the loader.
// A demand ends when a person retires the loader or the level recovers, and a
// flapping link is neither.
//
// So Core keeps the rows and reconciles when the Edge comes back. If the
// aggregate changed while the station was down, the re-derive on register is
// what notices; if it did not, nothing happened and nothing is written.
func (h *CoreHandler) SweepStaleEdges() {
	threshold := h.StaleEdgeThreshold
	if threshold <= 0 {
		threshold = DefaultStaleEdgeThreshold
	}
	staleIDs, err := h.db.MarkStaleEdges(threshold)
	if err != nil {
		log.Printf("core_handler: mark stale edges: %v", err)
		return
	}
	if len(staleIDs) > 0 {
		h.dbg("stale edge check: %d stale", len(staleIDs))
	}
	for _, sid := range staleIDs {
		log.Printf("core_handler: edge %s marked stale, sending notification", sid)
		h.sendStaleNotification(sid)
	}
}

func (h *CoreHandler) sendStaleNotification(stationID string) {
	h.sendData(protocol.SubjectEdgeStale, stationID,
		&protocol.EdgeStale{StationID: stationID, Message: "heartbeat timeout — marked stale by core"})
}
