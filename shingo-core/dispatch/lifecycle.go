// lifecycle.go — Order state machine driver.
//
// LifecycleService gains typed methods (CancelOrder, ConfirmReceipt,
// Release, MarkInTransit, MarkStaged, MarkDelivered, Queue,
// MoveToSourcing, Dispatch, Fail, BeginReshuffle, MarkPending) that
// follow Derek's existing parameter pattern: caller supplies the loaded
// *orders.Order; the lifecycle validates the transition against
// protocol.validTransitions, persists atomically, then fires actions
// from actionMap.
//
// CancelOrder and ConfirmReceipt preserve their existing public
// signatures (the new typed methods replace the implementations, not the
// public API).
//
// Side effects that need engine-level callbacks (sendToEdge,
// maybeCreateReturnOrder, etc.) stay on the EventBus — actions emit
// events via the existing Emitter interface; engine wiring subscribes
// and reacts. This keeps the dispatch package self-contained.

package dispatch

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// IllegalTransition is returned when a (from, to) pair is not in
// protocol.validTransitions. errors.As-friendly.
type IllegalTransition struct {
	From protocol.Status
	To   protocol.Status
}

func (e IllegalTransition) Error() string {
	return fmt.Sprintf("illegal transition: %s → %s", e.From, e.To)
}

// IsIllegalTransition is a convenience for callers that want to branch on
// the error class without an explicit errors.As call.
func IsIllegalTransition(err error) bool {
	var it IllegalTransition
	return errors.As(err, &it)
}

// Event is the audit/context payload for a transition. Not a routing
// key — the (from, to) pair routes; this carries data actions and audit
// need. PreviousStatus is set by transition() before firing actions.
type Event struct {
	Actor          string
	Reason         string
	PreviousStatus protocol.Status // populated by transition() before action dispatch
	StationID      string // for emitter calls that need station context
	RobotID        string
	ReceiptType    string
	FinalCount     int64
	ErrorCode      string
	ErrorDetail    string
}

// Action runs after the status update is persisted. Actions may write to
// the store, emit events, etc. Actions returning an error are LOGGED but
// do not roll back the transition.
//
// Actions are kept dispatch-internal — they may use s.db, s.backend,
// s.emitter, but not engine-level callbacks. Engine-side side effects
// (sendToEdge, maybeCreateReturnOrder) react to emitted events via the
// EventBus subscription pattern.
type Action func(s *LifecycleService, ord *orders.Order, ev Event) error

// transitionKey is (from, to) — the action map key.
type transitionKey struct {
	from protocol.Status
	to   protocol.Status
}

// actionMap registers actions per (from, to) transition. Pure transitions
// (status update + audit only, no other side effects) do not need entries.
//
// Engine-side reactions live in the EventBus subscriptions in
// engine/wiring.go and engine/wiring_*.go. Actions here emit the events
// those subscriptions consume.
var actionMap = map[transitionKey][]Action{
	// Delivery: fleet-reported. Fires the order-completed event so engine
	// wiring can apply bin arrival, send the edge update, and run
	// completion logic in handleOrderCompleted.
	{from: StatusInTransit, to: StatusDelivered}:   {fireCompleted},
	{from: StatusStaged, to: StatusDelivered}:      {fireCompleted},
	{from: StatusDispatched, to: StatusDelivered}:  {fireCompleted},

	// Confirm: edge confirmed receipt. Fires the order-completed event
	// (same reaction — the completion handler is idempotent).
	{from: StatusDelivered, to: StatusConfirmed}: {fireCompleted},

	// Compound parent reaching terminal: emit completed so wiring can
	// unlock the lane and clean up.
	{from: StatusReshuffling, to: StatusConfirmed}: {fireCompleted},

	// Cancel paths from any non-terminal status notify engine wiring
	// via the EventBus cancellation event.
	{from: StatusPending, to: StatusCancelled}:      {fireCancelled},
	{from: StatusSourcing, to: StatusCancelled}:     {fireCancelled},
	{from: StatusQueued, to: StatusCancelled}:       {fireCancelled},
	{from: StatusSubmitted, to: StatusCancelled}:    {fireCancelled},
	{from: StatusAcknowledged, to: StatusCancelled}: {fireCancelled},
	{from: StatusDispatched, to: StatusCancelled}:   {fireCancelled},
	{from: StatusInTransit, to: StatusCancelled}:    {fireCancelled},
	{from: StatusStaged, to: StatusCancelled}:       {fireCancelled},
	{from: StatusDelivered, to: StatusCancelled}:    {fireCancelled},
	{from: StatusReshuffling, to: StatusCancelled}:  {fireCancelled},

	// Failure paths notify engine wiring via the EventBus failure event.
	// Delivered → Failed covers the rare post-delivery failure (crash
	// recovery, late detection of bad delivery) — the auto-return guard
	// at engine/wiring.go:153 prevents an unwanted return order for a bin
	// that already reached its destination.
	{from: StatusPending, to: StatusFailed}:      {fireFailed},
	{from: StatusSourcing, to: StatusFailed}:     {fireFailed},
	{from: StatusQueued, to: StatusFailed}:       {fireFailed},
	{from: StatusSubmitted, to: StatusFailed}:    {fireFailed},
	{from: StatusAcknowledged, to: StatusFailed}: {fireFailed},
	{from: StatusDispatched, to: StatusFailed}:   {fireFailed},
	{from: StatusInTransit, to: StatusFailed}:    {fireFailed},
	{from: StatusStaged, to: StatusFailed}:       {fireFailed},
	{from: StatusDelivered, to: StatusFailed}:    {fireFailed},
	{from: StatusReshuffling, to: StatusFailed}:  {fireFailed},
}

