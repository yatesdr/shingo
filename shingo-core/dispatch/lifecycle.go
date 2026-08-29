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
// Side effects that need engine-level callbacks (sendToEdge, swap-peer
// unwind, etc.) stay on the EventBus — actions emit events via the
// existing Emitter interface; engine wiring subscribes and reacts. This
// keeps the dispatch package self-contained.

package dispatch

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store"
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

// ConcurrentTransition is returned when the order's status changed in the DB
// between the caller loading it and this transition persisting. The write is
// refused rather than applied on top of whatever landed first.
//
// The transition guard below validates against the caller's own snapshot
// (ord.Status), so it cannot see a status written by another goroutine since
// the load. The compare-and-swap in the store is what catches that, and this
// is how it surfaces. Callers that hold an order across a blocking step (the
// fulfillment scanner, notably) should treat it as "someone else owns this
// order now" and stop, not retry.
type ConcurrentTransition struct {
	OrderID  int64
	Expected protocol.Status
	To       protocol.Status
}

func (e ConcurrentTransition) Error() string {
	return fmt.Sprintf("order %d moved concurrently: expected %s, refusing write to %s",
		e.OrderID, e.Expected, e.To)
}

// IsConcurrentTransition reports whether err is a refused concurrent write.
func IsConcurrentTransition(err error) bool {
	var ct ConcurrentTransition
	return errors.As(err, &ct)
}

// Event is the audit/context payload for a transition. Not a routing
// key — the (from, to) pair routes; this carries data actions and audit
// need. PreviousStatus is set by transition() before firing actions.
type Event struct {
	Actor          string
	Reason         string
	PreviousStatus protocol.Status // populated by transition() before action dispatch
	StationID      string          // for emitter calls that need station context
	RobotID        string
	ReceiptType    string
	FinalCount     int64
	ErrorCode      string
	ErrorDetail    string
	// Ref is what the reason CONCERNS — node / payload / peer. Persisted to
	// order_history.ref as JSONB (migration 55). The live terminal-code
	// distribution is two values, so the code alone answers nothing and the
	// reference is what makes a reason actionable.
	Ref protocol.TermRef
}

