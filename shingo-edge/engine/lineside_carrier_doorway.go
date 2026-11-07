// lineside_carrier_doorway.go — the one way the Edge records what carrier is
// standing on a node.

package engine

import (
	"fmt"

	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
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

// recordLinesideCarrierIn is the doorway on a caller's transaction, for an
// identity write that must land with a seat write or not at all. It logs
// nothing: the caller logs after its commit.
func (e *Engine) recordLinesideCarrierIn(ex processes.DBTX, nodeID int64, carrier domain.LinesideCarrier, src domain.CarrierSource) error {
	payload, known := carrier.Payload()
	return processes.SetRuntimeLinesidePayload(ex, nodeID, string(payload), known, string(src))
}

// carrierRecordSaid says where a seat's recorded carrier came from, in the
// words of the act that recorded it: "from the switch off RUN", "from the
// delivery", "by the operator", "from Core's record of the bin"; "" for a
// source no person would recognise. For the flip it names the style that ran:
// the one style of the node's process, other than except, whose claim there
// takes the recorded part ("from a style switch" when none or several do).
// Reads the process's styles and their claims only for a flip.
func (e *Engine) carrierRecordSaid(node *processes.Node, rt *processes.RuntimeState, except int64) string {
	switch domain.CarrierSource(rt.LinesideSource) {
	case domain.CarrierFromFlip:
		if name := e.styleThatRan(node, string(rt.LinesidePayloadCode), except); name != "" {
			return "from the switch off " + name
		}
		return "from a style switch"
	case domain.CarrierFromLanding, domain.CarrierFromDelivery:
		return "from the delivery"
	case domain.CarrierFromOperator:
		return "by the operator"
	case domain.CarrierFromReconcile:
		return "from Core's record of the bin"
	}
	return ""
}

// styleThatRan is the one style of the node's process, other than except,
// whose claim at the node takes part; "" when none or several do.
func (e *Engine) styleThatRan(node *processes.Node, part string, except int64) string {
	styles, err := e.db.ListStylesByProcess(node.ProcessID)
	if err != nil {
		return ""
	}
	var ids []int64
	for _, s := range styles {
		if s.ID != except {
			ids = append(ids, s.ID)
		}
	}
	claims, err := e.db.NodeClaimsForStyles(ids)
	if err != nil {
		return ""
	}
	proc := &processes.Process{ID: node.ProcessID}
	name := ""
	for _, s := range styles {
		if s.ID == except || !claimTakes(claims.Resolve(proc, node, store.ExactStyle(s.ID)), part) {
			continue
		}
		if name != "" {
			return ""
		}
		name = s.Name
	}
	return name
}

// recordedAs is "recorded as P (said)": what a seat's known carrier is recorded
// as, an empty one as empty, and where the record came from when said names it.
func recordedAs(rt *processes.RuntimeState, said string) string {
	part := string(rt.LinesidePayloadCode)
	if part == "" {
		part = "empty"
	}
	if said == "" {
		return "recorded as " + part
	}
	return fmt.Sprintf("recorded as %s (%s)", part, said)
}
