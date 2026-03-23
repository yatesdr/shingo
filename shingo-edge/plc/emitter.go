package plc

// EventEmitter is the interface the PLC package uses to emit events.
// The engine package implements this via an adapter to avoid import cycles.
type EventEmitter interface {
	EmitCounterRead(rpID int64, plcName, tagName string, value int64)
	EmitCounterDelta(rpID, processID, styleID, delta, newCount int64, anomaly string)
	EmitCounterAnomaly(snapshotID, rpID int64, plcName, tagName string, oldVal, newVal int64, anomalyType string)
	EmitPLCConnected(plcName string)
	EmitPLCDisconnected(plcName string, err error)
	EmitPLCHealthAlert(plcName string, errMsg string)
	EmitPLCHealthRecover(plcName string)
	EmitCounterReadError(rpID int64, plcName, tagName, errMsg string)
	EmitWarLinkConnected()
	EmitWarLinkDisconnected(err error)
}