// Action runs after the status update is persisted. Actions may write to
// the store, emit events, etc. Actions returning an error are LOGGED but
// do not roll back the transition.
//
// Actions are kept dispatch-internal — they may use s.db, s.backend,
// s.emitter, but not engine-level callbacks. Engine-side side effects
// (sendToEdge, swap-peer unwind) react to emitted events via the
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
	{from: StatusInTransit, to: StatusDelivered}:  {fireCompleted},
	{from: StatusStaged, to: StatusDelivered}:     {fireCompleted},
	{from: StatusDispatched, to: StatusDelivered}: {fireCompleted},

	// Confirm: edge confirmed receipt. Fires the order-completed event
	// (same reaction — the completion handler is idempotent).
	{from: StatusDelivered, to: StatusConfirmed}: {fireCompleted},

	// Compound parent reaching terminal: emit completed so wiring can
	// unlock the lane and clean up.
	{from: StatusReshuffling, to: StatusConfirmed}: {fireCompleted},

	// Complex-order resume: a compound that ran a buried-bin reshuffle
	// for a complex parent transitions the parent back to Queued (not
	// Confirmed) so the fulfillment scanner picks it up and re-resolves
	// its original pickup step against the now-accessible slot. The
	// scanner trigger is wired against EventOrderQueued
	// (engine/wiring.go:258-262, RunOnce synchronously); without this
	// emit the parent would sit Queued until the next periodic sweep.
	//
	// fireRequeued, NOT fireCompleted: fireCompleted runs the delivered/
	// bin-arrival handler in engine.handleOrderCompleted, which doesn't
	// apply to a parent that hasn't yet picked anything up.
	//
	// Sequencing dependency: this assumes fulfillment.RunOnce() is
	// invoked synchronously from the EventOrderQueued subscription. A
	// future async-scanner refactor that changes that contract would
	// silently break the in-band resume — see compound.go's
	// AdvanceCompoundOrder routing for the matching note.
	// AND fireResumed, WHICH IS THE HALF THAT TELLS THE EDGE. Ordered FIRST
	// deliberately: fireRequeued runs the fulfillment scanner in-band and the
	// order is frequently dispatched again inside that call, so a push queued
	// after it would race the statuses that follow. The Edge must learn
	// `queued` before anything later can be legal for it.
	//
	// Without it the Edge's mirror stayed at `reshuffling` — see fireResumed
	// for why EventOrderQueued's own Edge push cannot cover this.
	{from: StatusReshuffling, to: StatusQueued}: {fireResumed, fireRequeued},

	// Cancel paths from any non-terminal status notify engine wiring
	// via the EventBus cancellation event.
	{from: StatusPending, to: StatusCancelled}:      {fireCancelled},
	{from: StatusSourcing, to: StatusCancelled}:     {fireCancelled},
	{from: StatusQueued, to: StatusCancelled}:       {fireCancelled},
	{from: StatusAcknowledged, to: StatusCancelled}: {fireCancelled},
	{from: StatusDispatched, to: StatusCancelled}:   {fireCancelled},
	{from: StatusInTransit, to: StatusCancelled}:    {fireCancelled},
	{from: StatusStaged, to: StatusCancelled}:       {fireCancelled},
	{from: StatusDelivered, to: StatusCancelled}:    {fireCancelled},
	{from: StatusReshuffling, to: StatusCancelled}:  {fireCancelled},

	// Faulted: entered when fleet reports transient failure. Fires the
	// faulted event so engine wiring can start the grace timer.
	{from: StatusDispatched, to: StatusFaulted}:   {fireFaulted},
	{from: StatusAcknowledged, to: StatusFaulted}: {fireFaulted},
	{from: StatusInTransit, to: StatusFaulted}:    {fireFaulted},
	{from: StatusStaged, to: StatusFaulted}:       {fireFaulted},

	// Faulted outgoing: reuse existing events.
	{from: StatusFaulted, to: StatusInTransit}: {fireFaultedRecovered},
	{from: StatusFaulted, to: StatusDelivered}: {fireCompleted},
	{from: StatusFaulted, to: StatusFailed}:    {fireFailed},
	{from: StatusFaulted, to: StatusCancelled}: {fireCancelled},

	// Failure paths notify engine wiring via the EventBus failure event.
	// Delivered → Failed covers the rare post-delivery failure (crash
	// recovery, late detection of bad delivery) — the auto-return guard
	// at engine/wiring.go:153 prevents an unwanted return order for a bin
	// that already reached its destination.
	{from: StatusPending, to: StatusFailed}:      {fireFailed},
	{from: StatusSourcing, to: StatusFailed}:     {fireFailed},
	{from: StatusQueued, to: StatusFailed}:       {fireFailed},
	{from: StatusAcknowledged, to: StatusFailed}: {fireFailed},
	{from: StatusDispatched, to: StatusFailed}:   {fireFailed},
	{from: StatusInTransit, to: StatusFailed}:    {fireFailed},
	{from: StatusStaged, to: StatusFailed}:       {fireFailed},
	{from: StatusDelivered, to: StatusFailed}:    {fireFailed},
	{from: StatusReshuffling, to: StatusFailed}:  {fireFailed},

	// Skipped: dispatcher-side terminal for "the work was never needed".
	// Only the pre-fleet statuses can reach this — once the fleet owns the
	// order (Acknowledged onward) the resolution is fail or cancel.
	{from: StatusPending, to: StatusSkipped}:  {fireSkipped},
	{from: StatusSourcing, to: StatusSkipped}: {fireSkipped},
	{from: StatusQueued, to: StatusSkipped}:   {fireSkipped},
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

	// One chokepoint: every terminal target releases the order's claims +
	// reservations atomically via TerminalizeOrder; non-terminal targets are a
	// plain status write. Keying on IsTerminal (derived from validTransitions)
	// means a future terminal status can't silently skip release the way the
	// success terminal 'confirmed' used to on the old status switch.
	var err error
	if protocol.IsTerminal(to) {
		detail := ev.ErrorDetail
		if detail == "" {
			detail = ev.Reason
		}
		// Terminal writes CAS on "still live" (see store.TerminalizeOrder). A
		// loser still releases this order's claims + reservations — only the
		// status write and the history row belong to the winner — so a refusal
		// here never strands a hold.
		var won bool
		won, err = s.db.TerminalizeOrderWithReason(ord.ID, to, detail, s.historyReason(ord, to, ev))
		if err == nil && !won {
			return ConcurrentTransition{OrderID: ord.ID, Expected: from, To: to}
		}
	} else {
		detail := ev.Reason
		if detail == "" {
			detail = fmt.Sprintf("%s → %s by %s", from, to, ev.Actor)
		}
		// Compare-and-swap on `from`: the guard above only proves the
		// transition is legal from the status this caller LOADED. Another
		// goroutine may have moved (or terminalized) the order since, and an
		// unconditional write by id would clobber it — CI #497, where a
		// fulfillment tick's queued→sourcing resurrected an order the
		// recovery op had just cancelled.
		var moved bool
		moved, err = s.db.UpdateOrderStatusFromWithReason(ord.ID, string(from), string(to), detail, s.historyReason(ord, to, ev))
		if err == nil && !moved {
			return ConcurrentTransition{OrderID: ord.ID, Expected: from, To: to}
		}
	}
	if err != nil {
		return fmt.Errorf("persist %s→%s: %w", from, to, err)
	}
	ord.Status = to

	// Futility accounting. Placed here because transition() is the one
	// chokepoint every status write goes through, so the detector cannot be
	// bypassed by a new call site the way a per-caller hook could.
	s.noteFutility(ord, from, to, ev)

	for _, action := range actionMap[transitionKey{from, to}] {
		if err := action(s, ord, ev); err != nil {
			log.Printf("dispatch: action failed on %s→%s for order %d: %v", from, to, ord.ID, err)
		}
	}
	return nil
}

