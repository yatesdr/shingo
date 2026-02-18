package messaging

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RawEnvelope is used for two-stage unmarshalling: first decode the envelope,
// then decode payload based on msg_type.
type RawEnvelope struct {
	MsgType   string          `json:"msg_type"`
	MsgID     string          `json:"msg_id"`
	ClientID  string          `json:"client_id"`
	FactoryID string          `json:"factory_id"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// DecodeEnvelope unmarshals a raw message into a typed Envelope with the correct payload type.
func DecodeEnvelope(data []byte) (*Envelope, error) {
	var raw RawEnvelope
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}

	env := &Envelope{
		MsgType:   raw.MsgType,
		MsgID:     raw.MsgID,
		ClientID:  raw.ClientID,
		FactoryID: raw.FactoryID,
		Timestamp: raw.Timestamp,
	}

	var payload any
	switch raw.MsgType {
	case "order_request":
		var p OrderRequest
		if err := json.Unmarshal(raw.Payload, &p); err != nil {
			return nil, fmt.Errorf("decode order_request payload: %w", err)
		}
		payload = p
	case "order_cancel":
		var p OrderCancel
		if err := json.Unmarshal(raw.Payload, &p); err != nil {
			return nil, fmt.Errorf("decode order_cancel payload: %w", err)
		}
		payload = p
	case "delivery_receipt":
		var p DeliveryReceipt
		if err := json.Unmarshal(raw.Payload, &p); err != nil {
			return nil, fmt.Errorf("decode delivery_receipt payload: %w", err)
		}
		payload = p
	case "redirect_request":
		var p RedirectRequest
		if err := json.Unmarshal(raw.Payload, &p); err != nil {
			return nil, fmt.Errorf("decode redirect_request payload: %w", err)
		}
		payload = p
	default:
		return nil, fmt.Errorf("unknown msg_type: %s", raw.MsgType)
	}
	env.Payload = payload
	return env, nil
}

// NewEnvelope creates an outbound envelope with a new UUID and timestamp.
func NewEnvelope(msgType, clientID, factoryID string, payload any) *Envelope {
	return &Envelope{
		MsgType:   msgType,
		MsgID:     uuid.New().String(),
		ClientID:  clientID,
		FactoryID: factoryID,
		Timestamp: time.Now(),
		Payload:   payload,
	}
}

// Encode marshals an envelope to JSON.
func (e *Envelope) Encode() ([]byte, error) {
	return json.Marshal(e)
}

// DispatchTopic returns the topic for sending to a specific client.
func DispatchTopic(prefix, clientID string) string {
	return prefix + "/" + clientID
}
