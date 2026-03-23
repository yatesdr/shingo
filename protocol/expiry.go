package protocol

import "time"

// Default TTLs by message type.
var defaultTTLs = map[string]time.Duration{
	TypeData: 5 * time.Minute,

	TypeOrderRequest:        10 * time.Minute,
	TypeOrderCancel:         10 * time.Minute,
	TypeOrderRedirect:       10 * time.Minute,
	TypeOrderStorageWaybill: 10 * time.Minute,
	TypeOrderIngest:         10 * time.Minute,

	TypeOrderAck:    10 * time.Minute,
	TypeOrderUpdate: 10 * time.Minute,

	TypeOrderReceipt:   30 * time.Minute,
	TypeOrderWaybill:   30 * time.Minute,
	TypeOrderError:     30 * time.Minute,
	TypeOrderCancelled: 30 * time.Minute,

	TypeOrderDelivered: 60 * time.Minute,
}

// Subject-specific TTLs for data channel messages.
var subjectTTLs = map[string]time.Duration{
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
	SubjectNodeStateRequest:    30 * time.Second,
	SubjectNodeStateResponse:   30 * time.Second,
}

// FallbackTTL is used when no specific TTL is configured.
const FallbackTTL = 10 * time.Minute

// DataTTLFor returns the TTL for a data channel subject.
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

// IsExpired returns true if the envelope has passed its expiry time.
func IsExpired(env *Envelope) bool {
	if env.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().UTC().After(env.ExpiresAt)
}

// IsExpiredHeader checks expiry using only the raw header.
func IsExpiredHeader(hdr *RawHeader) bool {
	if hdr.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().UTC().After(hdr.ExpiresAt)
}
