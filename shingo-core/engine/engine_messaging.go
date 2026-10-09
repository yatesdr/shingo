package engine

import (
	"fmt"

	"shingo/protocol"

	"shingocore/store/messaging"
)

// ── Outbound messaging ──────────────────────────────────────────────
//
// SendDataToEdge builds envelopes and pushes them through
// the outbox. RunFulfillmentScan is the test hook that triggers a
// single scanner pass; it lives alongside the messaging shims because
// both are thin wrappers used from outside the engine package.

// SendDataToEdge builds a data-channel envelope and enqueues it via outbox.
// Used by HTTP handlers to push data notifications (e.g., node structure changes).
func (e *Engine) SendDataToEdge(subject string, stationID string, payload any) error {
	data, msgType, err := messaging.BuildDataEnvelope(subject, e.cfg.Messaging.StationID, stationID, payload)
	if err != nil {
		return err
	}
	if err := e.db.EnqueueOutbox(e.cfg.Messaging.DispatchTopic, data, msgType, stationID); err != nil {
		e.logFn("engine: outbox enqueue data %s to %s failed: %v", subject, stationID, err)
		return fmt.Errorf("enqueue data %s: %w", subject, err)
	}
	return nil
}

// RequestEdgeReregister asks an edge (or every edge, when station is "") to
// re-send its registration — which now carries the cell catalog (Q-034). Same
// data-channel message core already fires when it detects an unregistered edge;
// this exposes it as an on-demand action (the Dashboard "re-sync edges" button)
// so a catalog change is picked up without waiting for a reconnect.
func (e *Engine) RequestEdgeReregister(station string) error {
	if station == "" {
		station = protocol.StationBroadcast
	}
	return e.SendDataToEdge(protocol.SubjectEdgeRegisterRequest, station,
		&protocol.EdgeRegisterRequest{StationID: station, Reason: "manual re-sync (dashboard)"})
}

// RunFulfillmentScan runs one pass of the fulfillment scanner and returns the
// number of orders processed. For testing.
func (e *Engine) RunFulfillmentScan() int {
	if e.fulfillment == nil {
		return 0
	}
	return e.fulfillment.RunOnce()
}

// SetEdgeFeedFlagsFunc wires the read behind EdgeFeedFlags to the messaging
// layer, which owns the feed send records. Set once at the composition root.
func (e *Engine) SetEdgeFeedFlagsFunc(fn func(station string) []string) {
	e.edgeFeedFlags.Store(&fn)
}

// EdgeFeedFlags lists the plain-text feed conditions flagged for one station —
// a feed resent three times without converging, a process two stations both
// report. Read-only; nil until the messaging layer is wired, and empty when
// nothing is wrong.
func (e *Engine) EdgeFeedFlags(station string) []string {
	if fn := e.edgeFeedFlags.Load(); fn != nil {
		return (*fn)(station)
	}
	return nil
}
