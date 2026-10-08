package fulfillment

import (
	"errors"
	"fmt"
	"log"
	"shingocore/store/reservations"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/service"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Scanner monitors queued orders and fulfills them when matching
// inventory becomes available. Runs event-driven with a periodic
// safety sweep.
//
// Construct via NewScanner. The zero value is not usable.
//
// dispatcher and finder are narrow consumer-side interfaces
// (see dispatcher.go). The concrete types *dispatch.Dispatcher and
// *dispatch.SourceFinder satisfy them structurally, so the
// engine wires them in unchanged. Holding interfaces here is what
// lets scanner_test.go stub a one-method fake dispatcher / finder.
type Scanner struct {
	db         Store
	dispatcher Dispatcher
	lifecycle  Lifecycle
	finder     BinFinder
	claimer    Claimer
	sendToEdge func(msgType, stationID string, payload any) error
	// failFn fails an order in the DB AND emits EventOrderFailed so the
	// standard handler chain (audit, return order, edge notification) fires.
	// Wired at construction to engine.failOrderAndEmit. Without this, the
	// scanner's structural-error path silently terminates orders with no
	// audit trail, no Edge notification, and no bin recovery.
	failFn   func(orderID int64, code, detail string)
	logFn    func(string, ...any)
	debugLog func(string, ...any)

	scanMu sync.Mutex // serializes scan() calls
	// askedThisPass is the orders this RunOnce has asked and not dispatched,
	// guarded by scanMu; nil outside a RunOnce (AcquireOne asks once anyway).
	askedThisPass map[int64]bool

	triggerMu sync.Mutex
	pending   bool // coalesce triggers during a scan
	stopChan  chan struct{}
}

// NewScanner constructs a Scanner wired to the provided dependencies.
// See package doc for the role of each argument.
//
// dispatcher and finder are accepted as narrow interfaces. Callers
// (engine) continue to pass the concrete *dispatch.Dispatcher and
// *dispatch.SourceFinder — Go's structural typing handles the rest.
func NewScanner(
	db Store,
	dispatcher Dispatcher,
	lifecycle Lifecycle,
	finder BinFinder,
	claimer Claimer,
	sendFn func(string, string, any) error,
	failFn func(orderID int64, code, detail string),
	logFn func(string, ...any),
	debugLog func(string, ...any),
) *Scanner {
	return &Scanner{
		db:         db,
		dispatcher: dispatcher,
		lifecycle:  lifecycle,
		finder:     finder,
		claimer:    claimer,
		sendToEdge: sendFn,
		failFn:     failFn,
		logFn:      logFn,
		debugLog:   debugLog,
		stopChan:   make(chan struct{}),
	}
}

// Trigger requests a scan. If a scan is already running, the request
// is coalesced — the scanner will re-run after the current scan finishes.
func (s *Scanner) Trigger() {
	s.triggerMu.Lock()
	s.pending = true
	s.triggerMu.Unlock()
}

// RunOnce executes a single scan pass. Only one scan runs at a time.
func (s *Scanner) RunOnce() int {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	s.triggerMu.Lock()
	s.pending = false
	s.triggerMu.Unlock()

	// AN ORDER IS ASKED ONCE PER PASS. A pass triggered while it scanned scans
	// again (below), and an order it has just asked and parked was asked again
	// there and paid its whole acquisition again for a state that had not moved
	// for it: the order-22 shape, a pair re-parked 43 times in 3 s under one
	// cause. Every trigger also starts a fresh pass of its own
	// (engine.triggerFulfillment), so nothing waits on this rerun to see it.
	s.askedThisPass = map[int64]bool{}
	defer func() { s.askedThisPass = nil }()

	fulfilled := s.scan()

	// If events arrived during the scan, run again
	s.triggerMu.Lock()
	again := s.pending
	s.pending = false
	s.triggerMu.Unlock()
	if again {
		fulfilled += s.scan()
	}
	return fulfilled
}

// StartPeriodicSweep runs the scanner every interval as a safety net.
func (s *Scanner) StartPeriodicSweep(interval time.Duration) {
	go func() {
		ticker := clock.Default().NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopChan:
				return
			case <-ticker.C():
				s.RunOnce()
			}
		}
	}()
}

// Stop halts the periodic sweep.
func (s *Scanner) Stop() {
	close(s.stopChan)
}

// yieldLostHand is the scanner's answer when its hand lost the status CAS: another
// dispatcher (a hand door) took the order first and holds it. The CAS is the
// detector and the loser yields (SYNTH §2.4). The hand held nothing, and the
// fleet-refusal demote would clear every claim under the order's id, which are
// the winner's.
func (s *Scanner) yieldLostHand(order *orders.Order, err error) {
	s.logFn("fulfillment: order %d was dispatched by another caller first — yielding: %v", order.ID, err)
}

func (s *Scanner) scan() int {
	orders, err := s.db.ListAcquiringOrders()
	if err != nil {
		log.Printf("fulfillment: list acquiring orders: %v", err)
		return 0
	}
	if len(orders) == 0 {
		return 0
	}

	if s.debugLog != nil {
		s.debugLog("fulfillment: scanning %d queued orders", len(orders))
	}

	fulfilled := 0
	for _, order := range orders {
		if s.tryFulfill(order) {
			fulfilled++
		}
	}
	if fulfilled > 0 {
		s.logFn("fulfillment: fulfilled %d queued orders", fulfilled)
	}
	return fulfilled
}

// AcquireOne tries one order once, as a door births it, without walking the
// waiting set (the orc's Stop 2 ruling, 2026-09-26: acquiring is one act, v4
// §2.6, and a demand's arrival is a scheduling event, Rule 2). The periodic
// pass stays the re-asker for whatever this parks.
//
// It takes the scan lock, so it never runs beside a pass. Reports whether the
// order was dispatched.
func (s *Scanner) AcquireOne(orderID int64) bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	return s.acquireLocked(orderID, nil)
}

// Door runs a door's birth, apply and acquire under the scan lock, so a pass
// never sees a born row its door has not finished (RULINGS-C1-P0 F1;
// dispatch/door.go). Its fn calls AcquireLocked, never AcquireOne.
func (s *Scanner) Door(fn func()) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	fn()
}

