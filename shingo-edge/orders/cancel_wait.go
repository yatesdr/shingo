package orders

import (
	"fmt"
	"time"

	"shingo/protocol"
)

// cancel_wait.go — the tap's wait for Core's answer to a cancel it asked
// (final brief 5d).
//
// THE WAITER IS MEMORY ONLY, as the ask pair's is (engine/ask.go): the tap
// blocks for a short wait and answers as the asks do. The row is what holds
// the cancel (orders.cancel_intent), so an answer that lands after the wait,
// or after an Edge restart, is applied by its arm all the same (core_word.go:
// the applier, RefuseCancel, EndCancelCoreDoesNotHold); only the tap's own
// answer is lost with the process. Nothing here is a statement or a message.
// The wait holds no lock: a caller under a cell lock or the changeover's door
// asks and does not wait.

// CancelAnswer is what the tap answers after its wait (AwaitCancel).
type CancelAnswer struct {
	// Cancelled: the order ended cancelled (Core's word; or here at the tap,
	// for an order with no versioned word of Core's).
	Cancelled bool
	// Refused is Core's sentence when it answered that it cannot cancel.
	Refused string
	// Ended is the status the order ended in when it was not cancelled (Core
	// ended it another way while the cancel was out).
	Ended protocol.Status
	// Pending: sent; Core has not answered yet. The answer, when it comes,
	// redraws the row.
	Pending bool
}

// Sentence is the answer in the operator's words, for a tap that is not
// cancelled.
func (a CancelAnswer) Sentence(orderID int64) string {
	switch {
	case a.Refused != "":
		return a.Refused
	case a.Ended != "":
		return fmt.Sprintf("order %d ended %s before Core took the cancel; it was not cancelled", orderID, a.Ended)
	case a.Pending:
		return CancelPendingNotice
	}
	return ""
}

// CancelPendingNotice is the tap's answer when Core has not answered within
// the wait: the cancel is sent, never failed.
//
// OWNER-WORDS
const CancelPendingNotice = "Cancel sent; Core has not answered yet"

// AwaitCancel waits up to wait for Core's answer to the cancel this station
// asked of order orderID, and answers by the row (cancelAnswerOf). A row that
// has already answered (ended at the tap, or answered before the wait began)
// answers at once. Pinned by TestAwaitCancel_AnswersAsTheAsksDo.
func (m *Manager) AwaitCancel(orderID int64, wait time.Duration) (CancelAnswer, error) {
	ch, _ := m.cancelWaits.LoadOrStore(orderID, make(chan struct{}))
	if a, err := m.cancelAnswerOf(orderID); err != nil || !a.Pending {
		return a, err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch.(chan struct{}):
	case <-timer.C:
	}
	return m.cancelAnswerOf(orderID)
}

// cancelAnswerOf reads the order's answer to the cancel asked of it.
func (m *Manager) cancelAnswerOf(orderID int64) (CancelAnswer, error) {
	o, err := m.db.GetOrder(orderID)
	if err != nil {
		return CancelAnswer{}, fmt.Errorf("get order %d: %w", orderID, err)
	}
	switch {
	case o.Status == StatusCancelled:
		return CancelAnswer{Cancelled: true}, nil
	case IsTerminal(o.Status):
		return CancelAnswer{Ended: o.Status}, nil
	case o.CancelRefused != "":
		return CancelAnswer{Refused: o.CancelRefused}, nil
	}
	return CancelAnswer{Pending: true}, nil
}

// cancelAnswered wakes the tap waiting on order orderID's cancel, if one is:
// Core's word ended the order, or Core refused the cancel.
func (m *Manager) cancelAnswered(orderID int64) {
	if ch, ok := m.cancelWaits.LoadAndDelete(orderID); ok {
		close(ch.(chan struct{}))
	}
}