// historyReason assembles the typed reason for the order_history row.
//
// Event.ErrorCode and Event.Actor were already being set at every Fail and
// Skip call site and then dropped here — transition() passed only the prose
// detail to the store, so the categories existed in memory and never reached
// disk. This is the thread-through.
//
// On a →queued row the code is the QUEUE code, not a terminal one: the order
// is not ending, it is waiting, and orders.queue_code is overwritten in place
// so the history row is the only place that reason becomes a time series.
//
// The reference defaults to the order's own node and payload when the call
// site did not set one. That is not a guess — it is the node and payload the
// order is FOR, which is exactly what "no_source_bin" concerns.
func (s *LifecycleService) historyReason(ord *orders.Order, to protocol.Status, ev Event) store.HistoryReason {
	r := store.HistoryReason{Code: ev.ErrorCode, Actor: ev.Actor, Ref: ev.Ref}

	if to == StatusQueued {
		// A terminal code on a queued row would be a category error.
		r.Code = ord.QueueCode
	}

	if r.Ref.Empty() {
		r.Ref = protocol.TermRef{Node: ord.ProcessNode, Payload: ord.PayloadCode}
		if r.Ref.Node == "" {
			r.Ref.Node = ord.DeliveryNode
		}
	}
	return r
}

// noteFutility feeds the rate-per-tuple detector: in_transit resets the
// tuple, and a terminal that never reached in_transit increments it.
//
// No-op when the detector is disabled (nil), which is also the whole cost on
// a plant that has not turned it on.
func (s *LifecycleService) noteFutility(ord *orders.Order, from, to protocol.Status, ev Event) {
	if s.futility == nil {
		return
	}
	key := FutilityKey{
		StationID:   ord.StationID,
		ProcessNode: ord.ProcessNode,
		PayloadCode: ord.PayloadCode,
	}

	if to == StatusInTransit {
		s.futility.NoteInTransit(key)
		return
	}
	if !protocol.IsTerminal(to) {
		return
	}

	// Did THIS order ever get a robot moving? `from` is not enough to answer
	// it — an order can reach a terminal from half a dozen states, and
	// in_transit → staged → cancelled is not futile while
	// queued → cancelled is. Only the history distinguishes them.
	//
	// One indexed EXISTS per terminal. Terminals run ~50/day at a plant and
	// ~242/h at the worst moment on record, so the cost is not a
	// consideration; being wrong about which orders count is.
	reached, err := s.db.OrderEverReachedStatus(ord.ID, string(StatusInTransit))
	if err != nil {
		// Fail closed: do not count an order we could not classify. An
		// over-count here would put a false FUTILITY line in the journal,
		// which is worse than a missed one on an observe-only detector.
		s.dbg("futility: classify order %d: %v", ord.ID, err)
		return
	}
	if reached {
		return
	}

	reason := ev.Reason
	if reason == "" {
		reason = ev.ErrorDetail
	}
	s.futility.NoteFutileTerminal(key, ord.ID, string(to), reason)
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
//
// Idempotent for the reserve-retry loop: with MoveToSourcing-at-start, a complex
// order re-enters DispatchPreparedComplex every tick while it
// sources partials, firing sourcing→sourcing repeatedly. Skip that self-transition
// (rather than adding a self-edge to the transition table) so its action hooks
// don't re-run; genuinely illegal from-states are still rejected by transition().
func (s *LifecycleService) MoveToSourcing(ord *orders.Order, actor, reason string) error {
	if ord.Status == StatusSourcing {
		return nil
	}
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
	return s.FailWithRef(ord, stationID, errorCode, detail, protocol.TermRef{})
}

// FailWithRef is Fail with an explicit reference on the terminal row.
//
// Exists for grace expiry. An order that times out was faulted for the whole
// grace window and the fleet may have said why at the start of it; by the time
// the deadline fires the poller has dropped its entry (rds/poller.go) so the
// reason is gone from memory, and the terminal row is the last chance to keep
// it. A failed order that says "gave up after 45m · cannot replan (60011)" is a
// different artifact from one that says it timed out.
//
// An empty ref behaves exactly as before: historyReason fills in the order's own
// node and payload.
func (s *LifecycleService) FailWithRef(ord *orders.Order, stationID, errorCode, detail string, ref protocol.TermRef) error {
	if protocol.IsTerminal(ord.Status) {
		return IllegalTransition{From: ord.Status, To: StatusFailed}
	}
	return s.transition(ord, StatusFailed, Event{
		Actor:       "system:" + stationID,
		Reason:      detail,
		ErrorCode:   errorCode,
		ErrorDetail: detail,
		StationID:   stationID,
		Ref:         ref,
	})
}

// Skip transitions a dispatcher-side status (Pending|Sourcing|Submitted|Queued)
// to Skipped — the "the work was never needed" terminal. Distinct from Fail
// because skipped orders are not alarms: the world already advanced past
// the order's purpose (e.g. complex evac with no bin at any pickup node).
// Same atomic-write + bin-claim-release semantics as Fail (SkipOrderAtomic).
func (s *LifecycleService) Skip(ord *orders.Order, stationID, errorCode, detail string) error {
	if protocol.IsTerminal(ord.Status) {
		return IllegalTransition{From: ord.Status, To: StatusSkipped}
	}
	return s.transition(ord, StatusSkipped, Event{
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

// ResumeCompound transitions Reshuffling → Queued for a complex
// parent whose buried-bin reshuffle compound finished successfully.
// The parent then sits at Queued so the fulfillment scanner runs the
// original complex-order resolve+dispatch against the now-accessible
// source slot.
//
// Distinct method (not parameterized CompleteCompound) because the two
// have different downstream semantics: CompleteCompound terminates the
// parent (lane unlock + EmitOrderCompleted); ResumeCompound hands the
// parent back into the dispatch pipeline. AdvanceCompoundOrder routes
// by OrderType to pick the right one.
// ── IT CLEARS THE CAUSE, AND CLEARING IS THE RIGHT WRITE HERE ─────────────
//
// A resumed parent carries whatever cause parked it before the dig — "storage is
// being rearranged", lane-locked, intake-buried. Every one of those describes a
// wait that has now ENDED: the excavation it named is finished, which is why
// this function is being called. Leaving it puts a stale sentence on an order
// that is ready to dispatch, which is the same lie a blank row tells, told the
// other way round — the evaluator states the rule at its own release ("CLEARED
// ON ENTRY. The cause described a wait that is over").
//
// So this does NOT get a cause of its own. A resumed parent is not waiting on
// anything; it is queued and next in line for the scanner. If it turns out to be
// blocked, the very next pass writes the real reason. That makes a blank row on
// a resumed parent MEANINGFUL rather than ambient: one that persists past a
// scanner tick is an order nothing picked up, which is a finding.
func (s *LifecycleService) ResumeCompound(ord *orders.Order) error {
	if err := s.transition(ord, StatusQueued, Event{
		Actor:     "system",
		Reason:    "reshuffle complete; parent requeued for re-resolution",
		StationID: ord.StationID,
	}); err != nil {
		return err
	}
	if err := s.db.SetOrderQueueDetail(ord.ID, "", "", ""); err != nil {
		// Best-effort, like every other queue-detail write: a stale sentence is
		// worth a log line, never worth failing a completed reshuffle's resume.
		log.Printf("dispatch: clear queue_reason on resume for order %d: %v", ord.ID, err)
		return nil
	}
	ord.QueueReason, ord.QueueCode, ord.QueueCause = "", "", ""
	return nil
}

// MarkPending IS DELETED. It wrote Pending as an INITIAL status, bypassing
// transition() and its validity check, for three doors that were already AT
// pending — the INSERT set the column. Their own comments said what the call was
// actually for: "what this call is really for is the HISTORY row, which
// transitions write and inserts do not" (engine/bin_move.go).
//
// orders.Create writes that row now, in the same statement's transaction, for
// every door including the two that never had one (complex intake, compound
// children). So the pending→pending status write had nothing left to do, and
// keeping it would have put a second `pending` row on every order that used it.
//
// Worth stating rather than deleting silently, for the same reason MarkReshuffling
// below is: an entry-point write that skips the state machine is what a caller
// reaches for when a transition is refused, and the refusal is usually right.

// MarkReshuffling IS DELETED. It wrote Reshuffling as an INITIAL status,
// bypassing transition() and its validity check, for exactly one caller: the
// synthetic restore-blockers parent. That subsystem is gone — v70 dropped its
// table, a regression test keeps it dropped, and a boot one-shot cancels any
// leftover rows — so the bypass had no caller and existed only as an offer.
//
// Worth stating rather than deleting silently, because the SHAPE is the thing:
// an entry-point write that skips the state machine is exactly what a future
// caller reaches for when a transition is refused, and the refusal is usually
// right. Every live path into Reshuffling goes through BeginReshuffle, from
// Pending / Sourcing / Queued, which the transition table allows.

// ── Action implementations ──────────────────────────────────────────────

func fireCompleted(s *LifecycleService, ord *orders.Order, ev Event) error {
	s.emitter.EmitOrderCompleted(ord.ID, ord.EdgeUUID, ev.StationID)
	return nil
}

func fireCancelled(s *LifecycleService, ord *orders.Order, ev Event) error {
	s.emitter.EmitOrderCancelled(ord.ID, ord.EdgeUUID, ev.StationID, ev.Reason, string(ev.PreviousStatus))
	return nil
}

// fireRequeued emits EventOrderQueued so engine wiring runs the
// fulfillment scanner in-band. Wired into actionMap for
// {Reshuffling, Queued} — see ResumeCompound. PayloadCode is empty
// because the requeued parent's payload context is already on the
// order row; the scanner reads it from there.
func fireRequeued(s *LifecycleService, ord *orders.Order, ev Event) error {
	s.emitter.EmitOrderQueued(ord.ID, ord.EdgeUUID, ev.StationID, "")
	return nil
}

// fireResumed tells the EDGE that the parent left `reshuffling`.
//
// ── WHY fireRequeued COULD NOT DO IT ──────────────────────────────────────
//
// EventOrderQueued has an Edge-facing subscriber, and it is the QUEUE-REASON
// push (engine/wiring.go): it returns early unless the order still IsAcquiring
// AND carries a non-empty QueueReason, because its job is delivering a block
// sentence to the board, not mirroring a status. A resumed parent fails both
// halves — ResumeCompound clears the reason (the wait it named is over) and the
// in-band scanner usually dispatches it out of the acquiring set in the same
// millisecond. So nothing was ever sent.
//
// WHAT THAT COST, MEASURED. lane-stress 2026-08-11: Core walked
// reshuffling → queued → sourcing → dispatched → in_transit → staged while the
// Edge's mirror sat at `reshuffling`. Both later pushes were illegal jumps
// under the shared transition table (protocol/types.go) and were rejected, so
// the board never showed the order staged, never offered Release, and three
// robots stood still from the first minute of the run to the end of the soak.
// Core refused nothing; nobody ever asked it to.
//
// It is a plain status push, not a new mechanism: the same TypeOrderUpdate the
// completion path already sends, on the transition that had none.
func fireResumed(s *LifecycleService, ord *orders.Order, ev Event) error {
	s.emitter.EmitOrderResumed(ord.ID, ord.EdgeUUID, ev.StationID)
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

func fireSkipped(s *LifecycleService, ord *orders.Order, ev Event) error {
	code := ev.ErrorCode
	if code == "" {
		code = "lifecycle_skipped"
	}
	detail := ev.ErrorDetail
	if detail == "" {
		detail = ev.Reason
	}
	s.emitter.EmitOrderSkipped(ord.ID, ord.EdgeUUID, ev.StationID, code, detail)
	return nil
}

func fireFaulted(s *LifecycleService, ord *orders.Order, ev Event) error {
	s.emitter.EmitOrderFaulted(ord.ID, ord.EdgeUUID, ev.StationID, ev.Reason)
	return nil
}

func fireFaultedRecovered(s *LifecycleService, ord *orders.Order, ev Event) error {
	s.emitter.EmitOrderFaultedRecovered(ord.ID, ord.EdgeUUID, ev.StationID, ev.RobotID)
	return nil
}

// MarkFaulted transitions {Dispatched,Acknowledged,InTransit,Staged} to Faulted
// when the fleet reports a transient failure. The grace timer is handled by
// the engine wiring layer.
//
// ref carries the FLEET'S OWN REASON when it gave one — the vendor code and
// text off the order's errors[]. It arrives on the status event and was dropped
// here until 2026-08-22, which is why all 730 faulted rows in a 30-day window
// carry the identical detail and a NULL code. It is usually empty: about 94% of
// faulted orders have no errors[] entry, so the absence is the fleet's, not
// ours.
//
// The caller must fill ref.Node and ref.Payload itself when it sets any vendor
// field. historyReason only defaults those two when the ref is EMPTY, so a ref
// carrying just a vendor code would otherwise record why and lose where.
func (s *LifecycleService) MarkFaulted(ord *orders.Order, robotID string, ref protocol.TermRef, reason string) error {
	return s.transition(ord, StatusFaulted, Event{
		Actor:   "fleet",
		Reason:  reason,
		RobotID: robotID,
		Ref:     ref,
	})
}

// MarkFaultedRecovered transitions Faulted back to InTransit when the fleet
// recovers within the grace window.
//
// THIS IS NOW THE RECOVERY PATH. It had no callers: the fleet reporting RUNNING
// mapped to StatusInTransit and went through MarkInTransit, which writes
// "fleet reported in transit" — the same row a normal transit transition
// writes, making a recovery indistinguishable from an order that was never in
// trouble. 706 of 730 faults recover, so that was the common case recording
// nothing about itself.
//
// reason carries the dwell ("Recovered after 18 s"); ref is copied from the
// faulted row so the recovery says what it recovered FROM, which is the only
// place that reason survives once the order is moving again.
func (s *LifecycleService) MarkFaultedRecovered(ord *orders.Order, robotID string, ref protocol.TermRef, reason string) error {
	return s.transition(ord, StatusInTransit, Event{
		Actor:   "fleet",
		Reason:  reason,
		RobotID: robotID,
		Ref:     ref,
	})
}

// ── Derived status sets (Phase 6) ───────────────────────────────────────

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
