package protocol

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"shingo/protocol/clock"
)

// Address identifies a message source or destination.
type Address struct {
	Role    string `json:"role"`
	Station string `json:"station"`
}

// Envelope is the universal message wrapper for all ShinGo communication.
type Envelope struct {
	Version   int             `json:"v"`
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Src       Address         `json:"src"`
	Dst       Address         `json:"dst"`
	Timestamp time.Time       `json:"ts"`
	ExpiresAt time.Time       `json:"exp"`
	CorID     string          `json:"cor,omitempty"`
	Payload   json.RawMessage `json:"p"`
}

// RawHeader is the minimal decode for routing decisions before full payload decode.
type RawHeader struct {
	Version   int       `json:"v"`
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Src       Address   `json:"src"`
	Dst       Address   `json:"dst"`
	ExpiresAt time.Time `json:"exp"`
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
	// would stamp exp = now and expire on the next tick. No msgType currently
	// uses NoExpiry; the guard is here so adding one cannot silently invert.
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
