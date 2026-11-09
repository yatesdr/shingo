package protocol

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"shingo/protocol/clock"
)

// Address identifies a message source or destination.
type Address struct {
	// Role is the sender or receiver class: RoleEdge or RoleCore.
	Role string `json:"role"`
	// Station is the station identity. For an edge, its station UID, minted
	// by Core at enrollment. For Core, its configured station id. On a
	// dispatch to every edge, StationBroadcast ("*"). Empty is valid on a
	// message to Core, which is a singleton.
	Station string `json:"station"`
}

// Envelope is the universal message wrapper for all ShinGo communication.
type Envelope struct {
	// Version is the protocol version (Version). A consumer rejects an
	// envelope whose version it does not know.
	Version int `json:"v"`
	// Type is the message type (a Type constant); it selects the schema of
	// Payload.
	Type string `json:"type"`
	// ID is the message's unique id: a UUID v4, lowercase hex with hyphens.
	ID string `json:"id"`
	// Src is the sender.
	Src Address `json:"src"`
	// Dst is the intended recipient.
	Dst Address `json:"dst"`
	// Timestamp is when the message was created, RFC 3339 in UTC.
	Timestamp time.Time `json:"ts"`
	// ExpiresAt is the absolute time after which a receiver drops the message
	// unprocessed, RFC 3339 in UTC. The zero time ("0001-01-01T00:00:00Z") is
	// no expiry (NoExpiry), never "expired".
	ExpiresAt time.Time `json:"exp"`
	// CorID links a reply to the message it answers, by that message's ID
	// (an order.ack's cor is its order.request's id). Omitted when empty.
	CorID string `json:"cor,omitempty"`
	// Payload is the type's body, always a JSON object. It stays raw bytes
	// through the header decode and is decoded only once routing has chosen a
	// handler.
	Payload json.RawMessage `json:"p"`
}

// RawHeader is the minimal decode for routing decisions before full payload
// decode: the envelope without its timestamp, correlation id and payload.
type RawHeader struct {
	Version   int       `json:"v"`    // as Envelope.Version
	Type      string    `json:"type"` // as Envelope.Type
	ID        string    `json:"id"`   // as Envelope.ID
	Src       Address   `json:"src"`  // as Envelope.Src
	Dst       Address   `json:"dst"`  // as Envelope.Dst
	ExpiresAt time.Time `json:"exp"`  // as Envelope.ExpiresAt; checked before any payload byte is read
}

// NewEnvelope creates an outbound envelope with default TTL.
func NewEnvelope(msgType string, src, dst Address, payload any) (*Envelope, error) {
	p, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	now := clock.Now().UTC()
	// Scale the TTL into the stamping clock domain so the expiry window is a
	// constant REAL-time budget even under fast-forward (clock.ScaleTTL is a
	// no-op in production / at 1×). Without this, lifecycle messages expire in
	// the pipeline at high speed and strand orders. See clock.ScaleTTL.
	// A zero TTL means NO expiry, not "expires now" — see NoExpiry. now.Add(0)
	// would stamp exp = now and expire on the next tick. Several types and
	// subjects use NoExpiry (protocol/expiry.go); the guard is what keeps them
	// from expiring on the next tick.
	exp := zeroOrDeadline(now, clock.ScaleTTL(DefaultTTLFor(msgType)))

	return &Envelope{
		Version:   Version,
		Type:      msgType,
		ID:        uuid.New().String(),
		Src:       src,
		Dst:       dst,
		Timestamp: now,
		ExpiresAt: exp,
		Payload:   p,
	}, nil
}

// zeroOrDeadline returns a zero time for a NoExpiry TTL and now+ttl otherwise.
//
// One function so both stamping sites cannot drift: the trap is that the
// natural expression, now.Add(ttl), turns "no expiry" into "already expired".
func zeroOrDeadline(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

// NewReply creates a reply envelope, setting CorID to the original message ID.
func NewReply(msgType string, src, dst Address, correlationID string, payload any) (*Envelope, error) {
	env, err := NewEnvelope(msgType, src, dst, payload)
	if err != nil {
		return nil, err
	}
	env.CorID = correlationID
	return env, nil
}

// NewDataEnvelope creates a data-channel envelope with subject-specific TTL.
func NewDataEnvelope(subject string, src, dst Address, body any) (*Envelope, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	p, err := json.Marshal(&Data{Subject: subject, Body: bodyBytes})
	if err != nil {
		return nil, err
	}

	now := clock.Now().UTC()
	// Scale into the stamping clock domain (no-op in production / at 1×) so the
	// expiry window is a constant real-time budget under fast-forward — see
	// clock.ScaleTTL.
	//
	// The comment here used to say telemetry deltas "get the same treatment"
	// because an expired delta is a lost production count. Scaling was never
	// enough: at Springfield the 5-minute budget was simply exceeded by the
	// transport, and ~17 deltas a day were dropped at Core's ingestor. Those
	// subjects now carry NoExpiry and leave ExpiresAt zero.
	exp := zeroOrDeadline(now, clock.ScaleTTL(DataTTLFor(subject)))

	return &Envelope{
		Version:   Version,
		Type:      TypeData,
		ID:        uuid.New().String(),
		Src:       src,
		Dst:       dst,
		Timestamp: now,
		ExpiresAt: exp,
		Payload:   p,
	}, nil
}

// NewDataReply creates a data-channel reply envelope with correlation ID.
func NewDataReply(subject string, src, dst Address, correlationID string, body any) (*Envelope, error) {
	env, err := NewDataEnvelope(subject, src, dst, body)
	if err != nil {
		return nil, err
	}
	env.CorID = correlationID
	return env, nil
}

// Encode marshals the envelope to JSON.
func (e *Envelope) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DecodePayload unmarshals the raw payload into the given target.
func (e *Envelope) DecodePayload(target any) error {
	return json.Unmarshal(e.Payload, target)
}
