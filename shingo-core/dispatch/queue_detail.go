package dispatch

import (
	"shingo/protocol"
	"shingocore/store/orders"
)

// queue_detail.go — THE ONE BODY THAT WRITES A WAIT.
//
// ── WHY THIS IS ONE FUNCTION AND NOT THREE ────────────────────────────────
//
// There were three: Dispatcher.setQueueReason, PlanningService.setQueueReason,
// and fulfillment Scanner.setQueueReason. Byte-for-byte the same decision —
// format the sentence, short-circuit when nothing changed, write all three
// columns together, mirror them back onto the in-memory order — differing only
// in which store handle and which log sink they closed over, and in whether they
// bothered to return the edge.
//
// THE SHORT-CIRCUIT IS THE REASON THIS MATTERS, not tidiness. SetOrderQueueDetail
// bumps updated_at, and updated_at is what every age instrument on the board
// reads. A park that rewrites the same sentence every scanner tick keeps its own
// row looking one tick old forever, so a wait that has lasted an hour reads as
// fresh. The three copies all had the guard; a fourth spelling written by
// somebody in a hurry would not, and it would be invisible — the sentence on the
// board would be right, and only the AGE would be a lie.
//
// It is also an event-loop guard: re-touching the row can re-trigger the very
// scanner tick that just parked the order.
//
// ── WHAT MUST SURVIVE ANY EDIT HERE ───────────────────────────────────────
//
//   - THE BOOL. It reports whether this call actually wrote a NEW wait, which is
//     a fact only this function holds. parkOnClaimedBlocker fires the
//     stopped-blocker alarm on the EDGE of a wait rather than on every pass that
//     re-asserts it (acceptance_dig.go), and the short-circuit below IS that
//     edge. A false means either "the row already said this" or "the write
//     failed"; neither is a new wait, and both are already logged or harmless.
//   - THE LOG SINK. The scanner writes to the plant log through an injected
//     logFn (a struct field, not a parameter — see Scanner.setQueueReason), and
//     the dispatch side writes through the standard logger. Hardcoding either
//     would silence the other.
//   - THE POINTER WRITE-BACK. Callers keep using the order struct after parking,
//     and the transition that queues it takes its history row's code off that
//     struct (lifecycle.go historyReason). A write that lands in the database but
//     not on the struct produces a `queued` history row born blank, which is the
//     only durable record of what a wait was for.
//
// ── THE CAUSE IS PART OF THE IDENTITY, NOT A DECORATION ───────────────────
//
// The comparison includes the cause. Two causes can share one code and render
// one sentence — the two blocker refusals do — so comparing without it leaves
// the stale cause on the row forever. The buried path sets "storage is being
// rearranged" on arrival and then NARROWS the cause to lane-locked or lock-race;
// without the cause in this comparison the narrower tag never lands.
//
// ── TWO SITES STILL WRITE THE COLUMNS DIRECTLY, AND BOTH ARE CORRECT ──────
//
// complex_intake.go writes the detail immediately after CreateOrder, before the
// order has ever been read back: there is no prior value to short-circuit
// against and no live struct anybody is about to transition. lifecycle.go's
// ResumeCompound CLEARS all three columns, and clearing is not a wait — there is
// no code and no params, so there is no sentence to format. Neither is a fourth
// spelling of this decision; they are different decisions.

// QueueDetailStore is the single store method the door needs. Narrow on purpose:
// the fulfillment Scanner holds an interface, not the concrete *store.DB, and a
// wider dependency here would drag that whole surface into the door.
type QueueDetailStore interface {
	SetOrderQueueDetail(id int64, reason string, code protocol.QueueCode, cause string) error
}

// WriteQueueDetail formats the operator sentence from code+params and writes
// sentence+code+cause together, returning whether it wrote a NEW wait.
//
// Best-effort by design: a failed write is logged and swallowed. Queue detail is
// advisory HMI/queue metadata and never a correctness gate, and failing a park
// because its explanation could not be stored would trade a described wait for
// an undescribed stall.
//
// who names the subsystem in the log line, so a failure is still attributable
// after the three copies became one.
func WriteQueueDetail(db QueueDetailStore, logf func(string, ...any), who string,
	order *orders.Order, code protocol.QueueCode, cause QueueCause, params QueueParams) bool {
	reason := FormatQueueSentence(code, params)
	if order.QueueReason == reason && order.QueueCode == string(code) && order.QueueCause == string(cause) {
		return false
	}
	if err := db.SetOrderQueueDetail(order.ID, reason, code, string(cause)); err != nil {
		logf("%s: set queue_reason (%s) for order %d: %v", who, cause, order.ID, err)
		return false
	}
	order.QueueReason = reason
	order.QueueCode = string(code)
	order.QueueCause = string(cause)
	return true
}
