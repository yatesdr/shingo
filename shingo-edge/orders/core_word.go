package orders

import (
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/orders"
)

// core_word.go — the one applier of Core's word about an order's status.
//
// ONE STATUS, CORE'S. A Core that versions its orders (protocol.OrderUpdate.
// Version) tells this Edge an order's status from the write that set it, with
// a counter bumped in the same statement. Every word of Core's about an order
// — the live order.update, the reconcile's snapshot, the projection — comes
// through applyCoreWord, which writes it only when its version is newer than
// the last one applied (orders.core_version, compared in the UPDATE's WHERE),
// with a history row and the status event. It is the only thing that writes a
// status Core told, and the only emitter of the four status-derived events for
// such an order (EventOrderStatusChanged, Completed, Delivered, Failed): a late,
// repeated or reordered word moves nothing and fires nothing.
//
// With a version, the messages that are not the status — the ack, the waybill
// and the dedicated envelopes (order.staged, order.delivered, order.error,
// order.cancelled, order.skipped) — write no status: they carry their own facts
// (where the robot parked, the delivered carrier, the reason). A Core that does
// not version (version 0 everywhere) is applied exactly as before, by the arms in
// lifecycle_service.go and manager_status.go.
//
// AN ORDER THAT HAS ENDED. A terminal Core told is final: Core does not move an
// ended order. A terminal this Edge wrote itself — the operator's abort, the
// operator's confirm — is the operator's word, which Core may not have yet. A
// newer word of Core's that the order is live keeps the row as the operator left
// it: on a cancelled row the cancel is sent again (the first may be lost); on a
// confirmed row a `delivered` is held.

// CoreWord is one word of Core's about an order: its status at its version, and
// the facts that ride it.
type CoreWord struct {
	Status  protocol.Status
	Version int64
	// Detail is the history row's sentence; "" writes a plain one.
	Detail  string
	RobotID string
	ETA     string
	// QueueReason and QueueCode are kept only while the order waits
	// (protocol.IsWaiting), as the live push always did.
	QueueReason, QueueCode string
	// ErrorCode is the code Core failed the order with (protocol.OrderUpdate.
	// ErrorCode), carried to the failed event and not stored. "" from an older
	// Core, a snapshot or a projection, none of which carries one.
	ErrorCode string
	// HasFault: the word carries the fault clock (an order.update, a snapshot).
	// A projection carries none, and a faulted order keeps the clock it has.
	HasFault          bool
	FaultSince        *time.Time
	FaultDeadline     *time.Time
	FaultNoticeAfterS int
	FaultRef          string
	// Reconcile: the word is a reconcile's (a snapshot, an Unlisted
	// projection), not a live tell. An operator-cancelled order Core still
	// reports live at the very version this Edge last applied sends its cancel
	// again: the live tell that would be newer may never come.
	Reconcile bool
}

// versionClashDetail opens the history row written when Core reports a
// different status at a version this Edge already applied. Each status write at
// Core bumps the version, so this is a defect to find, not a race; the row is
// kept. A constant, so it can be counted as MIRROR JUMP is.
const versionClashDetail = "VERSION CLASH"

// ApplyCoreWord applies a versioned word of Core's to the order with edge uuid
// uuid (applyCoreWord).
func (m *Manager) ApplyCoreWord(uuid string, w CoreWord) error {
	order, err := m.db.GetOrderByUUID(uuid)
	if err != nil {
		return fmt.Errorf("order %s not found: %w", uuid, err)
	}
	return m.applyCoreWord(order, w)
}

// ApplyCoreUpdate is a versioned order.update's whole effect: the status, the
// robot, the ETA, the wait and the fault clock, in one statement when it is
// newer (applyCoreWord).
func (m *Manager) ApplyCoreUpdate(p *protocol.OrderUpdate) error {
	m.DebugLog.Log("core word: uuid=%s status=%s version=%d robot=%s", p.OrderUUID, p.Status, p.Version, p.RobotID)
	return m.ApplyCoreWord(p.OrderUUID, CoreWord{
		Status:            protocol.Status(p.Status),
		Version:           p.Version,
		Detail:            p.Detail,
		RobotID:           p.RobotID,
		ETA:               p.ETA,
		QueueReason:       p.QueueReason,
		QueueCode:         p.QueueCode,
		ErrorCode:         p.ErrorCode,
		HasFault:          true,
		FaultSince:        p.FaultSince,
		FaultDeadline:     p.FaultDeadline,
		FaultNoticeAfterS: p.FaultNoticeAfterS,
		FaultRef:          encodeSnapshotFaultRef(p.FaultRef),
	})
}

