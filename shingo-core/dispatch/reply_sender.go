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

func (s *ReplySender) SendUpdate(env *protocol.Envelope, orderUUID, status, detail string) {
	if err := s.SendReply(protocol.TypeOrderUpdate, "order.update", env.Src.Station, env.ID, &protocol.OrderUpdate{
		OrderUUID: orderUUID,
		Status:    status,
		Detail:    detail,
	}); err != nil {
		log.Printf("dispatch: update reply for %s: %v", orderUUID, err)
		return
	}
	if s.debug != nil {
		s.debug("sendUpdate: uuid=%s status=%s", orderUUID, status)
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

// SendStaged re-announces where an order is parked: the station wait and its
// kind. Sent when a release named a different wait (the echo), so the Edge
// re-stages the leg at the wait Core holds instead of leaving it in transit.
func (s *ReplySender) SendStaged(env *protocol.Envelope, orderUUID, detail string, stationWait *int, waitKind string) {
	if err := s.SendReply(protocol.TypeOrderStaged, "order.staged", env.Src.Station, env.ID, &protocol.OrderStaged{
		OrderUUID: orderUUID, Detail: detail, StationWait: stationWait, WaitKind: waitKind,
	}); err != nil {
		log.Printf("dispatch: staged reply for %s: %v", orderUUID, err)
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
