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

// SetContainmentChangedFunc wires the push that follows a known containment
// write to the messaging layer, which builds and broadcasts the snapshot. One
// hook for every writer: the payload service's flag and bin-hold writes and
// the divert's arrival stamp. Set once at the composition root.
func (e *Engine) SetContainmentChangedFunc(fn func()) {
	e.containmentChanged.Store(&fn)
	e.payloadService.SetContainmentChangedFunc(fn)
}

// SetBinChangedFunc wires the push that follows a station door's write to a
// bin (LOAD, count, CLEAR) to the messaging layer, which rebuilds the bins
// snapshot of every Edge watching that bin. The LOAD and the count reach it
// through the bin manifest service; the CLEAR through this engine's
// ClearForReuseStampAndBookDeparture. Set once at the composition root.
func (e *Engine) SetBinChangedFunc(fn func(binID int64)) {
	e.binChanged.Store(&fn)
	e.binManifest.SetBinChangedFunc(fn)
}

// SetBinsRowsFunc wires the read of Core's current bins rows for node names
// (the messaging layer's CoreDataService.NodeBinsRows) that a write door's
// refusal carries, so the Edge redraws the nodes from Core's rows before it
// shows the sentence: the HTTP doors read it through the node service, the
// home clear and the wire intake through this engine. Set once at the
// composition root.
func (e *Engine) SetBinsRowsFunc(fn func(names []string) ([]protocol.NodeBinsRow, error)) {
	e.nodeService.SetBinsRowsFunc(fn)
}

// SetLevelChangedFunc wires the push that follows a group's level save (the
// Nodes page's level and layer-want save and remove, through the node service)
// to the messaging layer, which rebuilds the bins snapshot of every Edge with a
// consume loader feeding that group on its list. Set once at the composition
// root.
func (e *Engine) SetLevelChangedFunc(fn func(groupNodeID int64)) {
	e.nodeService.SetLevelChangedFunc(fn)
}

// notifyBinChanged calls the bin hook, if one is wired.
func (e *Engine) notifyBinChanged(binID int64) {
	if fn := e.binChanged.Load(); fn != nil && *fn != nil {
		(*fn)(binID)
	}
}

// notifyContainmentChanged calls the hook, if one is wired.
func (e *Engine) notifyContainmentChanged() {
	if fn := e.containmentChanged.Load(); fn != nil && *fn != nil {
		(*fn)()
	}
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