// applyCoreWord is the applier (the file's header).
func (m *Manager) applyCoreWord(order *orders.Order, w CoreWord) error {
	if IsTerminal(order.Status) {
		return m.coreWordOnEnded(order, w)
	}
	if w.Version <= order.CoreVersion {
		if w.Version == order.CoreVersion && w.Status != order.Status {
			detail := fmt.Sprintf("%s: Core says %s at version %d, which this Edge applied as %s; the row is kept",
				versionClashDetail, w.Status, w.Version, order.Status)
			log.Printf("orders: order %d: %s", order.ID, detail)
			if err := m.db.InsertOrderHistory(order.ID, string(order.Status), string(order.Status), detail); err != nil {
				log.Printf("orders: record a version clash on order %d: %v", order.ID, err)
			}
		}
		return nil
	}
	row := orders.CoreWordRow{
		Status:    string(w.Status),
		RobotID:   w.RobotID,
		ETA:       w.ETA,
		Version:   w.Version,
		KeepFault: w.Status == StatusFaulted && !w.HasFault,
	}
	if protocol.IsWaiting(w.Status) {
		row.QueueReason, row.QueueCode = w.QueueReason, w.QueueCode
	}
	if w.Status == StatusFaulted && w.HasFault {
		row.FaultSince, row.FaultDeadline = w.FaultSince, w.FaultDeadline
		row.FaultNoticeAfterS, row.FaultRef = w.FaultNoticeAfterS, w.FaultRef
	}
	wrote, err := m.db.ApplyCoreWord(order.ID, row)
	if err != nil {
		return fmt.Errorf("apply core word to order %d: %w", order.ID, err)
	}
	if !wrote {
		return nil // a newer word landed first
	}
	if w.Status == order.Status {
		// The status stands; the robot, the wait, the clock or the ETA moved.
		// No scoped event announces those, so the boards drawing the order
		// refetch.
		m.emitter.EmitOrderTouched(order.ID, order.UUID, order.ProcessNodeID)
		return nil
	}
	m.coreMoved(order, w)
	return nil
}

// coreMoved is a status Core told, applied: its history row, then the events in
// the order every status move fires them (applyTransition's), then the
// operator's word at the live→terminal edge.
func (m *Manager) coreMoved(order *orders.Order, w CoreWord) {
	old := order.Status
	detail := w.Detail
	if detail == "" {
		detail = fmt.Sprintf("core: %s (version %d)", w.Status, w.Version)
	}
	m.DebugLog.Log("core word applied: id=%d uuid=%s %s->%s version=%d", order.ID, order.UUID, old, w.Status, w.Version)
	if err := m.db.InsertOrderHistory(order.ID, string(old), string(w.Status), detail); err != nil {
		log.Printf("orders: history for order %d: %v", order.ID, err)
	}
	eta := w.ETA
	if eta == "" && order.ETA != nil {
		eta = *order.ETA
	}
	m.emitter.EmitOrderStatusChanged(order.ID, order.UUID, order.OrderType, string(old), string(w.Status), eta, nil, order.ProcessNodeID)
	if w.Status == StatusDelivered {
		m.coreDelivered(order)
	}
	if !IsTerminal(w.Status) {
		return
	}
	m.emitter.EmitOrderCompleted(order.ID, order.UUID, order.OrderType, nil, order.ProcessNodeID)
	if w.Status == StatusFailed {
		m.emitter.EmitOrderFailed(order.ID, order.UUID, order.OrderType, w.ErrorCode, detail)
	}
	m.coreEnded(order, w.Status, detail)
}

// coreEnded is what Core ending a live order says to the operator, the same for
// a live tell and a reconcile: a cancel this station did not ask for is
// announced (once per dead swap: a partner whose sibling already ended is not
// toasted again), and the position's auto-reorder holds for a cancel or a
// failure. An operator's own abort never reaches here: its row ended at the
// abort, and a Core word on an ended row moves nothing (coreWordOnEnded).
func (m *Manager) coreEnded(order *orders.Order, status protocol.Status, detail string) {
	switch status {
	case StatusCancelled:
		if !m.siblingAlreadyEnded(order) {
			m.emitter.EmitOrderCancelledByCore(order.ID, order.UUID, order.OrderType, detail)
		}
		kind, note := holdOnCoreCancel(order.ID, detail)
		m.holdReorderAt(order, kind, note)
	case StatusFailed:
		m.holdReorderAt(order, domain.ReorderHoldPaused, fmt.Sprintf("order %d failed (%s)", order.ID, detail))
	}
}

