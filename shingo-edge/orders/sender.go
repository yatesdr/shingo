package orders

import (
	"encoding/json"
	"fmt"

	"shingo/protocol"
	"shingoedge/store"
)

type OrderSender struct {
	db        *store.DB
	stationID string
}

func newOrderSender(db *store.DB, stationID string) *OrderSender {
	return &OrderSender{db: db, stationID: stationID}
}

func (s *OrderSender) src() protocol.Address {
	return protocol.Address{Role: protocol.RoleEdge, Station: s.stationID}
}

func (s *OrderSender) dst() protocol.Address {
	return protocol.Address{Role: protocol.RoleCore}
}

func (s *OrderSender) build(msgType string, payload any) (*protocol.Envelope, error) {
	return protocol.NewEnvelope(msgType, s.src(), s.dst(), payload)
}

func (s *OrderSender) enqueue(env *protocol.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if _, err := s.db.EnqueueOutbox(data, env.Type); err != nil {
		return fmt.Errorf("enqueue %s: %w", env.Type, err)
	}
	return nil
}

func (s *OrderSender) Queue(msgType string, payload any) error {
	env, err := s.build(msgType, payload)
	if err != nil {
		return err
	}
	return s.enqueue(env)
}