// AcquireLocked is AcquireOne for a caller inside Door. pick, when not nil, is
// the source the door chose before its INSERT, used in place of finding one.
func (s *Scanner) AcquireLocked(orderID int64, pick *dispatch.SourceResult) bool {
	return s.acquireLocked(orderID, pick)
}

func (s *Scanner) acquireLocked(orderID int64, pick *dispatch.SourceResult) bool {
	order, err := s.db.GetOrder(orderID)
	if err != nil || !protocol.IsAcquiring(order.Status) {
		return false
	}
	if s.fulfill(order, pick) {
		return true
	}
	// A PAIR IS ONE UNIT OF WORK. The pair rule elects the lowest acquiring leg
	// to lead (dispatch.dispatchPairInOnePass), and a leg that is not the leader
	// defers and writes nothing. A pass reaches the leader on its own; a door's
	// one acquire does not, so it asks the leader here, under the same lock, and
	// the leader dispatches both legs or parks them. Without this a pair's second
	// leg waited for the next periodic pass (up to a minute) with no cause on it.
	if leader := s.pairLeaderToAsk(order); leader != nil {
		if !s.fulfill(leader, nil) {
			s.park(leader)
		}
	}
	s.park(order)
	if fresh, err := s.db.GetOrder(orderID); err == nil {
		return fresh.Status == protocol.StatusDispatched
	}
	return false
}

// pairLeaderToAsk returns the order's pair partner when it is the leg that leads
// this pair's pass — top-level, still acquiring, and the lower id — else nil.
func (s *Scanner) pairLeaderToAsk(order *orders.Order) *orders.Order {
	if order.SiblingOrderUUID == "" {
		return nil
	}
	sib, err := s.db.GetOrderByUUID(order.SiblingOrderUUID)
	if err != nil || sib == nil || sib.ParentOrderID != nil || sib.ID >= order.ID ||
		!protocol.IsAcquiring(sib.Status) {
		return nil
	}
	return sib
}

func (s *Scanner) tryFulfill(order *orders.Order) bool {
	// Re-check status. The scan set is {pending, queued} (the acquiring set),
	// so re-verify the order is still acquiring (not cancelled/failed/dispatched
	// between listing and processing).
	if s.askedThisPass[order.ID] {
		return false // asked and parked earlier in this pass (RunOnce)
	}
	current, err := s.db.GetOrder(order.ID)
	if err != nil || !protocol.IsAcquiring(current.Status) {
		return false
	}
	// Use the fresh copy for all subsequent operations.
	if s.fulfill(current, nil) {
		return true
	}
	if s.askedThisPass != nil {
		s.askedThisPass[current.ID] = true
	}
	s.park(current)
	return false
}

// park is the tail of an acquire that did not dispatch: a `pending` order whose
// arm wrote its cause moves to `queued`, and its station is told once, with
// that cause (dispatch.ParkAndTell's rule; the station hears a later change of
// cause through the wait-change notify). A `queued` order is already where it
// waits.
//
// AN ORDER LEFT `pending` WITH NO CAUSE IS NOT MOVED. `queued` always says why
// (C1.1.5): an arm that wrote nothing leaves the order where it was, to be asked
// on the next pass, and this says so loudly — it is a gap in that arm.
//
// The copy in hand may be stale when it still says `pending`: an arm that ended
// the order, planned its dig or lost it to another caller did so on its own
// read. So a `pending` copy is read again, and only an order still `pending` is
// parked.
func (s *Scanner) park(order *orders.Order) {
	if order.Status != protocol.StatusPending {
		return
	}
	fresh, err := s.db.GetOrder(order.ID)
	if err != nil || fresh.Status != protocol.StatusPending {
		return
	}
	if fresh.QueueCode == "" {
		s.logFn("fulfillment: order %d was asked and not dispatched, and no cause was written — "+
			"left pending for the next pass (find the arm that parks without a cause)", fresh.ID)
		return
	}
	if err := s.lifecycle.Park(fresh, "fulfillment"); err != nil {
		s.logTransition(fresh.ID, "pending → queued after its acquire", err)
		return
	}
	if s.dispatcher != nil {
		s.dispatcher.NotifyWaitChanged(fresh)
	}
}

