package messaging

import (
	"log"

	"shingo/protocol"
	"shingocore/store"
)

// InboxDedup is a protocol.MessageHandler decorator that records every
// inbound envelope in the inbox table and drops replays before they
// reach the inner handler. It sits between the protocol ingestor and
// CoreHandler in the composition root:
//
//	ingestor → InboxDedup → CoreHandler → dispatcher
//
// The 8 order-channel methods (Edge → Core) are dedup-gated; HandleData
// and the 7 reply-channel methods (Core → Edge) pass through ungated,
// matching the pre-decorator behaviour where only order messages were
// guarded. See shouldProcess for the inbox contract.
//
// This type replaces the per-handler shouldProcessInbound guard that
// was copy-pasted across eight HandleOrder* methods on CoreHandler.
type InboxDedup struct {
	protocol.NoOpHandler // defensive: new MessageHandler methods compile as no-ops

	inner    protocol.MessageHandler
	db       *store.DB
	DebugLog func(string, ...any)
}

// NewInboxDedup wraps inner with inbox-deduplication for inbound order
// messages. db is the target for RecordInboundMessage writes.
func NewInboxDedup(inner protocol.MessageHandler, db *store.DB) *InboxDedup {
	return &InboxDedup{inner: inner, db: db}
}

func (d *InboxDedup) dbg(format string, args ...any) {
	if fn := d.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// shouldProcess mirrors the former CoreHandler.shouldProcessInbound
// contract exactly:
//   - nil envelope or empty ID: process (no inbox row to write).
//   - RecordInboundMessage error: drop the message and log. The
//     alternative (process on error) would risk double-execution if
//     the inbox write later succeeds on retry.
//   - already-seen: drop silently, dbg-log the replay.
//   - newly recorded: process.
func (d *InboxDedup) shouldProcess(env *protocol.Envelope) bool {
	if env == nil || env.ID == "" {
		return true
	}
	inserted, err := d.db.RecordInboundMessage(env.ID, env.Type, env.Src.Station)
	if err != nil {
		log.Printf("inbox_dedup: record %s: %v", env.ID, err)
		return false
	}
	if !inserted {
		d.dbg("duplicate inbound ignored: id=%s type=%s from=%s", env.ID, env.Type, env.Src.Station)
		return false
	}
	return true
}

// --- Edge → Core: order channel (dedup-gated) --------------------

func (d *InboxDedup) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	if !d.shouldProcess(env) {
		return
	}
	d.inner.HandleOrderRequest(env, p)
}

func (d *InboxDedup) HandleOrderCancel(env *protocol.Envelope, p *protocol.OrderCancel) {
	if !d.shouldProcess(env) {
		return
	}
	d.inner.HandleOrderCancel(env, p)
}

func (d *InboxDedup) HandleOrderReceipt(env *protocol.Envelope, p *protocol.OrderReceipt) {
	if !d.shouldProcess(env) {
		return
	}
	d.inner.HandleOrderReceipt(env, p)
}

func (d *InboxDedup) HandleOrderRedirect(env *protocol.Envelope, p *protocol.OrderRedirect) {
	if !d.shouldProcess(env) {
		return
	}
	d.inner.HandleOrderRedirect(env, p)
}

func (d *InboxDedup) HandleOrderStorageWaybill(env *protocol.Envelope, p *protocol.OrderStorageWaybill) {
	if !d.shouldProcess(env) {
		return
	}
	d.inner.HandleOrderStorageWaybill(env, p)
}

func (d *InboxDedup) HandleComplexOrderRequest(env *protocol.Envelope, p *protocol.ComplexOrderRequest) {
	if !d.shouldProcess(env) {
		return
	}
	d.inner.HandleComplexOrderRequest(env, p)
}

func (d *InboxDedup) HandleOrderRelease(env *protocol.Envelope, p *protocol.OrderRelease) {
	if !d.shouldProcess(env) {
		return
	}
	d.inner.HandleOrderRelease(env, p)
}

func (d *InboxDedup) HandleOrderIngest(env *protocol.Envelope, p *protocol.OrderIngestRequest) {
	if !d.shouldProcess(env) {
		return
	}
	d.inner.HandleOrderIngest(env, p)
}

// --- Data channel (ungated pass-through) -------------------------

// HandleData is not dedup-gated: data messages are subject-routed
// inside CoreDataService and were never guarded pre-decorator. Keeping
// the pre-decorator behaviour is the whole point of this passthrough.
func (d *InboxDedup) HandleData(env *protocol.Envelope, p *protocol.Data) {
	d.inner.HandleData(env, p)
}

// --- Core → Edge: reply channel (ungated pass-through) -----------
//
// These aren't reached in core today (CoreHandler inherits NoOpHandler
// no-ops for them), but overriding them here keeps the decorator a
// transparent wrapper instead of a message-swallower if the inner
// handler ever starts implementing them.

func (d *InboxDedup) HandleOrderAck(env *protocol.Envelope, p *protocol.OrderAck) {
	d.inner.HandleOrderAck(env, p)
}

func (d *InboxDedup) HandleOrderWaybill(env *protocol.Envelope, p *protocol.OrderWaybill) {
	d.inner.HandleOrderWaybill(env, p)
}

func (d *InboxDedup) HandleOrderUpdate(env *protocol.Envelope, p *protocol.OrderUpdate) {
	d.inner.HandleOrderUpdate(env, p)
}

func (d *InboxDedup) HandleOrderDelivered(env *protocol.Envelope, p *protocol.OrderDelivered) {
	d.inner.HandleOrderDelivered(env, p)
}

func (d *InboxDedup) HandleOrderError(env *protocol.Envelope, p *protocol.OrderError) {
	d.inner.HandleOrderError(env, p)
}

func (d *InboxDedup) HandleOrderCancelled(env *protocol.Envelope, p *protocol.OrderCancelled) {
	d.inner.HandleOrderCancelled(env, p)
}

func (d *InboxDedup) HandleOrderStaged(env *protocol.Envelope, p *protocol.OrderStaged) {
	d.inner.HandleOrderStaged(env, p)
}

// Compile-time check that *InboxDedup implements MessageHandler.
var _ protocol.MessageHandler = (*InboxDedup)(nil)
