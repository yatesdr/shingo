package engine

import (
	"fmt"

	"shingo/protocol"
)

// ── Outbound messaging ──────────────────────────────────────────────
//
// SendToEdge / SendDataToEdge build envelopes and push them through
// the outbox. RunFulfillmentScan is the test hook that triggers a
// single scanner pass; it lives alongside the messaging shims because
// both are thin wrappers used from outside the engine package.

// SendToEdge is an exported wrapper around sendToEdge, allowing HTTP handlers
// and other external callers to enqueue messages for edge stations via outbox.
func (e *Engine) SendToEdge(msgType string, stationID string, payload any) error {
	return e.sendToEdge(msgType, stationID, payload)
}

// SendDataToEdge builds a data-channel envelope and enqueues it via outbox.
// Used by HTTP handlers to push data notifications (e.g., node structure changes).
func (e *Engine) SendDataToEdge(subject string, stationID string, payload any) error {
	coreAddr := protocol.Address{Role: protocol.RoleCore, Station: e.cfg.Messaging.StationID}
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: stationID}
	env, err := protocol.NewDataEnvelope(subject, coreAddr, edgeAddr, payload)
	if err != nil {
		return fmt.Errorf("build data %s: %w", subject, err)
	}
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode data %s: %w", subject, err)
	}
	msgType := "data." + subject
	if err := e.db.EnqueueOutbox(e.cfg.Messaging.DispatchTopic, data, msgType, stationID); err != nil {
		e.logFn("engine: outbox enqueue data %s to %s failed: %v", subject, stationID, err)
		return fmt.Errorf("enqueue data %s: %w", subject, err)
	}
	return nil
}

// RunFulfillmentScan runs one pass of the fulfillment scanner and returns the
// number of orders processed. For testing.
func (e *Engine) RunFulfillmentScan() int {
	if e.fulfillment == nil {
		return 0
	}
	return e.fulfillment.RunOnce()
}