// fulfill is one acquire attempt on an order already read fresh: a pass's
// acquiring order, or a door's just-born one. pick, when not nil, is the door's
// pre-INSERT source.
func (s *Scanner) fulfill(order *orders.Order, pick *dispatch.SourceResult) bool {
	// The wait the station holds, before the hand's `dispatched` write clears it:
	// a fleet refusal re-parks against it (DemoteAfterFleetRefusal).
	told := dispatch.WaitOf(order)

	// Stage-3 tripwire: a simple-family order must never be classified coordinated,
	// or the IsCoordinated discriminator below inverts (a plain order routed to the
	// role gate + fast-path). The invariant is the order.Coordinated COLUMN, not
	// steps-presence — a plain order MAY carry a plan; provenance is what separates
	// it from a coordinated leg. Loud construction-bug log; the OrderType read is a
	// legitimate assertion (Stage-5 forbidigo carve-out).
	//
	// Plain orders re-enter idempotently — a held bin is reused, never re-found
	// (see the BinID branch below) — so a waiting plain order safely re-dispatches.
	dispatch.AssertSimpleNotCoordinated(order)

	// Compound children (reshuffle legs) are dispatched by the compound machinery
	// (AdvanceCompoundOrder), sequentially, never by the scanner — skip them so a
	// scanner tick can't race that sequential dispatch. Keyed off the parent link
	// (a data property), not OrderType. A compound child is the only order carrying
	// a ParentOrderID.
	if order.ParentOrderID != nil {
		return false
	}

	// No re-entrancy guard: intake never claims (the scanner is the single
	// claimer, the model complex has always used), and a door's half-applied
	// birth cannot be reached because the door holds the scan lock across it
	// (dispatch/door.go). Simple and complex carry the same guards here (none).

	// ── Dispatch discriminator (Stage 3) ──
	// Dispatch no longer branches on OrderType; it keys on plan provenance.
	// IsCoordinated == the order carries an Edge-authored coordinated plan
	// (StepsJSON). Coordinated orders take the role gate (isConcreteStorageDropoff)
	// + the unconditional line/process-node fast-path INSIDE DispatchPreparedComplex
	// (reserveComplexDestination and its 2b05dce comment) — unchanged. Plain
	// single-transport orders take the full occupancy gate + node-driven reserve
	// below. This preserves today's exact split (StepsJSON!="" ⟺ OrderType==Complex)
	// while stopping the type read — including the no-wait complex changeover
	// orders (BuildReleaseSteps / StageSteps / StagedDeliverSteps / SequentialBackfill),
	// which stay coordinated (role gate + fast-path + their hard slot claim).
	if dispatch.IsCoordinated(order) {
		if err := s.dispatcher.DispatchPreparedComplex(order); err != nil {
			// Debug, not info: this fires on every transient hold — a swap leg
			// waiting on its sibling, a finder pool with no bin yet — and the
			// scanner retries on the next tick. Springfield emitted ~4,000/day
			// of it into the default logger, the same ~25k/day stream that
			// carries the TERMINAL "engine: order N failed:" line, burying the
			// signal at roughly 800:1.
			//
			// Nothing is lost by moving it: the dispatcher already logs the
			// same reason to the debug stream one frame deeper (1,997 vs 1,999
			// lines/day for the finder-pool-empty pair alone), so this line was
			// a duplicate that happened to be louder than the original.
			if s.debugLog != nil {
				s.debugLog("fulfillment: coordinated order %d dispatch failed: %v", order.ID, err)
			}
			return false
		}
		// Clear stale queue_reason on success (DispatchPreparedComplex also clears
		// it, but be defensive against the rare path that doesn't run to completion).
		if order.QueueReason != "" {
			s.setQueueReason(order, "", "", dispatch.QueueParams{})
		}
		return true
	}

	// ── PLAIN (single-transport) path ──
	// Full occupancy gate, relocated off the type split: a plain order is ALWAYS
	// full-gated. A simple retrieve to an occupied line stays gated — the fast-path
	// is structurally unreachable here (it lives only in the coordinated branch),
	// so it can't leak onto a single-transport order (the round-7 requirement).
	// order.ID self-exclusion (A7): the in-flight count reads every order holding a
	// claimed bin bound here (orders.InFlightForDropoffSQL), so an order retrying
	// after it has claimed must not count its own row.
	if blocked, cap := dispatch.CheckDropoffCapacity(s.db, order.DeliveryNode, order.ID); blocked {
		s.setQueueReason(order, protocol.QueueWaitingForSlot, cap.Cause, cap.Params)
		return false
	}

	// Source: an order already holding its bin — a store (claimed at intake) or a
	// retrieve/move that soft-held its bin on an earlier pass — dispatches the held
	// bin and never re-finds. This is the length-1 idempotency: re-entry reuses the claimed bin (FindSource is not consulted,
	// no second bin). Otherwise fall through to find + claim via the shared
	// SourceFinder. The node-driven destination reserve happens just before each
	// dispatch (in dispatchHeldBin here, and after the source claim in the finder
	// path below) — after the malformed-order guards, so an invalid order fails
	// without acquiring a reservation.
	if order.BinID != nil {
		return s.dispatchHeldBin(order)
	}

	payloadCode := order.PayloadCode
	// Empty-carrier and node-local sources are exempt from the payload key. An
	// empty is a generic carrier, so the operator-agnostic loader request
	// ("REQUEST EMPTY") legitimately ships a blank payload_code; a node-local
	// (move) source relocates the physical bin AT the source node — the finder's
	// concrete-node tier claims it regardless of payload. Firing the guard for
	// either turned a legitimate wait into a hard "cannot match a source bin"
	// failure [A6]. A blank payload on a full-payload (retrieve) source stays a
	// construction bug. Stage 4: keyed on SourceIntent data, not OrderType.
	if payloadCode == "" && order.SourceIntent != dispatch.SourceIntentEmpty && order.SourceIntent != dispatch.SourceIntentLocal {
		// Empty PayloadCode on a retrieve/move order is a construction
		// bug — the order was queued without the key the fulfiller
		// needs. Returning silently here would leave the order forever
		// queued with no operator-visible signal; route through the
		// standard failure path so the operator sees it.
		detail := "order has empty payload_code; cannot match a source bin"
		if s.failFn != nil {
			s.failFn(order.ID, "structural", detail)
		} else {
			s.logFn("fulfillment: order %d has empty payload_code but failFn not wired — order left in queued state, fix scanner construction",
				order.ID)
		}
		return false
	}

	// Source finding via the shared SourceFinder — the ONE seam intake planning
	// and this replay path both route through, so the scanner can't re-drift its
	// scoping from intake. The finder is pure (no claims/transitions); it returns
	// a closed outcome and, on Found, the bin AND its node together (which
	// deleted the old post-claim source-node re-resolve + its rollback).
	intent := dispatch.IntentFull
	if order.SourceIntent == dispatch.SourceIntentEmpty {
		intent = dispatch.IntentEmpty
	}
	var res dispatch.SourceResult
	planned := false
	if pick != nil {
		res, planned = *pick, true
	} else {
		res, planned = s.plannedSource(order)
	}
	if !planned {
		res = s.finder.FindSource(order, intent)
	}
	// mapFinderOutcome is the shared admission point: it fails loudly on an
	// unknown outcome instead of letting a default arm mis-file it.
	switch dispatch.MapFinderOutcome(res) {
	case dispatch.OutcomeWait:
		s.setQueueReason(order, res.QueueCode, res.QueueCause, res.QueueParams)
		return false
	case dispatch.OutcomeReshuffle:
		// Plan the reshuffle HERE, and only here. Intake queues a buried source
		// (planning_service.go parkBuried) and the pass the queued event runs plans it
		// before HandleOrderRequest returns. Burial also arises over TIME: an order
		// that queued with an accessible source can be buried by a later store while
		// it waits, and this arm is what unburies its lane.
		//
		// The dropoff gate above is the PRECONDITION, not an incidental ordering: a
		// simple-retrieve reshuffle compound IS the delivery, so it may only be planned
		// against a destination known clear. That holds here — same tick, same
		// goroutine, under scanMu. The destination then stays covered against other
		// orders for the whole compound, and not by the parent, which holds no claim:
		// compound.go gives the retrieve child the parent's delivery node, and the
		// child counts as inbound there (orders.InFlightForDropoffSQL) from creation
		// — as a dig leg awaiting the fleet until its handover, and by the claim its
		// hand takes after it.
		//
		// Return false and never advance the compound: createCompound already
		// dispatched the first child, and the parent has left the acquiring set
		// (→ reshuffling, IsAcquiring={pending,queued}), so no later pass
		// re-plans it. Two orders buried in the SAME lane in one pass are serialized by
		// planBuriedReshuffle's lane lock — the second gets ErrReshuffleWait.
		//
		// Being able to re-plan on a LATER tick is also what finally makes the buried
		// path wait-not-fail. "No free shuffle slot" is congestion, and at
		// intake it had nowhere to go but a terminal fail (sim order 21, 2026-07-10).
		// Here it just waits: ErrReshuffleWait keeps the order queued, and the next
		// tick tries again once a slot frees.
		if err := s.dispatcher.PlanBuriedReshuffle(order, res.Buried); err != nil {
			if errors.Is(err, dispatch.ErrReshuffleWait) {
				// Congestion — the lane is busy, no shuffle slot is free right now, or a
				// blocker is claimed by an order still carrying it out. Stay queued and
				// retry next tick. NEVER fail: the lane is not broken, it is crowded.
				//
				// The cause comes off the error rather than being written flat here: the
				// three waits clear on three different signals, and a row that says only
				// "congestion" cannot tell an engineer which one to go look at.
				// THE PARAMS COME OFF THE ERROR, like the cause. This site wrote
				// only the payload, so a lane-locked row named neither the corridor
				// nor the excavation holding it — the two facts an operator needs
				// and the only ones that make the wait actionable.
				s.setQueueReason(order, protocol.QueueStorageRearranging, dispatch.ReshuffleWaitCause(err),
					dispatch.ReshuffleWaitParams(err, order.PayloadCode))
				return false
			}
			if errors.Is(err, dispatch.ErrParentGone) {
				// The order ended while its dig was being planned (a cancel landed
				// after this pass read it). Nothing was written and nothing waits:
				// no fail on a terminal order, no queue reason on it.
				s.logFn("fulfillment: order %d ended while its dig was planned — no dig: %v", order.ID, err)
				return false
			}
			// Structural — real lane geometry (no parent group, bad target slot). Route
			// through failFn so the standard EventOrderFailed chain fires, the same
			// disposition intake gives a non-transient planning error.
			if s.failFn != nil {
				s.failFn(order.ID, "reshuffle", err.Error())
			} else {
				s.logFn("fulfillment: order %d reshuffle plan failed but failFn not wired — order left queued, fix scanner construction: %v",
					order.ID, err)
			}
			return false
		}
		s.logFn("fulfillment: order %d source buried — reshuffle compound planned on replay", order.ID)
		return false
	case dispatch.OutcomeStructural:
		// Terminal (permanent/config). Route through failFn so the standard
		// EventOrderFailed chain fires (audit, return order, edge notification).
		// A nil failFn in production is a wiring mistake — log loudly, don't mask.
		if s.failFn != nil {
			s.failFn(order.ID, "structural", res.Err.Error())
		} else {
			s.logFn("fulfillment: order %d structural error but failFn not wired — order left in queued state, fix scanner construction: %v",
				order.ID, res.Err)
		}
		return false
	}

	// OutcomeFound.
	bin, sourceNode := res.Bin, res.Node

	// ACQUIRING WRITES NO STATUS (v4 §2.6: an act, not a rung). An order that
	// waits after this point parks in `queued` with its cause, keeping its soft
	// reservations across passes (park, after fulfill returns). An order that ends
	// or is taken by another caller meanwhile is caught by the owner test in every
	// hold statement (reservations.OwnerLiveSQL) and by the hand's status CAS,
	// before any robot is asked.

	// Test seam: the deterministic claim-race hook fires at acquire time, between
	// Find and the soft reserve. Post the claim-move the
	// intake planner no longer claims, so this is the ONE find→acquire window — a
	// no-op in production, injected by concurrency tests to prove a race re-queues
	// (never drops).
	// Guarded because some scanner unit tests wire a nil dispatcher for the
	// fail path that never reaches dispatch; production always has a dispatcher.
	if s.dispatcher != nil {
		s.dispatcher.PostFindHook()
	}

	// ── Rule 1: soft until complete ───────────────────────────────────────
	// Slot FIRST, then bin — the same global order the complex path uses
	// (complex_dispatch slots → bins → confirm→fleet). Both are SOFT here (pending
	// reservations, no hard claimed_by); the hard claim for BOTH lands at dispatch,
	// in one confirm step, immediately before the fleet call. A soft-holding order
	// that hits any of the requeue branches below parks in `queued`, KEEPS its
	// soft holds, and is retried next tick (re-entering via the BinID branch, which
	// reuses — never re-finds — its own held bin).

	// Resolve destination BEFORE the slot reserve so the reserve targets a real
	// node. A storage dropoff reserves its slot soft (ReserveStorageDropoff); a line
	// dest is a no-op. On conflict, park under waiting_for_slot — no
	// bin has been acquired yet, so there is nothing to release.
	// The node value is deliberately discarded: ReserveStorageDropoff below SETTLES
	// the destination (group resolve, dug-lane re-aim) and returns the authoritative
	// one. This read stays for the distinct disposition it owns — a destination that
	// does not resolve at all parks under DestUnresolved, which the settle's generic
	// error would not say.
	if _, err := s.db.GetNodeByDotName(order.DeliveryNode); err != nil {
		s.logFn("fulfillment: dest node %q not found for order %d: %v", order.DeliveryNode, order.ID, err)
		// Destination node can't be resolved right now — re-queue and retry.
		// This is a DESTINATION failure, so it parks under waiting_for_slot with
		// DestUnresolved set, and the sentence says the node cannot be resolved.
		// It used to park under waiting_for_material purely to refresh the row,
		// which pointed the operator at inventory for a delivery-node lookup
		// failure (F6 in the 2026-07-20 queue-reason study).
		s.setQueueReason(order, protocol.QueueWaitingForSlot, dispatch.CauseDestNodeUnresolved,
			dispatch.QueueParams{Destination: order.DeliveryNode, DestUnresolved: true})
		return false
	}
	// destNode comes from the settle, not from a read taken before it.
	// THE CAUSE COMES FROM THE VERDICT. This site and the held-bin one below used
	// to derive it themselves, from the same four lines, and both got a hard read
	// failure wrong the same way — parking it as "the destination slot is
	// contended", which sends an operator to a slot that is probably empty.
	dest := s.dispatcher.ReserveStorageDropoff(order)
	if dest.Refused() {
		s.setQueueReason(order, protocol.QueueWaitingForSlot, dest.Cause,
			dispatch.QueueParams{Destination: order.DeliveryNode})
		return false
	}
	destNode := dest.Node

	// Bin SOFT-acquire: a pending reservation (no hard claim). On a lost race the
	// bin was reserved by a concurrent order — park under waiting_for_material and
	// retry. On success, stamp BinID NOW: an order holding a pending bin reservation
	// re-enters via the BinID branch next tick and reuses THIS bin (the owner-aware
	// keep arm — the finders exclude pending-reserved bins owner-blind, so re-finding
	// would shop a second bin and double-source). RemainingUOP is the operator's
	// release-correction count, persisted at intake; it is threaded through the
	// confirm at dispatch (nil for retrieve/retrieve_empty = a plain claim).
	if err := s.claimer.ReserveForDispatch(bin.ID, order.ID); err != nil {
		if errors.Is(err, reservations.ErrOwnerEnded) {
			return false // the order ended after this pass read it: nothing held, nothing to park
		}
		if errors.Is(err, reservations.ErrNotTheNamedBin) {
			s.skipForNamedBin(order, bin.ID, sourceNode.Name)
			return false
		}
		if s.debugLog != nil {
			s.debugLog("fulfillment: soft-reserve bin %d for order %d failed: %v", bin.ID, order.ID, err)
		}
		// The bin was reserved by a concurrent order in the Find→Reserve window —
		// the order IS waiting on material again, so park it under that code (the
		// race is the cause). The slot soft-reserve above is retained; it is
		// owner-aware and harmless to hold across the retry.
		s.setQueueReason(order, protocol.QueueWaitingForMaterial, dispatch.CauseBinLockRace,
			dispatch.QueueParams{Payload: order.PayloadCode})
		return false
	}

	// Error handling policy: log and continue. Do not add early returns without understanding the caller contract. See 2567plandiscussion.md.
	if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
		s.logFn("fulfillment: update bin_id for order %d: %v", order.ID, err)
	}
	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		s.logFn("fulfillment: update source_node for order %d: %v", order.ID, err)
	}
	// AND ON THE ORDER THIS PASS HANDS ON. The hand and the gated create read the
	// struct, not the row: a gated retrieve's classifier resolves its pickup from
	// order.SourceNode, and the source it was born with (blank, or a group) is not
	// the slot this bin stands on. It errored and left the order staged with no
	// cause for the floor to free (demo 103, 267, 409).
	order.BinID = &bin.ID
	order.SourceNode = sourceNode.Name

	// Lane admission (P4), with no row written. Every lane is marked, so the
	// in-lane questions are the gate's, asked at the tail append; what parks the
	// order here is a lane that could not be read. The mouth rows are taken in
	// the hand below.
	if !s.admitLanes(order, sourceNode, destNode, dispatch.EntryFreshBin) {
		return false
	}

	// THE HAND: the slot (if a storage dropoff), the bin, the lane mouths, the
	// occupancy and the `dispatched` status in ONE transaction, then the fleet
	// call (dispatch.DispatchHand). It replaces confirm-then-dispatch, whose two
	// confirms committed separately: a bin that refused after its slot confirmed
	// left the order waiting with the slot held hard. A refused hand holds
	// nothing; the order parks under the refusing leg's cause, keeping its soft
	// holds, and re-enters via the held-bin arm next tick.
	vendorOrderID, err := s.dispatcher.DispatchHand(order, bin.ID, sourceNode, destNode)
	if err != nil {
		if dispatch.IsConcurrentTransition(err) {
			s.yieldLostHand(order, err)
			return false
		}
		if s.endedOrSkipped(order, err, bin.ID, sourceNode.Name) {
			return false
		}
		if r, refused := dispatch.AsHandRefusal(err); refused {
			s.logFn("fulfillment: hand for order %d refused, re-queuing (soft holds kept): %v", order.ID, err)
			s.parkRefusedHand(order, r)
			return false
		}
		if s.statusWriteFailed(order, err) {
			return false
		}
		// A fleet that will not take an order right now is a transient
		// robot-system issue, not a permanent failure, so the refusal goes through
		// the one demote door: armor off, paper DEMOTED rather than deleted,
		// pointer kept. The retry re-confirms the pending row it kept — it does not
		// re-acquire, which is why deleting the paper here wedged the order (§8).
		s.logFn("fulfillment: fleet dispatch failed for order %d, re-queuing: %v", order.ID, err)
		// ONE DOOR. This arm used to call a release that DELETES the reservations
		// and the order_bins rows while leaving orders.bin_id
		// stamped — and the comment beside it said the order "re-soft-acquires
		// next tick", which nothing did. It re-entered through dispatchHeldBin,
		// which confirms by id and never re-acquires, and parked under
		// claim-failed forever. Armor off, paper demoted, pointer kept.
		code, cause, params := dispatch.FleetRefusalCause(err, order.DeliveryNode)
		s.dispatcher.DemoteAfterFleetRefusal(order, told, code, cause, params)
		return false
	}

	s.logFn("fulfillment: order %d fulfilled — bin %d (%s -> %s) vendor=%s",
		order.ID, bin.ID, sourceNode.Name, destNode.Name, vendorOrderID)
	s.notifyEdgeDispatched(order, sourceNode, vendorOrderID)
	return true
}

