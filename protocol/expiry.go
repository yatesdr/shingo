package protocol

import (
	"time"

	"shingo/protocol/clock"
)

// Default TTLs by message type.
var defaultTTLs = map[string]time.Duration{
	TypeData: 5 * time.Minute,

	TypeOrderRequest:  10 * time.Minute,
	TypeOrderCancel:   10 * time.Minute,
	TypeOrderRedirect: 10 * time.Minute,
	TypeOrderIngest:   10 * time.Minute,

	TypeOrderAck:    10 * time.Minute,
	TypeOrderUpdate: 10 * time.Minute,

	TypeOrderReceipt:   30 * time.Minute,
	TypeOrderWaybill:   30 * time.Minute,
	TypeOrderError:     30 * time.Minute,
	TypeOrderSkipped:   30 * time.Minute,
	TypeOrderCancelled: 30 * time.Minute,

	TypeOrderDelivered: 60 * time.Minute,
}

// NoExpiry marks a subject whose envelopes carry NO `exp` on the wire.
//
// It is not "expire immediately". NewDataEnvelope leaves ExpiresAt zero for
// these, and IsExpired/IsExpiredHeader both treat a zero ExpiresAt as never
// expiring. The distinction matters because `now.Add(0)` — what a zero TTL used
// to produce — stamps exp = now, which expires on the very next clock tick and
// is the exact opposite of the intent.
//
// Reserve it for subjects where a DROPPED copy costs more than a LATE one.
// That is a property of the receiving handler, not of the sender, so do not add
// a subject here without checking what the receiving side's handler does with
// a copy that arrives after newer ones (its dedup key and ordering guard).
const NoExpiry time.Duration = 0

// Subject-specific TTLs for data channel messages.
var subjectTTLs = map[string]time.Duration{
	// The two sequenced inventory deltas carry information nothing else
	// resupplies. Most other data subjects are snapshots whose successor
	// carries the same truth a few seconds later, so dropping a late copy costs
	// nothing; these are increments, and a dropped one is a permanently wrong
	// count that never self-corrects.
	//
	// A late copy is not always harmless. Core dedups on a per-scope high-water
	// SequenceID, so a replay is a no-op, but a delta that arrives after a
	// higher seq of its scope is skipped with no ledger row; and a delta from a
	// superseded epoch goes to the discrepancy audit, not the count. Expiring
	// them would lose more: a dropped delta is lost with no record at all.
	//
	// Measured at Springfield 2026-08-21, before the edge was hardwired: ~17
	// bin_uop_delta a day arrived past the 5-minute default and were discarded
	// by the ingestor before any handler ran, averaging 142 minutes late and
	// peaking at 23 hours. The edge marked every one of them sent.
	SubjectBinUOPDelta:         NoExpiry,
	SubjectLinesideBucketDelta: NoExpiry,

	// Core's count announcements to the Edge. One dropped after an outage
	// longer than TypeData's 5 minutes leaves the station holding a carrier
	// under a generation that has ended, so every count it reports is
	// discarded at Core. A late copy cannot walk a carrier's epoch backward:
	// every Edge write they reach goes through the store's epochAssignOnBind,
	// which moves a bound carrier's stamp forward only, and a bind into an
	// empty slot is refused for the carrier that last left it at an older
	// stamp (BindEmptySlotUnlessDeparted). What a late UOPAdjustment still
	// does is write its count as sent, over any ticks the Edge counted after
	// Core sent it.
	SubjectUOPAdjustment:   NoExpiry,
	SubjectBinEpochRefresh: NoExpiry,

	SubjectEdgeHeartbeat:    90 * time.Second,
	SubjectEdgeHeartbeatAck: 90 * time.Second,
	SubjectEdgeRegister:     5 * time.Minute,
	SubjectEdgeRegistered:   5 * time.Minute,

	SubjectProductionReport:    5 * time.Minute,
	SubjectProductionReportAck: 5 * time.Minute,
	SubjectEdgeStale:           5 * time.Minute,
	SubjectEdgeRegisterRequest: 5 * time.Minute,
	SubjectNodeListRequest:     5 * time.Minute,
	SubjectNodeListResponse:    5 * time.Minute,
	SubjectOrderStatusRequest:  5 * time.Minute,
	SubjectOrderStatusResponse: 5 * time.Minute,
}

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
