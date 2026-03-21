package dispatch

import (
	"log"

	"shingo/protocol"
)

func (d *Dispatcher) coreAddress() protocol.Address {
	return protocol.Address{Role: protocol.RoleCore, Station: d.stationID}
}

func (d *Dispatcher) sendAck(env *protocol.Envelope, orderUUID string, shingoOrderID int64, sourceNode string) {
	stationID := env.Src.Station
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	reply, err := protocol.NewReply(protocol.TypeOrderAck, d.coreAddress(), edgeAddr, env.ID, &protocol.OrderAck{
		OrderUUID:     orderUUID,
		ShingoOrderID: shingoOrderID,
		SourceNode:    sourceNode,
	})
	if err != nil {
		log.Printf("dispatch: build ack reply: %v", err)
		return
	}
	data, err := reply.Encode()
	if err != nil {
		log.Printf("dispatch: encode ack reply: %v", err)
		return
	}
	d.dbg("sendAck: uuid=%s shingo_id=%d source=%s", orderUUID, shingoOrderID, sourceNode)
	if err := d.db.EnqueueOutbox(d.dispatchTopic, data, "order.ack", stationID); err != nil {
		log.Printf("dispatch: enqueue ack for %s: %v", orderUUID, err)
	}
}

func (d *Dispatcher) sendError(env *protocol.Envelope, orderUUID, errorCode, detail string) {
	stationID := env.Src.Station
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	reply, err := protocol.NewReply(protocol.TypeOrderError, d.coreAddress(), edgeAddr, env.ID, &protocol.OrderError{
		OrderUUID: orderUUID,
		ErrorCode: errorCode,
		Detail:    detail,
	})
	if err != nil {
		log.Printf("dispatch: build error reply: %v", err)
		return
	}
	data, err := reply.Encode()
	if err != nil {
		log.Printf("dispatch: encode error reply: %v", err)
		return
	}
	d.dbg("sendError: uuid=%s code=%s detail=%s", orderUUID, errorCode, detail)
	if err := d.db.EnqueueOutbox(d.dispatchTopic, data, "order.error", stationID); err != nil {
		log.Printf("dispatch: enqueue error reply for %s: %v", orderUUID, err)
	}
}

// syntheticEnvelope creates a minimal envelope for internal dispatch (compound orders).
func (d *Dispatcher) syntheticEnvelope(stationID string) *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: d.stationID},
	}
}
