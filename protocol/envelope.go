package protocol

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Address identifies a message source or destination.
type Address struct {
	Role    string `json:"role"`
	Station string `json:"station"`
}

// Envelope is the universal message wrapper for all ShinGo communication.
type Envelope struct {
	Version   int              `json:"v"`
	Type      string           `json:"type"`
	ID        string           `json:"id"`
	Src       Address          `json:"src"`
	Dst       Address          `json:"dst"`
	Timestamp time.Time        `json:"ts"`
	ExpiresAt time.Time        `json:"exp"`
	CorID     string           `json:"cor,omitempty"`
	Payload   json.RawMessage  `json:"p"`
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

	now := time.Now().UTC()
	exp := now.Add(DefaultTTLFor(msgType))

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

	now := time.Now().UTC()
	exp := now.Add(DataTTLFor(subject))

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