// endedOrSkipped answers a hand refused with something that is not a wait
// (dispatch.NotAWait), and reports whether err was one: an order that ended
// after this pass read it is left alone (nothing claimed, nothing to requeue,
// its row untouched), and a move whose named bin has left its source is
// skipped.
func (s *Scanner) endedOrSkipped(order *orders.Order, err error, found int64, source string) bool {
	switch {
	case errors.Is(err, reservations.ErrOwnerEnded):
		return true
	case errors.Is(err, reservations.ErrNotTheNamedBin):
		s.skipForNamedBin(order, found, source)
		return true
	}
	return false
}

// skipForNamedBin ends a move whose named bin has left its source. The hold or
// claim statement refused the bin it found (reservations.NamedBinSQL): it is not
// the bin the move was for, or that bin no longer stands on the source. Either
// way the move's work is gone, and waiting would only lift whatever lands there
// next. The skip releases whatever the order holds (SkipOrderAtomic).
func (s *Scanner) skipForNamedBin(order *orders.Order, found int64, source string) {
	named := int64(0)
	if order.NamedBinID != nil {
		named = *order.NamedBinID
	}
	detail := fmt.Sprintf("move %d was for bin %d, which is no longer on %s (found bin %d there)",
		order.ID, named, source, found)
	s.logFn("fulfillment: %s — skipped", detail)
	s.dispatcher.SkipOrder(order, string(protocol.TermNotNeeded), detail)
}

