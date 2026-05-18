package protocol

import (
	"encoding/json"
	"log"
)

// FilterFunc returns true if the message should be processed.
type FilterFunc func(hdr *RawHeader) bool

// Ingestor performs two-phase decode (header gate → full envelope) and
// hands successfully-decoded envelopes to the registered Dispatch
// callback. Composition roots wire Dispatch to a router.Router's
// Dispatch method; the router is the sole dispatcher.
type Ingestor struct {
	filter     FilterFunc
	SigningKey []byte // optional HMAC-SHA256 key; when set, unsigned messages are rejected
	DebugLog   func(string, ...any)

	// Dispatch is invoked once per successfully-decoded envelope.
	// When nil, the envelope is parsed but not dispatched — useful for
	// tests that exercise only the decode / filter / expiry / signing
	// gates. Production composition roots wire it to a protocol/router
	// dispatch call. Field (rather than a router interface) avoids an
	// import cycle between protocol and protocol/router.
	Dispatch func(env *Envelope)
}

// NewIngestor creates an ingestor with the given filter. Wire
// Dispatch (e.g., to a *router.Router.Dispatch) before calling
// HandleRaw on live traffic; a nil Dispatch decodes and drops.
func NewIngestor(filter FilterFunc) *Ingestor {
	return &Ingestor{
		filter: filter,
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

	// Dispatch via the router hook (set by composition roots in
	// cmd/*/main.go). When the hook isn't wired the envelope is decoded
	// but not dispatched — useful for tests that only exercise the
	// decode/filter/expiry/signing paths.
	if ing.Dispatch != nil {
		ing.Dispatch(&env)
	}
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
