package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store"
)

type ReplySender struct {
	db    *store.DB
	topic string
	src   protocol.Address
	debug func(string, ...any)
}

func newReplySender(db *store.DB, topic, stationID string, debug func(string, ...any)) *ReplySender {
	return &ReplySender{
		db:    db,
		topic: topic,
		src:   protocol.Address{Role: protocol.RoleCore, Station: stationID},
		debug: debug,
	}
}

func (s *ReplySender) CoreAddress() protocol.Address {
	return s.src
}

func (s *ReplySender) SendReply(msgType, eventType, stationID, correlationID string, payload any) error {
	dst := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	reply, err := protocol.NewReply(msgType, s.src, dst, correlationID, payload)
	if err != nil {
		return fmt.Errorf("build %s reply: %w", msgType, err)
	}
	data, err := reply.Encode()
	if err != nil {
		return fmt.Errorf("encode %s reply: %w", msgType, err)
	}
	if err := s.db.EnqueueOutbox(s.topic, data, eventType, stationID); err != nil {
		return fmt.Errorf("enqueue %s reply: %w", msgType, err)
	}
	return nil
}

func (s *ReplySender) SendAck(env *protocol.Envelope, orderUUID string, shingoOrderID int64, sourceNode string) {
	if err := s.SendReply(protocol.TypeOrderAck, "order.ack", env.Src.Station, env.ID, &protocol.OrderAck{
		OrderUUID:     orderUUID,
		ShingoOrderID: shingoOrderID,
		SourceNode:    sourceNode,
	}); err != nil {
		log.Printf("dispatch: ack reply for %s: %v", orderUUID, err)
		return
	}
	if s.debug != nil {
		s.debug("sendAck: uuid=%s shingo_id=%d source=%s", orderUUID, shingoOrderID, sourceNode)
	}
}

func (s *ReplySender) SendError(env *protocol.Envelope, orderUUID, errorCode, detail string) {
	if err := s.SendReply(protocol.TypeOrderError, "order.error", env.Src.Station, env.ID, &protocol.OrderError{
		OrderUUID: orderUUID,
		ErrorCode: errorCode,
		Detail:    detail,
	}); err != nil {
		log.Printf("dispatch: error reply for %s: %v", orderUUID, err)
		return
	}
	if s.debug != nil {
		s.debug("sendError: uuid=%s code=%s detail=%s", orderUUID, errorCode, detail)
	}
}

func (s *ReplySender) SendCancelled(env *protocol.Envelope, orderUUID, reason string) {
	if err := s.SendReply(protocol.TypeOrderCancelled, "order.cancelled", env.Src.Station, env.ID, &protocol.OrderCancelled{
		OrderUUID: orderUUID,
		Reason:    reason,
	}); err != nil {
		log.Printf("dispatch: cancelled reply for %s: %v", orderUUID, err)
	}
}