// plannedSource reads the order's birth pick (orders.plan, v147) in place of a
// pick: the planned bin and its node while the pick stands, one read where a
// pick is tens (SYNTH F4). No plan, a stale one or an unreadable answer and the
// caller picks, as every order did before the plan existed.
//
// The asker is the order itself: a plain order in this arm has no compound
// parent (those are skipped above), so it owns its own lanes.
func (s *Scanner) plannedSource(order *orders.Order) (dispatch.SourceResult, bool) {
	if order.Plan == nil {
		return dispatch.SourceResult{}, false
	}
	node, err := s.db.PlannedPickStands(order.Plan, domain.AskerFor(order.ID, order.ID))
	if err != nil {
		s.logFn("fulfillment: order %d: read its planned bin %d: %v — picking instead", order.ID, order.Plan.BinID, err)
		return dispatch.SourceResult{}, false
	}
	if node == nil {
		if s.debugLog != nil {
			s.debugLog("fulfillment: order %d: planned bin %d at %s no longer stands — picking again",
				order.ID, order.Plan.BinID, order.Plan.Node)
		}
		return dispatch.SourceResult{}, false
	}
	return dispatch.SourceResult{Outcome: dispatch.OutcomeFound, Bin: &bins.Bin{ID: order.Plan.BinID}, Node: node}, true
}

