// countgroup_sender.go — outbound data message to core for count-group
// request/ack telemetry. Follows the established SendClaimSync /
// RequestOrderStatusSync pattern: build a NewDataEnvelope, call sendFn.

package engine

import (
	"fmt"

	"shingo/protocol"
)

// SendCountGroupAck publishes a CountGroupAck back to core via the edge's
// configured send function. Returns an error only if sendFn is unset
// (messaging not connected yet) or the envelope build fails; caller is
// expected to log-and-continue since ack delivery failures don't affect
// PLC state, only the audit trail.
func (e *Engine) SendCountGroupAck(ack *protocol.CountGroupAck) error {
	if e.sendFn == nil {
		return fmt.Errorf("send function not configured (messaging not connected)")
	}
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectCountGroupAck,
		protocol.Address{Role: protocol.RoleEdge, Station: e.cfg.StationID()},
		protocol.Address{Role: protocol.RoleCore},
		ack,
	)
	if err != nil {
		return fmt.Errorf("build countgroup ack envelope: %w", err)
	}
	return e.sendFn(env)
}
