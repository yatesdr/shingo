package protocol

import (
	"encoding/json"
	"log"
)

// FilterFunc returns true if the message should be processed.
type FilterFunc func(hdr *RawHeader) bool

// MessageHandler defines callbacks for all protocol message types.
// Embed NoOpHandler and override only the methods you need.
type MessageHandler interface {
	// Generic data channel
	HandleData(env *Envelope, p *Data)

	// Edge -> Core
	HandleOrderRequest(env *Envelope, p *OrderRequest)
	HandleOrderCancel(env *Envelope, p *OrderCancel)
	HandleOrderReceipt(env *Envelope, p *OrderReceipt)
	HandleOrderRedirect(env *Envelope, p *OrderRedirect)
	HandleOrderStorageWaybill(env *Envelope, p *OrderStorageWaybill)

	// Edge -> Core: complex order lifecycle
	HandleComplexOrderRequest(env *Envelope, p *ComplexOrderRequest)
	HandleOrderRelease(env *Envelope, p *OrderRelease)

	// Edge -> Core: origination
	HandleOrderIngest(env *Envelope, p *OrderIngestRequest)

	// Core -> Edge
	HandleOrderAck(env *Envelope, p *OrderAck)
	HandleOrderWaybill(env *Envelope, p *OrderWaybill)
	HandleOrderUpdate(env *Envelope, p *OrderUpdate)
	HandleOrderDelivered(env *Envelope, p *OrderDelivered)
	HandleOrderError(env *Envelope, p *OrderError)
	HandleOrderCancelled(env *Envelope, p *OrderCancelled)
	HandleOrderStaged(env *Envelope, p *OrderStaged)
}

// Ingestor performs two-phase decode and dispatches to a MessageHandler.
type Ingestor struct {
	handler    MessageHandler
	filter     FilterFunc
	SigningKey []byte // optional HMAC-SHA256 key; when set, unsigned messages are rejected
	DebugLog   func(string, ...any)
}

// NewIngestor creates an ingestor with the given handler and filter.
func NewIngestor(handler MessageHandler, filter FilterFunc) *Ingestor {
	return &Ingestor{
		handler: handler,
		filter:  filter,
	}
}

func (ing *Ingestor) dbg(format string, args ...any) {
	if fn := ing.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// HandleRaw is the entry point for raw message bytes from the messaging layer.
func (ing *Ingestor) HandleRaw(data []byte) {
	ing.dbg("raw: size=%d data=%s", len(data), truncateBytes(data, 1024))

	// Verify signature if signing is enabled
	inner, err := VerifyAndUnwrap(data, ing.SigningKey)
	if err != nil {
		log.Printf("protocol: dropping message with invalid signature")
		ing.dbg("signature verification failed: %v", err)
		return
	}
	data = inner

	// Phase 1: decode routing header only
	var hdr RawHeader
	if err := json.Unmarshal(data, &hdr); err != nil {
		log.Printf("protocol: header decode error: %v", err)
		ing.dbg("header decode error: %v", err)
		return
	}

	ing.dbg("header: type=%s id=%s dst=%s/%s", hdr.Type, hdr.ID, hdr.Dst.Role, hdr.Dst.Station)

	// Check expiry
	if IsExpiredHeader(&hdr) {
		log.Printf("protocol: dropping expired message %s (type=%s)", hdr.ID, hdr.Type)
		return
	}

	// Apply filter
	if ing.filter != nil && !ing.filter(&hdr) {
		return
	}

	// Phase 2: full envelope decode
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		log.Printf("protocol: envelope decode error: %v", err)
		ing.dbg("envelope decode error: %v", err)
		return
	}

	// Dispatch by type
	ing.dbg("dispatch: type=%s id=%s", env.Type, env.ID)
	switch env.Type {
	case TypeData:
		decodeAndCall(ing.handler.HandleData, &env, ing.dbg)
	case TypeOrderRequest:
		decodeAndCall(ing.handler.HandleOrderRequest, &env, ing.dbg)
	case TypeOrderCancel:
		decodeAndCall(ing.handler.HandleOrderCancel, &env, ing.dbg)
	case TypeOrderReceipt:
		decodeAndCall(ing.handler.HandleOrderReceipt, &env, ing.dbg)
	case TypeOrderRedirect:
		decodeAndCall(ing.handler.HandleOrderRedirect, &env, ing.dbg)
	case TypeOrderStorageWaybill:
		decodeAndCall(ing.handler.HandleOrderStorageWaybill, &env, ing.dbg)
	case TypeComplexOrderRequest:
		decodeAndCall(ing.handler.HandleComplexOrderRequest, &env, ing.dbg)
	case TypeOrderRelease:
		decodeAndCall(ing.handler.HandleOrderRelease, &env, ing.dbg)
	case TypeOrderIngest:
		decodeAndCall(ing.handler.HandleOrderIngest, &env, ing.dbg)
	case TypeOrderAck:
		decodeAndCall(ing.handler.HandleOrderAck, &env, ing.dbg)
	case TypeOrderWaybill:
		decodeAndCall(ing.handler.HandleOrderWaybill, &env, ing.dbg)
	case TypeOrderUpdate:
		decodeAndCall(ing.handler.HandleOrderUpdate, &env, ing.dbg)
	case TypeOrderDelivered:
		decodeAndCall(ing.handler.HandleOrderDelivered, &env, ing.dbg)
	case TypeOrderError:
		decodeAndCall(ing.handler.HandleOrderError, &env, ing.dbg)
	case TypeOrderCancelled:
		decodeAndCall(ing.handler.HandleOrderCancelled, &env, ing.dbg)
	case TypeOrderStaged:
		decodeAndCall(ing.handler.HandleOrderStaged, &env, ing.dbg)
	default:
		log.Printf("protocol: unknown message type: %s", env.Type)
	}
}

// decodeAndCall unmarshals the payload and calls the handler method.
func decodeAndCall[T any](fn func(*Envelope, *T), env *Envelope, dbg func(string, ...any)) {
	var p T
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		log.Printf("protocol: payload decode error for %s: %v", env.Type, err)
		if dbg != nil {
			dbg("payload decode error: type=%s error=%v", env.Type, err)
		}
		return
	}
	fn(env, &p)
}

func truncateBytes(data []byte, maxLen int) string {
	if len(data) == 0 {
		return "<empty>"
	}
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "...(truncated)"
}