// coreWordOnEnded is a word of Core's about an order whose row has ended (the
// file's header). An older word moves nothing. A newer one is noted, so a later
// word older than it is known to be older.
func (m *Manager) coreWordOnEnded(order *orders.Order, w CoreWord) error {
	if w.Version < order.CoreVersion {
		return nil
	}
	newer := w.Version > order.CoreVersion
	switch {
	case order.Status == StatusCancelled && !IsTerminal(w.Status) && (newer || w.Reconcile):
		m.resendCancel(order, w)
	case order.Status == StatusConfirmed && w.Status == StatusDelivered && newer:
		m.DebugLog.Log("core word held: id=%d uuid=%s Core says delivered at version %d; the operator confirmed",
			order.ID, order.UUID, w.Version)
	}
	if newer {
		if err := m.db.NoteCoreVersion(order.ID, w.Version); err != nil {
			log.Printf("orders: note core version on order %d: %v", order.ID, err)
		}
	}
	return nil
}

// resendCancel sends this station's cancel of an order again, with the reason
// its abort gave (Core keys the partner leg's fate on it: an accepted half-swap
// leaves the partner alone). The first may never have reached Core, and the
// robot is still working the order. A copy Core already acted on is a no-op
// there: it does not move an ended order.
func (m *Manager) resendCancel(order *orders.Order, w CoreWord) {
	reason := "aborted by operator"
	if d, err := m.db.LatestOrderHistoryDetailsForStatus([]int64{order.ID}, string(StatusCancelled)); err == nil && d[order.ID] != "" {
		reason = d[order.ID]
	}
	if err := m.sender.Queue(protocol.TypeOrderCancel, &protocol.OrderCancel{OrderUUID: order.UUID, Reason: reason}); err != nil {
		log.Printf("orders: re-send the cancel of order %d: %v", order.ID, err)
		return
	}
	log.Printf("orders: order %d was cancelled here and Core still holds it %s (version %d); the cancel is sent again",
		order.ID, w.Status, w.Version)
}

// coreDelivered is Core's word that an order is delivered, at the applier's
// edge. The delivered follow-ons (EventOrderDelivered's carrier check and
// pullback clear, an auto-confirmed order's confirm) need order.delivered's
// facts as well, which Core queues before the status write, so they normally
// land first. Whichever of the two lands second runs them, once
// (deliveredFollowOns). Both halves are held in memory: an Edge restart between
// the two drops the follow-ons for that order, and Core's stuck-delivered sweep
// still confirms it.
func (m *Manager) coreDelivered(order *orders.Order) {
	if v, ok := m.deliveredFacts.LoadAndDelete(order.ID); ok {
		m.deliveredFollowOns(order, v.(deliveredSeed))
		return
	}
	m.deliveredAwaiting.Store(order.ID, struct{}{})
}

// deliveredFactsIn is a versioned order.delivered's facts, after their columns
// are written: the follow-ons run now if Core's delivered has already been
// applied, else they wait for it.
func (m *Manager) deliveredFactsIn(order *orders.Order, seed deliveredSeed) {
	if _, ok := m.deliveredAwaiting.LoadAndDelete(order.ID); ok {
		fresh, err := m.db.GetOrder(order.ID)
		if err != nil {
			log.Printf("orders: delivered follow-ons for order %d: %v", order.ID, err)
			return
		}
		// An order that ended before its facts landed (the operator confirmed
		// it on Core's delivered) has had its completion: a carrier check
		// after it would run in the wrong order. The facts are on the row.
		if IsTerminal(fresh.Status) {
			m.DebugLog.Log("delivered follow-ons dropped: id=%d uuid=%s ended %s before order.delivered landed",
				fresh.ID, fresh.UUID, fresh.Status)
			return
		}
		m.deliveredFollowOns(fresh, seed)
		return
	}
	m.deliveredFacts.Store(order.ID, seed)
}

// deliveredFollowOns is a delivery's follow-ons, once: the delivered event with
// the carrier's facts, then an auto-confirmed order's confirm.
func (m *Manager) deliveredFollowOns(order *orders.Order, seed deliveredSeed) {
	m.emitter.EmitOrderDelivered(order.ID, order.UUID, order.OrderType, order.ProcessNodeID, order.BinID,
		seed.binDestNode, order.DeliveryNode, seed.fromNotice)
	if order.AutoConfirm {
		m.DebugLog.Log("auto-confirm: id=%d uuid=%s qty=%d", order.ID, order.UUID, order.Quantity)
		if err := m.ConfirmDelivery(order.ID, order.Quantity); err != nil {
			log.Printf("orders: auto-confirm order %d: %v", order.ID, err)
		}
	}
}

// versioned reports whether an order's status is Core's versioned word: a
// versioned word of Core's has been applied to it. Its non-status messages then
// write no status (the file's header).
func versioned(order *orders.Order) bool { return order.CoreVersion > 0 }