// dispatchHeldBin dispatches a plain order that already holds its source bin —
// a retrieve/move that soft-acquired its bin on a prior tick and parked in
// `queued`. It never re-finds or re-acquires (re-finding would shop a second
// bin — the finders exclude pending-reserved bins owner-blind, so the held bin's
// own reservation hides it). This is the owner-aware keep arm: the order's OWN
// held bin is reused. On a transient fleet failure it parks in `queued`,
// KEEPS its soft hold, and retries next tick (the same bin re-confirms and
// re-dispatches, owner-idempotent).
func (s *Scanner) dispatchHeldBin(order *orders.Order) bool {
	told := dispatch.WaitOf(order) // as in fulfill: what the station holds before the hand
	// ── THE THREE ARMS BELOW ALL USED TO RETURN WITH A BLANK ROW ─────────
	//
	// This is the highest-probability blank-cause producer in the system, and the
	// reason is the retry: dispatchHeldBin is where every order that already
	// holds its bin re-enters, so the scanner hits these arms EVERY TICK for as
	// long as the condition lasts. An order can sit here for hours with nothing
	// on the row explaining it, which is the "stuck in queued, no cause" residue
	// the liveness floor kept reporting as "(none)" — a gap in the arms, never in
	// the releaser inventory.
	if order.BinID == nil {
		// SHADOWED. "A construction bug" is true of a broken order and FALSE of a
		// coordinator, whose NULL bin_id is permanent and correct — so this arm
		// would file a fault against a folder. Reachability here was NOT
		// established by reading (the scanner's own coordinator skip and
		// reshuffling ∉ IsAcquiring both guard upstream), which is exactly why
		// the spelling is measured before it is cut.
		owns, oerr := s.db.OrderOwnsNoCargo(order.ID)
		service.NoteFolderShadow(service.FolderSiteHeldBinDispatch, order.ID, owns, oerr)
		// The plain path only routes here when order.BinID != nil, so a nil here
		// is a construction bug — surface it, don't dispatch with no bin.
		s.logFn("fulfillment: order %d reached dispatchHeldBin with no claimed bin; skipping", order.ID)
		s.setQueueReason(order, protocol.QueueWaitingForMaterial, dispatch.CauseHeldBinMissing,
			dispatch.QueueParams{Payload: order.PayloadCode})
		return false
	}
	sourceNode, err := s.db.GetNodeByDotName(order.SourceNode)
	if err != nil {
		s.logFn("fulfillment: held-bin order %d source node %q not found: %v", order.ID, order.SourceNode, err)
		s.setQueueReason(order, protocol.QueueWaitingForMaterial, dispatch.CauseReadFailed,
			dispatch.QueueParams{Payload: order.PayloadCode})
		return false
	}
	// Node value discarded — the settle below returns the authoritative one. The
	// read stays for the disposition it owns (see the fresh-bin arm).
	if _, err := s.db.GetNodeByDotName(order.DeliveryNode); err != nil {
		// The DESTINATION vocabulary, not the material one — the same distinction
		// the fresh path draws at its own dest-resolve arm (CauseDestNodeUnresolved,
		// F6 in the 2026-07-20 queue-reason study): parking a delivery-node lookup
		// failure under waiting_for_material points the operator at inventory for a
		// problem that has nothing to do with it.
		s.logFn("fulfillment: held-bin order %d dest node %q not found: %v", order.ID, order.DeliveryNode, err)
		s.setQueueReason(order, protocol.QueueWaitingForSlot, dispatch.CauseDestNodeUnresolved,
			dispatch.QueueParams{Destination: order.DeliveryNode, DestUnresolved: true})
		return false
	}
	// (Re)secure the destination slot reserve-only before dispatch (node-driven;
	// a no-op for non-storage dests). Owner-idempotent, so a store that reserved at
	// intake passes through; a loser (or a slot that filled) requeues holding its
	// bin, never dropping into an occupied slot (#115/#117, generalized).
	dest := s.dispatcher.ReserveStorageDropoff(order)
	if dest.Refused() {
		s.setQueueReason(order, protocol.QueueWaitingForSlot, dest.Cause,
			dispatch.QueueParams{Destination: order.DeliveryNode})
		if s.debugLog != nil {
			s.debugLog("fulfillment: held-bin order %d holding — destination not secured (%s): %v",
				order.ID, dest.Cause, dest.Err)
		}
		return false
	}
	destNode := dest.Node
	// PENDING → DISPATCHED for every order (SYNTH F8, C1.1.2): it goes from its
	// birth to the hand, so its history never says it waited when it did not.
	// Lane admission (P4): same as the fresh path — the physical question with no
	// row written; the mouth rows are taken in the hand. EntryHeldBin: this caller
	// skips nothing, because it never called the finder and so has no answer to
	// the reachability question. A held bin buried after its hold was taken is
	// the gate's to serve: its acceptance arm digs it out at the mark.
	if !s.admitLanes(order, sourceNode, destNode, dispatch.EntryHeldBin) {
		return false
	}
	// THE HAND, as on the fresh path: the held bin is still SOFT (pending
	// reservation from the prior tick); the slot, the bin, the lane mouths, the
	// occupancy and the status are taken in one transaction, or none of them is.
	vendorOrderID, err := s.dispatcher.DispatchHand(order, *order.BinID, sourceNode, destNode)
	if dispatch.IsConcurrentTransition(err) {
		s.yieldLostHand(order, err)
		return false
	}
	if s.endedOrSkipped(order, err, *order.BinID, sourceNode.Name) {
		return false
	}
	if r, refused := dispatch.AsHandRefusal(err); refused {
		// ── "KEEP THE HOLD AND RETRY" NEEDS THERE TO STILL BE A HOLD ──────
		//
		// The sentence above is the whole premise of this arm, and when it is false
		// the retry is not a wait — it is a loop with no releaser. the hand's bin leg
		// runs the claim seatbelt (reservations.HeldByOwnerSQL): a hard claim may
		// only land where THIS order already holds a live reservation. So an order
		// whose soft hold has been reaped, but whose orders.bin_id survived, fails
		// the seatbelt on every tick, forever, and never shops for another bin —
		// dispatchHeldBin never re-finds, by design.
		//
		// SIM 2026-08-31, order 22. Fifty-two sim-minutes waiting (then `sourcing`) under
		// claim-failed, logging this line several times a second:
		//
		//	confirm bin 4 for order 22: bin 4 is locked, already claimed,
		//	or does not exist
		//
		// Bin 4 was none of those things. It was `available`, unclaimed, unlocked,
		// and carried NO reservation at all — and neither did order 22. The message
		// lists the three conditions the SQL tests and omits the fourth, which was
		// the one that failed. compound.go names this shape exactly: "a wait, it is
		// a queued order with claim-failed on it and no releaser, forever."
		//
		// So ask which failure this is. A hold that still exists is the transient
		// case the arm was written for — somebody else got the bin first, retry,
		// the world changes. A hold that is GONE cannot come back on its own: the
		// pointer is the only thing keeping the order in this arm, and dropping it
		// routes the order through the finder next tick, which is the releaser.
		//
		// FAIL TOWARD THE RETRY on an unreadable reservations table. A wrong "keep"
		// costs a tick and asks again; a wrong "forget" throws away a live soft hold
		// and shops for a second bin the order does not need.
		//
		// A LANE refusal is not this question: the hold is not in doubt, a
		// corridor is busy. Only a slot or bin refusal asks whether the hold is
		// still there.
		if r.Leg != dispatch.HandLegLane && !s.stillHoldsBin(order) {
			s.logFn("fulfillment: held-bin order %d no longer holds a reservation on bin %d — its soft "+
				"hold was reaped and only the pointer survived, so the claim seatbelt can never open. "+
				"Forgetting the bin so the finder picks again.", order.ID, *order.BinID)
			if cerr := s.db.ClearOrderBinID(order.ID); cerr != nil {
				// Keep the old disposition: it retries, which is where it already was.
				s.logFn("fulfillment: order %d could not forget its stale bin pointer: %v", order.ID, cerr)
			}
			s.setQueueReason(order, protocol.QueueWaitingForMaterial, dispatch.CauseHeldBinMissing,
				dispatch.QueueParams{Payload: order.PayloadCode})
			return false
		}
		s.logFn("fulfillment: held-bin order %d hand refused, re-queuing (hold kept): %v", order.ID, err)
		s.parkRefusedHand(order, r)
		return false
	}
	if s.statusWriteFailed(order, err) {
		return false
	}
	if err != nil {
		s.logFn("fulfillment: held-bin order %d fleet dispatch failed, re-queuing (paper kept): %v", order.ID, err)
		// The same one door as the fresh-bin arm above, and this arm needed it
		// more: an order that is ALREADY holding its bin is the exact shape the
		// pointer wedge traps, because dispatchHeldBin is where it comes back to.
		code, cause, params := dispatch.FleetRefusalCause(err, order.DeliveryNode)
		s.dispatcher.DemoteAfterFleetRefusal(order, told, code, cause, params)
		return false
	}
	s.logFn("fulfillment: held-bin order %d fulfilled — bin %d (%s -> %s) vendor=%s",
		order.ID, *order.BinID, sourceNode.Name, destNode.Name, vendorOrderID)
	s.notifyEdgeDispatched(order, sourceNode, vendorOrderID)
	return true
}