// transition is the shared driver. Validates (from, to) against
// protocol.validTransitions, persists the new status (atomically for
// terminal states, plain UpdateOrderStatus otherwise), then fires
// actionMap actions.
//
// Returns IllegalTransition if the transition is not allowed.
// Returns the store error if persistence fails (status unchanged).
// Action errors are logged but not returned — the transition has happened.
func (s *LifecycleService) transition(ord *orders.Order, to protocol.Status, ev Event) error {
	from := ord.Status
	if !protocol.IsValidTransition(from, to) {
		return IllegalTransition{From: from, To: to}
	}
	ev.PreviousStatus = from

	var err error
	switch to {
	case StatusCancelled:
		// CancelOrderAtomic writes status='cancelled' AND releases bin
		// claims in a single transaction. Preserves crash-safety.
		err = s.db.CancelOrderAtomic(ord.ID, ev.Reason)
	case StatusFailed:
		// FailOrderAtomic writes status='failed' AND releases bin claims
		// atomically. Same rationale.
		detail := ev.ErrorDetail
		if detail == "" {
			detail = ev.Reason
		}
		err = s.db.FailOrderAtomic(ord.ID, detail)
	default:
		detail := ev.Reason
		if detail == "" {
			detail = fmt.Sprintf("%s → %s by %s", from, to, ev.Actor)
		}
		err = s.db.UpdateOrderStatus(ord.ID, string(to), detail)
	}
	if err != nil {
		return fmt.Errorf("persist %s→%s: %w", from, to, err)
	}
	ord.Status = to

	for _, action := range actionMap[transitionKey{from, to}] {
		if err := action(s, ord, ev); err != nil {
			log.Printf("dispatch: action failed on %s→%s for order %d: %v", from, to, ord.ID, err)
		}
	}
	return nil
}

// ── Public typed methods ────────────────────────────────────────────────

// CancelOrder transitions any non-terminal status to Cancelled. Cancels
// the vendor order if active, then writes the new status atomically (with
// bin claim release). Caller supplies the loaded order, station ID for
// the emitter, and a reason string.
//
// Signature preserved from Derek's original. Internals now go through
// transition().
func (s *LifecycleService) CancelOrder(ord *orders.Order, stationID, reason string) {
	if protocol.IsTerminal(ord.Status) {
		// Idempotent: already terminal, nothing to do. Mirrors the
		// behaviour of the previous implementation (which silently
		// no-op'd via the inline terminal check).
		return
	}

	// Cancel the vendor leg first so we don't leave a robot moving for an
	// already-cancelled order. Fleet errors are logged but don't block
	// the local cancellation.
	if ord.VendorOrderID != "" {
		if err := s.backend.CancelOrder(ord.VendorOrderID); err != nil {
			log.Printf("dispatch: cancel vendor order %s: %v", ord.VendorOrderID, err)
			s.dbg("cancel fleet error: vendor_id=%s: %v", ord.VendorOrderID, err)
		} else {
			s.dbg("cancel fleet ok: vendor_id=%s", ord.VendorOrderID)
		}
	}

	if err := s.transition(ord, StatusCancelled, Event{
		Actor:     "system:" + stationID,
		Reason:    reason,
		StationID: stationID,
	}); err != nil {
		log.Printf("dispatch: cancel order %d: %v", ord.ID, err)
	}
}

