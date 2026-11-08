// consume_stroke.go — a consume stroke charges the cart of its part standing
// at the position, or is logged (decision 55).

package engine

import (
	"log"

	"shingoedge/store/processes"
)

// chargeOrLog applies one consume stroke's draw (what the pile did not take)
// to the seat as rt holds it. A cart of the stroke's part standing there
// (cartOfThePart) is charged input, into the negative if that is where the
// count goes: the release records what it reads. With none there — the seat
// empty, a cart released at 0, a cart of another part, a cart that cannot say
// read as the departing part — the stroke is logged in the ledger against the
// position and the part (logNoCart), and nothing else ever happens to it: no
// hold, no replay onto the next cart, which lands at its full count.
//
// Both writes name the seat as the walk read it (processes.SeatAsRead), so a
// seat changed since — a landing, a lift, a release, an operator's count under
// countMu — refuses, and the stroke re-reads the seat once, under the lock,
// and applies to it as it now is. The re-read runs only on a refusal: +0
// statements on every other stroke, and one statement per stroke, as the
// charge or the hold it replaces was. Returns the seat applied to and what the
// cart was charged (0 when logged, so no bin delta is sent). Caller holds
// countMu.
func (e *Engine) chargeOrLog(node *processes.Node, rt *processes.RuntimeState, claim *processes.NodeClaim, tick tickScope,
	unsaid unsaidCarrier, input int) (*processes.RuntimeState, int) {
	for attempt := 0; ; attempt++ {
		charged, written := e.chargeOrLogOnce(node, rt, claim, tick, unsaid, input)
		if written {
			return rt, charged
		}
		if attempt == 1 {
			log.Printf("counter delta: node %s: the seat changed twice under the count lock; %d not charged or logged",
				node.CoreNodeName, input)
			return rt, 0
		}
		fresh, err := e.db.GetProcessNodeRuntime(node.ID)
		if err != nil || fresh == nil {
			log.Printf("counter delta: node %s: the seat changed since the tick read it and cannot be re-read (%v); %d not charged or logged",
				node.CoreNodeName, err, input)
			return rt, 0
		}
		rt = fresh
	}
}

// chargeOrLogOnce is one attempt of chargeOrLog on the seat as rt holds it.
// written is false only when the seat refused the write (it changed since rt
// was read); a failed statement is logged and counts as written, as the
// charge always did.
func (e *Engine) chargeOrLogOnce(node *processes.Node, rt *processes.RuntimeState, claim *processes.NodeClaim, tick tickScope,
	unsaid unsaidCarrier, input int) (charged int, written bool) {
	if input == 0 {
		return 0, true
	}
	seat := processes.SeatAsReadOf(rt)
	if cartOfThePart(rt, claim, unsaid) {
		ok, err := processes.ChargeSeatUOP(e.db.DB, node.ID, -input, false, seat)
		if err != nil {
			log.Printf("update UOP for node %d: %v", node.ID, err)
			return input, true
		}
		if !ok {
			return 0, false
		}
		return input, true
	}
	ok, err := e.logNoCart(tick.processID, tick.catid, node, claim.PayloadCode, input, seat, tick.source)
	if err != nil {
		log.Printf("counter delta: node %s: %d of part %q not logged: %v", node.CoreNodeName, input, claim.PayloadCode, err)
		return 0, true
	}
	return 0, ok
}

// cartOfThePart reports whether a cart of the stroke's part stands at the
// seat: bound, not released at 0 or below (its strokes are logged, R3), and
// not a part the claim does not take — recorded as one, or a cart that cannot
// say read as the departing part under an open changeover (unsaidCarrier).
func cartOfThePart(rt *processes.RuntimeState, claim *processes.NodeClaim, unsaid unsaidCarrier) bool {
	return rt.ActiveBinID != nil && !rt.ReleasedEmpty && !drawsAnotherPart(rt, claim, unsaid.partOf(rt))
}