// statusWriteFailed answers a hand whose `→ dispatched` write failed with a
// database error (dispatch.StatusWriteFailed): nothing was taken and the fleet
// was never asked, so it is not a fleet refusal. The order waits under
// read-failed, keeping its soft holds, and the next pass asks again.
func (s *Scanner) statusWriteFailed(order *orders.Order, err error) bool {
	var swf *dispatch.StatusWriteFailed
	if !errors.As(err, &swf) {
		return false
	}
	s.logFn("fulfillment: order %d: %v — waiting (soft holds kept)", order.ID, err)
	s.setQueueReason(order, protocol.QueueWaitingForMaterial, dispatch.CauseReadFailed,
		dispatch.QueueParams{Payload: order.PayloadCode})
	return true
}

// stillHoldsBin reports whether this order still has a live reservation on the
// bin its pointer names.
//
// It is the question "is the premise of the retry above still true". Release
// DELETES the row rather than marking it, so any bin row returned for this order
// is a live hold — pending or confirmed, both of which satisfy the claim
// seatbelt.
//
// TRUE ON A READ ERROR, deliberately, and it is the only direction that is safe
// here: the caller uses a false answer to THROW AWAY a soft hold, and doing that
// because the database blinked would send an order shopping for a second bin
// while it still owns the first.
func (s *Scanner) stillHoldsBin(order *orders.Order) bool {
	if order == nil || order.BinID == nil {
		return false
	}
	held, err := s.db.ListReservationsByOrder(order.ID)
	if err != nil {
		s.logFn("fulfillment: order %d — could not read its reservations (%v); assuming the hold "+
			"stands and retrying", order.ID, err)
		return true
	}
	for _, r := range held {
		if r.Kind == reservations.KindBin && r.BinID == *order.BinID {
			return true
		}
	}
	return false
}

