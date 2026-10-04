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
// the stale cause on the row forever: a buried demand moving from "nowhere to
// put the blocker" to "a robot is in the lane" keeps one code and one sentence,
// and without the cause in this comparison the second tag never lands. Telling
// the station is a different question with a different answer (WaitTold): the
// wire carries no cause.
//
// ── ONE SITE STILL WRITES THE COLUMNS DIRECTLY, AND IT IS CORRECT ─────────
//
// lifecycle.go's ResumeCompound CLEARS all three columns, and clearing is not a
// wait — there is no code and no params, so there is no sentence to format. It
// is not another spelling of this decision; it is a different decision. Complex
// intake and the pair park used to write directly too, which left the order's
// struct (complex intake) or the station (the pair park's partner) behind the
// row; both write through here now.

// QueueDetailStore is the single store method the door needs. Narrow on purpose:
// the fulfillment Scanner holds an interface, not the concrete *store.DB, and a
// wider dependency here would drag that whole surface into the door.
type QueueDetailStore interface {
	SetOrderQueueDetail(id int64, reason string, code protocol.QueueCode, cause string) error
}

// QueueWait is what an order is waiting on, as its row carries it: the code
// and the sentence the station reads, and the cause, which stays in Core.
type QueueWait struct {
	Code, Cause, Reason string
}

// WaitOf reads the wait an order struct carries.
func WaitOf(o *orders.Order) QueueWait {
	return QueueWait{Code: o.QueueCode, Cause: o.QueueCause, Reason: o.QueueReason}
}

// WaitTold reports whether a station that holds prev is told next. It is told a
// new code, or a new cause that reads as a new sentence. The wire carries the
// status, the sentence and the code, never the cause, so a cause alone is not
// news; and a sentence whose code and cause are unchanged (a count in it moved)
// is not a new wait. The first wait (prev has no code) is never told here: it
// reaches the station with the order's announcement, the queued event's push.
// Clearing (next has no code) is not a wait.
//
// One predicate for both doors that tell a station: WriteQueueDetail asks it of
// each write, and the push after a queued announcement asks it of the wait the
// order was announced with, to skip a change the write already told.
func WaitTold(prev, next QueueWait) bool {
	return prev.Code != "" && next.Code != "" &&
		(prev.Code != next.Code || (prev.Cause != next.Cause && prev.Reason != next.Reason))
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
//
// A CHANGED WAIT IS TOLD TO THE STATION, ONCE. notify is called when the write
// is one WaitTold tells and the order is still acquiring (queued or sourcing).
// A wait that changed later used to stay in Core: a refill that waited for
// material and then for its slot went on reading "waiting for material" at the
// station. Writes that repeat the row write nothing, so a wait re-asserted on
// every scanner pass costs no statement and no message. nil notifies nobody.
func WriteQueueDetail(db QueueDetailStore, logf func(string, ...any), who string,
	order *orders.Order, code protocol.QueueCode, cause QueueCause, params QueueParams,
	notify func(*orders.Order)) bool {
	return writeQueueWait(db, logf, who, order,
		QueueWait{Code: string(code), Cause: string(cause), Reason: FormatQueueSentence(code, params)}, notify)
}

// writeQueueWait is WriteQueueDetail for a sentence that is already formatted:
// the pair park copies the blocked leg's wait onto its partner as it stands.
func writeQueueWait(db QueueDetailStore, logf func(string, ...any), who string,
	order *orders.Order, next QueueWait, notify func(*orders.Order)) bool {
	prev := WaitOf(order)
	if prev == next {
		return false
	}
	if err := db.SetOrderQueueDetail(order.ID, next.Reason, protocol.QueueCode(next.Code), next.Cause); err != nil {
		logf("%s: set queue_reason (%s) for order %d: %v", who, next.Cause, order.ID, err)
		return false
	}
	order.QueueReason, order.QueueCode, order.QueueCause = next.Reason, next.Code, next.Cause
	if notify != nil && WaitTold(prev, next) && protocol.IsAcquiring(order.Status) {
		notify(order)
	}
	return true
}
