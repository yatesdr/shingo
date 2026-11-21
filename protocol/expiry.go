package protocol

import (
	"time"

	"shingo/protocol/clock"
)

// Default TTLs by message type. A type that never expires is a row of the
// must-arrive table (must_arrive.go), which adds it here.
var defaultTTLs = withNeverExpires(map[string]time.Duration{
	TypeData: 5 * time.Minute,

	TypeOrderRequest:  10 * time.Minute,
	TypeOrderRedirect: 10 * time.Minute,

	TypeOrderAck:    10 * time.Minute,
	TypeOrderUpdate: 10 * time.Minute,

	TypeOrderReceipt:   30 * time.Minute,
	TypeOrderWaybill:   30 * time.Minute,
	TypeOrderError:     30 * time.Minute,
	TypeOrderSkipped:   30 * time.Minute,
	TypeOrderCancelled: 30 * time.Minute,

	TypeOrderDelivered: 60 * time.Minute,

	// The ephemeral topic's two (ephemeral.go): worth reading for seconds.
	TypeRobotWatch:     EphemeralTTL,
	TypeRobotPositions: EphemeralTTL,
}, true)

// NoExpiry marks a subject whose envelopes carry NO `exp` on the wire.
//
// It is not "expire immediately". NewDataEnvelope leaves ExpiresAt zero for
// these, and IsExpired/IsExpiredHeader both treat a zero ExpiresAt as never
// expiring. The distinction matters because `now.Add(0)` — what a zero TTL used
// to produce — stamps exp = now, which expires on the very next clock tick and
// is the exact opposite of the intent.
//
// Reserve it for messages where a DROPPED copy costs more than a LATE one, and
// give it only through a row of the must-arrive table (must_arrive.go), which
// says what makes a late copy safe at its receiver.
const NoExpiry time.Duration = 0

// Subject-specific TTLs for data channel messages. A subject that never
// expires is a row of the must-arrive table (must_arrive.go), which adds it
// here.
var subjectTTLs = withNeverExpires(map[string]time.Duration{
	// The heartbeat and its ack carry the feed digests, and the ack drives the
	// Edge's reconcile after a gap. Both are stamped on one clock and checked
	// on the other, so the TTL is also the clock skew a station tolerates: at
	// 90 s a Pi clock off by 90 s silently lost its acks or its heartbeats.
	// Five minutes is the rest of the data channel's TTL.
	SubjectEdgeHeartbeat:    5 * time.Minute,
	SubjectEdgeHeartbeatAck: 5 * time.Minute,
	SubjectEdgeRegister:     5 * time.Minute,
	SubjectEdgeRegistered:   5 * time.Minute,

	SubjectEdgeStale:           5 * time.Minute,
	SubjectEdgeRegisterRequest: 5 * time.Minute,
	SubjectNodeListRequest:     5 * time.Minute,
	SubjectNodeListResponse:    5 * time.Minute,
	SubjectOrderStatusRequest:  5 * time.Minute,
	SubjectOrderStatusResponse: 5 * time.Minute,

	// The cell catalog is a whole snapshot that the heartbeat ack heals: a copy
	// lost or expired shows as a differing cells digest on the next ack, which
	// sends the catalog again. So it takes the data channel's TTL and is not
	// must-arrive.
	SubjectEdgeCells: 5 * time.Minute,

	// The ask pair (ask.go): an ask stays answerable for AskTTL, and Core
	// answers one that arrives late instead of the ingestor dropping it
	// (Ingestor.OnExpired); the answer never expires (must_arrive.go).
	SubjectEdgeAsk: AskTTL,
}, false)

// FallbackTTL is used when no specific TTL is configured.
const FallbackTTL = 10 * time.Minute

// DataTTLFor returns the TTL for a data channel subject. A return of NoExpiry
// means the envelope carries no expiry at all — see NoExpiry.
func DataTTLFor(subject string) time.Duration {
	if ttl, ok := subjectTTLs[subject]; ok {
		return ttl
	}
	return defaultTTLs[TypeData]
}

// DefaultTTLFor returns the default TTL for a message type.
func DefaultTTLFor(msgType string) time.Duration {
	if ttl, ok := defaultTTLs[msgType]; ok {
		return ttl
	}
	return FallbackTTL
}

// IsExpired returns true if the envelope has passed its expiry time. It uses the
// injected clock (clock.Now) rather than time.Now so that under sim fast-forward —
// where envelopes are stamped in back-dated sim time — the consumer compares like
// for like. In production clock.Now() IS time.Now(), so behavior is unchanged.
// (NewEnvelope already stamps exp from the sim clock; this is the missing dual.)
func IsExpired(env *Envelope) bool {
	if env.ExpiresAt.IsZero() {
		return false
	}
	return clock.Now().UTC().After(env.ExpiresAt)
}

// IsExpiredHeader checks expiry using only the raw header. Same sim-clock rationale
// as IsExpired.
func IsExpiredHeader(hdr *RawHeader) bool {
	if hdr.ExpiresAt.IsZero() {
		return false
	}
	return clock.Now().UTC().After(hdr.ExpiresAt)
}
