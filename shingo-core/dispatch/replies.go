package dispatch

import "shingo/protocol"

// TODO(dead-code): no callers as of 2026-04-17; verify before the next refactor.
func (d *Dispatcher) coreAddress() protocol.Address {
	return d.replies.CoreAddress()
}

func (d *Dispatcher) sendAck(env *protocol.Envelope, orderUUID string, shingoOrderID int64, sourceNode string) {
	d.replies.SendAck(env, orderUUID, shingoOrderID, sourceNode)
}

func (d *Dispatcher) sendError(env *protocol.Envelope, orderUUID, errorCode, detail string) {
	d.replies.SendError(env, orderUUID, errorCode, detail)
}

// syntheticEnvelope creates a minimal envelope for internal dispatch (compound orders).
func (d *Dispatcher) syntheticEnvelope(stationID string) *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: d.stationID},
	}
}
