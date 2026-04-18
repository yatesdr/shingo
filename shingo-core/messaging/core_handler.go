package messaging

import (
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingocore/store"
)

// CoreHandler handles inbound protocol messages on the orders topic.
// It processes registration and heartbeat messages directly, and
// delegates order messages to the dispatcher.
//
// dispatcher is the narrow consumer-side Dispatcher interface (see
// dispatcher.go in this package). *dispatch.Dispatcher satisfies it
// structurally, so engine wiring is unchanged; the indirection is
// what lets core_handler_test.go stub a fake.
type CoreHandler struct {
	protocol.NoOpHandler

	db            *store.DB
	client        *Client
	stationID     string
	dispatchTopic string
	dispatcher    Dispatcher
	dataService   *CoreDataService
	DebugLog      func(string, ...any)

	// Background goroutine for stale edge detection
	stopOnce sync.Once
	stopCh   chan struct{}
}

// NewCoreHandler creates a handler for inbound edge messages.
//
// dispatcher is accepted as the narrow Dispatcher interface. Callers
// (engine) pass the concrete *dispatch.Dispatcher; structural typing
// handles the rest.
func NewCoreHandler(db *store.DB, client *Client, stationID, dispatchTopic string, dispatcher Dispatcher) *CoreHandler {
	h := &CoreHandler{
		db:            db,
		client:        client,
		stationID:     stationID,
		dispatchTopic: dispatchTopic,
		dispatcher:    dispatcher,
		stopCh:        make(chan struct{}),
	}
	h.dataService = newCoreDataService(db, h)
	return h
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

func (h *CoreHandler) HandleData(env *protocol.Envelope, p *protocol.Data) {
	h.dataService.Handle(env, p)
}

// Order message handlers delegate to the dispatcher. Inbox
// deduplication is performed by the InboxDedup decorator in the
// composition root — these methods assume the envelope has already
// cleared the dedup guard.

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

func (h *CoreHandler) HandleOrderStorageWaybill(env *protocol.Envelope, p *protocol.OrderStorageWaybill) {
	log.Printf("core_handler: storage waybill from %s: uuid=%s", env.Src.Station, p.OrderUUID)
	h.dbg("-> storage_waybill from=%s uuid=%s", env.Src.Station, p.OrderUUID)
	h.dispatcher.HandleOrderStorageWaybill(env, p)
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
			staleIDs, err := h.db.MarkStaleEdges(180 * time.Second)
			if err != nil {
				log.Printf("core_handler: mark stale edges: %v", err)
				continue
			}
			if len(staleIDs) > 0 {
				h.dbg("stale edge check: %d stale", len(staleIDs))
			}
			for _, sid := range staleIDs {
				log.Printf("core_handler: edge %s marked stale, sending notification", sid)
				h.sendStaleNotification(sid)
			}
		}
	}
}

func (h *CoreHandler) sendStaleNotification(stationID string) {
	h.sendData(protocol.SubjectEdgeStale, stationID,
		&protocol.EdgeStale{StationID: stationID, Message: "heartbeat timeout — marked stale by core"})
}
