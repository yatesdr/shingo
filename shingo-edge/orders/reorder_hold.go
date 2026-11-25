package orders

import (
	"fmt"
	"log"
	"strings"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/domain"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// holdOnCoreCancel is the hold a cancel Core sends puts on the order's
// position: off when Core gave up on an order that waited too long (its reason
// opens protocol.CancelReasonAbandonedPrefix), a pause for any other.
//
// AN ABANDONED SWAP ARRIVES AS TWO CANCELS. Core cancels the leg that waited
// ("abandoned: ...") and then, in the same call stack, its partner ("coordinated
// swap ... cancelling ..."), which is a pause by its words. The write's rank
// (processes.HoldReorder) keeps the pause from stepping the off down, whichever
// arrives first. Pinned by TestCoreCancel_AnAbandonedSwapEndsOff.
func holdOnCoreCancel(orderID int64, reason string) (domain.ReorderHold, string) {
	if strings.HasPrefix(reason, protocol.CancelReasonAbandonedPrefix) {
		return domain.ReorderHoldOff, fmt.Sprintf("Core gave up waiting on order %d (%s)", orderID, reason)
	}
	return domain.ReorderHoldPaused, fmt.Sprintf("Core cancelled order %d (%s)", orderID, reason)
}

// holdReorderAt holds the auto-reorder of the order's position (kind, at the
// engine's clock), unless the position holds a higher kind. An order with no
// position holds nothing. A write that fails is logged and the reply goes on:
// the order's own status is what the reply is about, and a missed pause costs
// one re-ask.
func (m *Manager) holdReorderAt(order *orders.Order, kind domain.ReorderHold, note string) {
	if order.ProcessNodeID == nil {
		return
	}
	wrote, err := processes.HoldReorder(m.db.DB, *order.ProcessNodeID, kind, clock.Now(), note)
	switch {
	case err != nil:
		log.Printf("orders: auto-reorder hold for order %d: %v", order.ID, err)
	case wrote:
		log.Printf("orders: auto-reorder %s at node %d: %s", kind, *order.ProcessNodeID, note)
	}
}

// CancelOrderByPerson is the Orders page's cancel, the HMI cancel door: the
// order's cancel (CancelOrder), then a pause on its position's auto-reorder, so
// the sweep does not ask again a minute after a person said stop. A cancel the
// door refuses (an ended order, a delivered one: ErrDelivered) pauses
// nothing. The Edge's own cancels (a changeover's start or cancel, a
// send-back's) go through CancelOrderWithReason and pause nothing.
func (m *Manager) CancelOrderByPerson(orderID int64) error {
	if err := m.CancelOrder(orderID); err != nil {
		return err
	}
	order, err := m.db.GetOrder(orderID)
	if err != nil {
		log.Printf("orders: auto-reorder hold for order %d: read the cancelled order: %v", orderID, err)
		return nil
	}
	m.holdReorderAt(order, domain.ReorderHoldPaused, fmt.Sprintf("a person cancelled order %d", orderID))
	return nil
}
