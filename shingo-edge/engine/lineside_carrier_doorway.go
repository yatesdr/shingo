// lineside_carrier_doorway.go — the one way the Edge records what carrier is
// standing on a node.

package engine

import (
	"shingoedge/domain"
)

// recordLinesideCarrier is THE DOORWAY for the lineside identity. Every write
// of process_node_runtime_states.lineside_payload_code goes through here and
// names its source.
//
// ── WHY A DOORWAY, AND WHY THIS FIELD ────────────────────────────────────
//
// The identity of the carrier at a node used to be read off active_claim_id,
// which eighteen paths write and which most of them fill from the process's
// active style — the REQUESTED identity. So an ordinary recovery action could
// re-aim a cell's idea of what was standing on it, and nothing recorded that it
// had. An operator cycle-counting a carrier they could see would stamp the
// requested claim over the lineside one and disarm the evacuation fix for that
// carrier's entire stay.
//
// The identity now lives on its own field, written only from here. That is what
// makes the counting case safe rather than the count being forbidden: a count
// never reaches this function, because a count is about parts.
//
// ── OPERATOR WRITES ARE FIRST-CLASS ──────────────────────────────────────
//
// This does not ban operator authorship, and banning it would be wrong. On a
// manual load the operator IS the best instrument available — no delivery
// envelope arrives ahead of them, so without their answer there is none. Their
// assertion is recorded like any other, with the source naming who said it.
//
// ── UNKNOWN CLEARS, IT DOES NOT SKIP ─────────────────────────────────────
//
// Passing an unknown carrier erases what was there. Leaving a stale identity
// standing would be worse than the gap: the readers of this field fail open on
// an empty value and act confidently on a populated one, so a value held over
// from the previous occupant is a confident wrong answer about this one.
//
// -- THE DOUBT SURVIVES THE WRITE -----------------------------------------
//
// All three of what this function is told go to the row: the payload, whether
// it is an ANSWER (lineside_payload_known), and who said so (lineside_source),
// plus the time it was said. The known bit is the load-bearing one. Without it
// a KNOWN EMPTY carrier and an unreadable node both arrive at the readers as
// "", and each reader guessed the same way -- fall back to the claim, which is
// the requested identity and the exact read this doorway exists to end. With
// it, binAtNode and residentEvacDest ask "was this established" instead of
// "is the string blank".
func (e *Engine) recordLinesideCarrier(nodeID int64, nodeName string, carrier domain.LinesideCarrier, src domain.CarrierSource) {
	payload, known := carrier.Payload()
	if err := e.db.SetProcessNodeRuntimeLinesidePayload(nodeID, string(payload), known, string(src)); err != nil {
		e.logFn("lineside carrier: record node=%d (%s) source=%s: %v", nodeID, nodeName, src, err)
		return
	}
	switch {
	case !known:
		e.debugFn("lineside carrier: node=%d (%s) cleared, source=%s", nodeID, nodeName, src)
	case payload == "":
		e.debugFn("lineside carrier: node=%d (%s) is empty, source=%s", nodeID, nodeName, src)
	default:
		e.debugFn("lineside carrier: node=%d (%s) is %s, source=%s", nodeID, nodeName, payload, src)
	}
}