// parkRefusedHand parks an order whose hand refused. The hand held nothing —
// its transaction rolled back — so the order keeps exactly its soft holds and
// waits under the refusing leg's code and cause, which are the ones the split
// confirm and the lane acquire wrote for the same refusals. The acquire's tail
// parks it (park).
func (s *Scanner) parkRefusedHand(order *orders.Order, r *dispatch.HandRefusal) {
	s.setQueueReason(order, r.Code, r.Cause, r.ParkParams(order))
}

// admitLanes asks lane admission before dispatch (P4). Returns true to proceed;
// false when the order waits on a lane that could not be read, so the caller
// returns false too.
//
// IT REFUSES NOTHING ELSE. Both plain callers declare that the gate owns the
// entry (dispatch.EntryKind), and every lane is marked, so admission admits every
// lane: the in-lane questions are asked at the tail append. The refusal arm, and
// the held-bin dig it fed (digForBuriedHeldBin), are gone with the unmarked lane.
func (s *Scanner) admitLanes(order *orders.Order, sourceNode, destNode *nodes.Node, kind dispatch.EntryKind) bool {
	if _, _, _, err := s.dispatcher.AdmitLanesForOrder(order, sourceNode, destNode, kind); err != nil {
		s.logFn("fulfillment: lane acquire for order %d errored: %v (retrying)", order.ID, err)
		s.setQueueReason(order, protocol.QueueWaitingForMaterial, dispatch.CauseLaneAcquireError,
			dispatch.QueueParams{Payload: order.PayloadCode})
		return false
	}
	return true
}

// logTransition reports a failed lifecycle transition from the scanner.
//
// A refused compare-and-swap (ConcurrentTransition) is not a failure — it is
// the CAS doing its job: another actor (operator cancel, a fail, a peer tick)
// terminalized or moved the order while this tick held a stale snapshot. It
// reads as information so a working guard doesn't look like breakage in the
// plant log. Everything else keeps its original wording.
//
// It does NOT decide control flow: call sites that must abandon the tick check
// dispatch.IsConcurrentTransition themselves and return. See fulfillOrder /
// fulfillOrderWithHeldBin, where continuing would dispatch a robot for an
// order that is no longer ours.
func (s *Scanner) logTransition(orderID int64, what string, err error) {
	if dispatch.IsConcurrentTransition(err) {
		s.logFn("fulfillment: order %d moved under us (%s) — another actor owns it now: %v", orderID, what, err)
		return
	}
	s.logFn("fulfillment: order %d %s: %v", orderID, what, err)
}

// setQueueReason is the scanner's door onto the queue-reason columns — the
// package-local name for dispatch.WriteQueueDetail, which holds the decision.
//
// s.logFn IS A STRUCT FIELD, NOT AN INJECTED PARAMETER, and that is what the
// door's log-sink argument exists for: the scanner writes to the plant log
// through a sink the process wires up, and the dispatch side writes through the
// standard logger. One body, two destinations.
func (s *Scanner) setQueueReason(order *orders.Order, code protocol.QueueCode, cause dispatch.QueueCause, params dispatch.QueueParams) {
	var notify func(*orders.Order)
	if s.dispatcher != nil {
		notify = s.dispatcher.NotifyWaitChanged
	}
	dispatch.WriteQueueDetail(s.db, s.logFn, "fulfillment", order, code, cause, params, notify)
}

// notifyEdgeDispatched sends the ack + waybill to Edge after a successful
// dispatch. Shared by the retrieve/move path and the store path.
func (s *Scanner) notifyEdgeDispatched(order *orders.Order, sourceNode *nodes.Node, vendorOrderID string) {
	// Only a station that sent the order or that it projects to is told (v4
	// §2.5; the orc's Stop 1 ruling): an unaware station has no row to ack.
	if order.StationID == "" || order.StationUnaware {
		return
	}
	if err := s.sendToEdge(protocol.TypeOrderAck, order.StationID, &protocol.OrderAck{
		OrderUUID:     order.EdgeUUID,
		ShingoOrderID: order.ID,
		SourceNode:    sourceNode.Name,
	}); err != nil {
		s.logFn("fulfillment: ack for order %d: %v", order.ID, err)
	}
	if err := s.sendToEdge(protocol.TypeOrderWaybill, order.StationID, &protocol.OrderWaybill{
		OrderUUID: order.EdgeUUID,
		WaybillID: vendorOrderID,
	}); err != nil {
		s.logFn("fulfillment: waybill for order %d: %v", order.ID, err)
	}
}

// pickupBeforeDropoffAt was a swap-pattern bypass for the
// dropoff-capacity gate. Removed 2026-05 along with the gate's
// application to complex orders — the bypass only covered single-
// order swaps (same order picks up at its own delivery), missing
// two-robot supply where the evac sibling owns the pickup. Complex
// orders now skip the gate entirely; the step planner is the
// choreography source of truth.
