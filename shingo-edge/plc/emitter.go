package plc

// TickKind is how a counted tick was attributed (engine attributeTick).
type TickKind string

const (
	// TickToday: the point's style, or the active style's own part — the
	// attribution every tick had before the PLC's part was read.
	TickToday TickKind = "today"
	// TickTarget: the PLC's part is the open changeover's target's.
	TickTarget TickKind = "target"
	// TickOffPlan: the part is one other style's, the active set proven.
	TickOffPlan TickKind = "off-plan"
	// TickHeld: the part is no style's, or several's, the active set proven.
	TickHeld TickKind = "held"
)

// TickIdentity is what a counted tick is stamped with before its snapshot row
// is written: the style it counts under, the PLC's part and how it was decided.
type TickIdentity struct {
	StyleID int64
	CATID   string
	Kind    TickKind
}

// EventEmitter is the interface the PLC package uses to emit events.
// The engine package implements this via an adapter to avoid import cycles.
type EventEmitter interface {
	EmitCounterRead(rpID int64, plcName, tagName string, value int64)
	// AttributeTick decides a counted tick's identity for a process whose
	// reporting point is on pointStyleID. Called by the poll before the
	// snapshot INSERT; it must never wait on the CATID monitor.
	AttributeTick(processID, pointStyleID int64) TickIdentity
	EmitCounterDelta(rpID, processID, delta, newCount int64, anomaly string, id TickIdentity)
	// EmitLineCounterDelta is a station counter's counted strokes, with the
	// identity AttributeTick gave them against the process's active style.
	EmitLineCounterDelta(counterID, processID, delta int64, anomaly string, id TickIdentity)
	EmitPLCConnected(plcName string)
	EmitPLCDisconnected(plcName string, err error)
	EmitPLCHealthAlert(plcName string, errMsg string)
	EmitPLCHealthRecover(plcName string)
	EmitCounterReadError(rpID int64, plcName, tagName, errMsg string)
	EmitWarLinkConnected()
	EmitWarLinkDisconnected(err error)
	// EmitCurtainChanged says a curtain tag a station view reads changed its
	// reading in the cache (Manager.WatchCurtainTag).
	EmitCurtainChanged()
}
