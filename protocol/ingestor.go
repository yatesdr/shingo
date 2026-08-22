package protocol

import (
	"encoding/json"
	"log"
	"sync/atomic"
	"time"

	"shingo/protocol/clock"
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
	// rawPreviewBytes, not 1 KB. This fires for EVERY inbound message, and on the
	// edge that log lands on the Pi's SD card. Node-list and plant-claims payloads
	// run to several KB each, so a handful of them dominated the edge debug log
	// (measured ~22.7 MB/day at Springfield). A short prefix is enough to identify
	// a message and correlate it — size= already gives the true length, and the
	// decoded envelope is logged by the handlers below when it matters.
	ing.dbg("raw: size=%d data=%s", len(data), truncateBytes(data, rawPreviewBytes))

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
		expiredDrops.Add(1)
		log.Printf("protocol: dropping expired message %s (type=%s subject=%s expired %s ago)",
			hdr.ID, hdr.Type, subjectOf(data), clock.Now().UTC().Sub(hdr.ExpiresAt).Round(time.Second))
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

// rawPreviewBytes is how much of an inbound message body HandleRaw previews.
// Deliberately small — see the note at the call site.
const rawPreviewBytes = 160

func truncateBytes(data []byte, maxLen int) string {
	if len(data) == 0 {
		return "<empty>"
	}
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "...(truncated)"
}

// expiredDrops counts envelopes discarded for expiry, for the lifetime of the
// process.
//
// Package-level rather than a field on Ingestor because there is exactly one
// ingestor per process and the health strip needs to read it without a new
// accessor chain through the engine. It is a total, not a rate: the caller
// windows it (see waitsSinceBaseline in shingo-core/www/core_health.go), so a
// long-running Core does not latch a green strip red on history.
//
// It exists because this drop was the larger of the two silent loss channels
// and nothing counted it. Springfield discarded 797 envelopes on 2026-08-20 and
// 544 on 2026-08-21 with no signal anywhere; a number on /status would have
// pointed at the failing WiFi weeks before anyone went looking.
var expiredDrops atomic.Int64

// ExpiredDrops reports how many envelopes this process has dropped for expiry.
func ExpiredDrops() int64 { return expiredDrops.Load() }

// subjectOf digs the data-channel subject out of a raw envelope.
//
// The subject is the only field that says whether a dropped message mattered —
// a lineside snapshot is superseded in 60s, a bin_uop_delta is a permanently
// wrong count — and it is NOT on RawHeader: it lives inside the payload, which
// the two-phase decode deliberately does not touch until after the expiry and
// filter checks.
//
// So this is a second unmarshal, and it runs ONLY on the drop branch. Adding
// the payload to RawHeader would have put a json.RawMessage allocation on every
// message to serve a path that fires a few hundred times a day. Returns "" for
// a non-data envelope or an undecodable payload; the caller logs it either way.
func subjectOf(raw []byte) string {
	var probe struct {
		P struct {
			Subject string `json:"subject"`
		} `json:"p"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.P.Subject
}
