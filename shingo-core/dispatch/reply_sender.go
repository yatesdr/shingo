package dispatch

import (
	"fmt"
	"log"
	"sync"

	"shingo/protocol"
	"shingocore/store"
)

type ReplySender struct {
	db    *store.DB
	topic string
	src   protocol.Address
	debug func(string, ...any)

	// watched collects the order.error replies sent for an envelope an
	// in-process caller is waiting on (see Dispatcher.RefusalsFor). Keyed by
	// envelope ID, the reply's correlation ID. Nothing on the wire changes.
	watchMu sync.Mutex
	watched map[string]*[]protocol.OrderError
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
	s.noteError(env, protocol.OrderError{OrderUUID: orderUUID, ErrorCode: errorCode, Detail: detail})
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

func (s *ReplySender) noteError(env *protocol.Envelope, e protocol.OrderError) {
	if env == nil || env.ID == "" {
		return
	}
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if list, ok := s.watched[env.ID]; ok {
		*list = append(*list, e)
	}
}

// RefusalsFor runs fn and returns the order.error replies sent for env while it
// ran. For in-process callers (the Test Orders page) that call a handler
// directly and must answer with what the wire would have carried. The replies
// are still sent as before.
func (d *Dispatcher) RefusalsFor(env *protocol.Envelope, fn func()) []protocol.OrderError {
	s := d.replies
	var list []protocol.OrderError
	if env != nil && env.ID != "" {
		s.watchMu.Lock()
		if s.watched == nil {
			s.watched = map[string]*[]protocol.OrderError{}
		}
		s.watched[env.ID] = &list
		s.watchMu.Unlock()
		defer func() {
			s.watchMu.Lock()
			delete(s.watched, env.ID)
			s.watchMu.Unlock()
		}()
	}
	fn()
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	return append([]protocol.OrderError(nil), list...)
}