// ConfirmReceipt transitions Delivered → Confirmed with a receipt.
// Idempotent: returns (false, nil) if the order is already completed.
//
// Signature preserved from Derek's original.
func (s *LifecycleService) ConfirmReceipt(ord *orders.Order, stationID, receiptType string, finalCount int64) (bool, error) {
	if ord.CompletedAt != nil {
		s.dbg("delivery receipt: uuid=%s already completed", ord.EdgeUUID)
		return false, nil
	}
	if err := s.transition(ord, StatusConfirmed, Event{
		Actor:       "edge:" + stationID,
		Reason:      fmt.Sprintf("receipt: %s, count: %d", receiptType, finalCount),
		ReceiptType: receiptType,
		FinalCount:  finalCount,
		StationID:   stationID,
	}); err != nil {
		return false, err
	}
	if err := s.db.CompleteOrder(ord.ID); err != nil {
		return false, fmt.Errorf("complete order %d: %w", ord.ID, err)
	}
	return true, nil
}

// Release transitions Staged → InTransit. Used by the complex order
// release-from-staging path. The wait-index increment and fleet release
// happen in the caller (complex.go); this just validates and persists
// the status change.
func (s *LifecycleService) Release(ord *orders.Order, actor string) error {
	return s.transition(ord, StatusInTransit, Event{
		Actor:  actor,
		Reason: fmt.Sprintf("released from staging (wait %d)", ord.WaitIndex),
	})
}

// MarkInTransit transitions to InTransit. Called by the wiring layer
// after fleet.MapState identifies the vendor state as in-transit.
func (s *LifecycleService) MarkInTransit(ord *orders.Order, robotID, actor string) error {
	return s.transition(ord, StatusInTransit, Event{
		Actor:   actor,
		Reason:  "fleet reported in transit",
		RobotID: robotID,
	})
}

// Acknowledge transitions Submitted|Queued → Acknowledged. Called by
// the wiring layer when the fleet ACKs a previously-submitted order.
// Pure transition — no side effects fire (the actionMap has no entry
// for any (*, Acknowledged) pair).
func (s *LifecycleService) Acknowledge(ord *orders.Order, actor string) error {
	return s.transition(ord, StatusAcknowledged, Event{
		Actor:  actor,
		Reason: "fleet acknowledged order",
	})
}

// MarkStaged transitions InTransit → Staged. Called when the fleet
// reports the robot is dwelling at a staging node.
func (s *LifecycleService) MarkStaged(ord *orders.Order, actor string) error {
	return s.transition(ord, StatusStaged, Event{
		Actor:  actor,
		Reason: "fleet reported dwelling at staging node",
	})
}

// MarkDelivered transitions {InTransit, Staged, Dispatched} → Delivered.
// Called when the fleet reports the order has been delivered.
func (s *LifecycleService) MarkDelivered(ord *orders.Order, actor string) error {
	return s.transition(ord, StatusDelivered, Event{
		Actor:  actor,
		Reason: "fleet reported delivered",
	})
}

// Queue transitions {Pending, Sourcing} → Queued. Used by the fulfillment
// scanner when an order is awaiting inventory.
func (s *LifecycleService) Queue(ord *orders.Order, actor, reason string) error {
	if reason == "" {
		reason = "awaiting inventory"
	}
	return s.transition(ord, StatusQueued, Event{
		Actor:  actor,
		Reason: reason,
	})
}

// MoveToSourcing transitions {Pending, Queued, Acknowledged, Dispatched}
// → Sourcing. Used by planning, redirect, and scanner re-resolve paths.
func (s *LifecycleService) MoveToSourcing(ord *orders.Order, actor, reason string) error {
	return s.transition(ord, StatusSourcing, Event{
		Actor:  actor,
		Reason: reason,
	})
}

// Dispatch transitions Queued|Acknowledged|Sourcing → Dispatched after
// the bin is resolved and the fleet order is created. Bin resolution and
// vendor order creation MUST complete before this is called.
func (s *LifecycleService) Dispatch(ord *orders.Order, vendorOrderID, actor string) error {
	return s.transition(ord, StatusDispatched, Event{
		Actor:  actor,
		Reason: fmt.Sprintf("vendor order %s created", vendorOrderID),
	})
}

// Fail transitions any non-terminal status to Failed via FailOrderAtomic
// (which also releases bin claims).
func (s *LifecycleService) Fail(ord *orders.Order, stationID, errorCode, detail string) error {
	if protocol.IsTerminal(ord.Status) {
		return IllegalTransition{From: ord.Status, To: StatusFailed}
	}
	return s.transition(ord, StatusFailed, Event{
		Actor:       "system:" + stationID,
		Reason:      detail,
		ErrorCode:   errorCode,
		ErrorDetail: detail,
		StationID:   stationID,
	})
}

// BeginReshuffle transitions {Pending, Sourcing} → Reshuffling for a
// compound parent order. Called from Pending when planning detects a
// buried bin at order intake; called from Sourcing when the planner has
// already moved the order through MoveToSourcing before discovering the
// buried bin via the resolver.
func (s *LifecycleService) BeginReshuffle(ord *orders.Order, reason string) error {
	return s.transition(ord, StatusReshuffling, Event{
		Actor:  "system",
		Reason: reason,
	})
}

// CompleteCompound transitions Reshuffling → Confirmed for a compound
// parent order whose children all completed successfully. Wraps the
// internal transition() driver with the canonical Reshuffling-complete
// event payload so the (Reshuffling, Confirmed) actionMap entry fires
// fireCompleted.
func (s *LifecycleService) CompleteCompound(ord *orders.Order) error {
	return s.transition(ord, StatusConfirmed, Event{
		Actor:     "system",
		Reason:    "reshuffle complete",
		StationID: ord.StationID,
	})
}

// MarkPending writes the initial Pending status for a freshly-created
// order. Entry-point write — no source status, bypasses transition
// validation. Used only by Create*Order methods at order intake.
func (s *LifecycleService) MarkPending(ord *orders.Order, reason string) error {
	if err := s.db.UpdateOrderStatus(ord.ID, string(StatusPending), reason); err != nil {
		return fmt.Errorf("mark pending order %d: %w", ord.ID, err)
	}
	ord.Status = StatusPending
	return nil
}

// ── Action implementations ──────────────────────────────────────────────

func fireCompleted(s *LifecycleService, ord *orders.Order, ev Event) error {
	s.emitter.EmitOrderCompleted(ord.ID, ord.EdgeUUID, ev.StationID)
	return nil
}

func fireCancelled(s *LifecycleService, ord *orders.Order, ev Event) error {
	s.emitter.EmitOrderCancelled(ord.ID, ord.EdgeUUID, ev.StationID, ev.Reason, string(ev.PreviousStatus))
	return nil
}

func fireFailed(s *LifecycleService, ord *orders.Order, ev Event) error {
	code := ev.ErrorCode
	if code == "" {
		code = "lifecycle_failed"
	}
	detail := ev.ErrorDetail
	if detail == "" {
		detail = ev.Reason
	}
	s.emitter.EmitOrderFailed(ord.ID, ord.EdgeUUID, ev.StationID, code, detail)
	return nil
}

// ── Derived status sets (Phase 6) ───────────────────────────────────────

// IsInFlight returns true for statuses where a robot is committed but
// the order has not reached its destination. Replaces the inline switch
// in engine/wiring_auto_return.go:54.
func IsInFlight(status protocol.Status) bool {
	switch status {
	case StatusDispatched, StatusAcknowledged, StatusInTransit, StatusStaged:
		return true
	}
	return false
}

// IsPostDelivery returns true if the bin is at (or past) the destination
// node. Replaces engine/wiring.go:153 and engine/orders.go:85.
//
// Note: a compound parent reaching StatusConfirmed via Reshuffling →
// Confirmed never went through Delivered. A bin was never assigned to the
// parent (children carry bin claims), so IsPostDelivery's "bin at
// destination" semantics don't apply to compound parents in any state.
// Callers that need to handle compound parents specially should check
// ParentOrderID != nil first.
func IsPostDelivery(status protocol.Status) bool {
	return status == StatusDelivered || status == StatusConfirmed
}
