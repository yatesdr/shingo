package fulfillment

import (
	"errors"
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
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

	scanMu    sync.Mutex // serializes scan() calls
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
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopChan:
				return
			case <-ticker.C:
				s.RunOnce()
			}
		}
	}()
}

// Stop halts the periodic sweep.
func (s *Scanner) Stop() {
	close(s.stopChan)
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

func (s *Scanner) tryFulfill(order *orders.Order) bool {
	// Re-check status. The scan set is {queued, sourcing} (the acquiring set),
	// so re-verify the order is still acquiring (not cancelled/failed/dispatched
	// between listing and processing).
	current, err := s.db.GetOrder(order.ID)
	if err != nil || !protocol.IsAcquiring(current.Status) {
		return false
	}

	// Use the fresh copy for all subsequent operations.
	order = current

	// Stage-3 tripwire: a simple-family order must never be classified coordinated,
	// or the IsCoordinated discriminator below inverts (a plain order routed to the
	// role gate + fast-path). The invariant is the order.Coordinated COLUMN, not
	// steps-presence — a plain order MAY carry a plan; provenance is what separates
	// it from a coordinated leg. Loud construction-bug log; the OrderType read is a
	// legitimate assertion (Stage-5 forbidigo carve-out).
	//
	// The old sourcing-reentry guard that lived here (scope sourcing re-attempts
	// to complex only, since simple re-sourcing could double-claim) is GONE: plain
	// orders now re-enter idempotently — a held bin is reused, never re-found (see
	// the BinID branch below) — so a `sourcing` plain order safely re-dispatches.
	dispatch.AssertSimpleNotCoordinated(order)

	// Compound children (reshuffle legs) are dispatched by the compound machinery
	// (AdvanceCompoundOrder), sequentially, never by the scanner — skip them so a
	// scanner tick can't race that sequential dispatch. Keyed off the parent link
	// (a data property), not OrderType. A compound child is the only order carrying
	// a ParentOrderID.
	if order.ParentOrderID != nil {
		return false
	}

	// The old re-entrancy guard here — skip a PLAIN order in `sourcing` with no
	// claimed bin — is RETIRED by the single-claimer change. It existed only
	// because simple had TWO bin-claimers (the intake planner AND this scanner), so
	// the scanner had to avoid pouncing while intake was mid-claim. Now intake
	// never claims (planTransport validates/resolves + queues; the scanner is the
	// single claimer, the model complex has always used), so that race is gone —
	// simple and complex carry the SAME guards here (none), the symmetry that
	// proves the
	// two-claimer smell is gone. Keeping the guard would be actively WRONG: the only
	// way a plain order now reaches `sourcing` with no bin is a crash between this
	// scanner's own MoveToSourcing and its claim, and skipping it forever would wedge
	// a recoverable order. sourcing→queued and queued→sourcing are both legal
	// (protocol/types.go) and IsAcquiring={queued,sourcing}, so a crashed-in-sourcing
	// order is re-scanned and the scanner's own claim is idempotent.

	// ── Dispatch discriminator (Stage 3) ──
	// Dispatch no longer branches on OrderType; it keys on plan provenance.
	// IsCoordinated == the order carries an Edge-authored coordinated plan
	// (StepsJSON). Coordinated orders take the role gate (isConcreteStorageDropoff)
	// + the unconditional line/process-node fast-path INSIDE DispatchPreparedComplex
	// (complex_dispatch.go:359 + the 2b05dce comment) — unchanged. Plain
	// single-transport orders take the full occupancy gate + node-driven reserve
	// below. This preserves today's exact split (StepsJSON!="" ⟺ OrderType==Complex)
	// while stopping the type read — including the no-wait complex changeover
	// orders (BuildReleaseSteps / StageSteps / StagedDeliverSteps / SequentialBackfill),
	// which stay coordinated (role gate + fast-path + their hard slot claim).
	if dispatch.IsCoordinated(order) {
		if err := s.dispatcher.DispatchPreparedComplex(order); err != nil {
			s.logFn("fulfillment: coordinated order %d dispatch failed: %v", order.ID, err)
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
	// order.ID self-exclusion (A7): the in-flight tally counts `sourcing` orders,
	// so a self-retrying order must not count its own row.
	if blocked, cap := dispatch.CheckDropoffCapacity(s.db, order.DeliveryNode, order.ID); blocked {
		s.setQueueReason(order, protocol.QueueWaitingForSlot, cap.Cause, cap.Params)
		return false
	}

	// Source: an order already holding its bin — a store (claimed at intake) or a
	// retrieve/move re-entered from `sourcing` — dispatches the held bin and never
	// re-finds. This is the length-1 idempotency that let the sourcing-reentry
	// guard dissolve: re-entry reuses the claimed bin (FindSource is not consulted,
	// no second bin). Otherwise fall through to find + claim via the shared
	// SourceFinder. The ★ node-driven destination reserve happens just before each
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
	res := s.finder.FindSource(order, intent)
	// mapFinderOutcome is the shared admission point: it fails loudly on an
	// unknown outcome instead of letting a default arm mis-file it.
	switch dispatch.MapFinderOutcome(res) {
	case dispatch.OutcomeWait:
		s.setQueueReason(order, res.QueueCode, res.QueueCause, res.QueueParams)
		return false
	case dispatch.OutcomeReshuffle:
		// Plan the reshuffle HERE, not only at intake. planTransport runs once, at
		// intake, but burial arises over TIME: an order that queued with an accessible
		// source — behind a full destination, or behind inventory — can be buried by a
		// later store while it waits. This scanner is the only thing that looks at it
		// again. Before this arm existed the order re-queued forever ("awaiting
		// reshuffle") and nothing in the system would ever unbury its lane.
		//
		// The dropoff gate above is the PRECONDITION, not an incidental ordering: a
		// simple-retrieve reshuffle compound IS the delivery, so it may only be planned
		// against a destination known clear. That holds here — same tick, same
		// goroutine, under scanMu. Once the parent flips to `reshuffling` it also
		// counts as in-flight inbound to its own delivery_node
		// (CountInFlightByDeliveryNode excludes only `queued` and terminal), so the
		// destination stays reserved against other orders for the whole compound.
		//
		// Return false and never advance the compound: createCompound already
		// dispatched the first child, and the parent has left the acquiring set
		// (queued → reshuffling, IsAcquiring={queued,sourcing}), so no later pass
		// re-plans it. Two orders buried in the SAME lane in one pass are serialized by
		// planBuriedReshuffle's lane lock — the second gets ErrReshuffleWait.
		//
		// Being able to re-plan on a LATER tick is also what finally makes the buried
		// path wait-not-fail (D18-Q4). "No free shuffle slot" is congestion, and at
		// intake it had nowhere to go but a terminal fail (sim order 21, 2026-07-10).
		// Here it just waits: ErrReshuffleWait keeps the order queued, and the next
		// tick tries again once a slot frees.
		if err := s.dispatcher.PlanBuriedReshuffle(order, res.Buried); err != nil {
			if errors.Is(err, dispatch.ErrReshuffleWait) {
				// Congestion — the lane is busy, or no shuffle slot is free right now.
				// Stay queued and retry next tick. NEVER fail: the lane is not broken,
				// it is crowded.
				s.setQueueReason(order, protocol.QueueStorageRearranging, "reshuffle-congestion",
					dispatch.QueueParams{Payload: order.PayloadCode})
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

	// MoveToSourcing BEFORE acquiring anything (the normalized timing — the intake
	// planners and the complex path both move-before-hold). A soft-holding order
	// lives in `sourcing` while it waits; the requeue paths below retarget to
	// `sourcing` (NEVER `queued`) so the order stays in the acquiring set and the
	// scanner retries it, holding its soft reservations across ticks.
	if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "bin found, soft-holding"); err != nil {
		s.logFn("fulfillment: order %d → sourcing: %v", order.ID, err)
	}

	// Test seam: the deterministic claim-race hook fires at acquire time (order now
	// in `sourcing`), between Find and the soft reserve. Post the claim-move the
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
	// that hits any of the requeue branches below parks in `sourcing`, KEEPS its
	// soft holds, and is retried next tick (re-entering via the BinID branch, which
	// reuses — never re-finds — its own held bin).

	// Resolve destination BEFORE the slot reserve so the reserve targets a real
	// node. A storage dropoff reserves its slot soft (ReserveStorageDropoff); a line
	// dest is a no-op. On conflict, park in sourcing under waiting_for_slot — no
	// bin has been acquired yet, so there is nothing to release.
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		s.logFn("fulfillment: dest node %q not found for order %d: %v", order.DeliveryNode, order.ID, err)
		// Destination node can't be resolved right now — re-queue and retry.
		// This is a DESTINATION failure, so it parks under waiting_for_slot with
		// DestUnresolved set, and the sentence says the node cannot be resolved.
		// It used to park under waiting_for_material purely to refresh the row,
		// which pointed the operator at inventory for a delivery-node lookup
		// failure (F6 in the 2026-07-20 queue-reason study).
		s.setQueueReason(order, protocol.QueueWaitingForSlot, "dest-node-unresolved",
			dispatch.QueueParams{Destination: order.DeliveryNode, DestUnresolved: true})
		if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "dest unresolved, retrying"); err != nil {
			s.logFn("fulfillment: order %d → sourcing after dest resolve fail: %v", order.ID, err)
		}
		return false
	}
	if err := s.dispatcher.ReserveStorageDropoff(order); err != nil {
		s.setQueueReason(order, protocol.QueueWaitingForSlot, "slot-reserved",
			dispatch.QueueParams{Destination: order.DeliveryNode})
		if qerr := s.lifecycle.MoveToSourcing(order, "fulfillment", "destination slot contended"); qerr != nil {
			s.logFn("fulfillment: order %d → sourcing after reserve conflict: %v", order.ID, qerr)
		}
		return false
	}

	// Bin SOFT-acquire: a pending reservation (no hard claim). On a lost race the
	// bin was reserved by a concurrent order — park under waiting_for_material and
	// retry. On success, stamp BinID NOW: an order holding a pending bin reservation
	// re-enters via the BinID branch next tick and reuses THIS bin (the owner-aware
	// keep arm — the finders exclude pending-reserved bins owner-blind, so re-finding
	// would shop a second bin and double-source). RemainingUOP is the operator's
	// release-correction count, persisted at intake; it is threaded through the
	// confirm at dispatch (nil for retrieve/retrieve_empty = a plain claim).
	if err := s.claimer.ReserveForDispatch(bin.ID, order.ID); err != nil {
		if s.debugLog != nil {
			s.debugLog("fulfillment: soft-reserve bin %d for order %d failed: %v", bin.ID, order.ID, err)
		}
		// The bin was reserved by a concurrent order in the Find→Reserve window —
		// the order IS waiting on material again, so park it under that code (the
		// race is the cause). The slot soft-reserve above is retained; it is
		// owner-aware and harmless to hold across the retry.
		s.setQueueReason(order, protocol.QueueWaitingForMaterial, "lock-race",
			dispatch.QueueParams{Payload: order.PayloadCode})
		if qerr := s.lifecycle.MoveToSourcing(order, "fulfillment", "bin race, retrying"); qerr != nil {
			s.logFn("fulfillment: order %d → sourcing after reserve fail: %v", order.ID, qerr)
		}
		return false
	}

	// Error handling policy: log and continue. Do not add early returns without understanding the caller contract. See 2567plandiscussion.md.
	if err := s.db.UpdateOrderBinID(order.ID, bin.ID); err != nil {
		s.logFn("fulfillment: update bin_id for order %d: %v", order.ID, err)
	}
	if err := s.db.UpdateOrderSourceNode(order.ID, sourceNode.Name); err != nil {
		s.logFn("fulfillment: update source_node for order %d: %v", order.ID, err)
	}

	// Confirm-at-dispatch: hard-claim BOTH the slot (if a storage dropoff) AND the
	// bin, in one step, immediately before the fleet call. Slots-before-bins (the
	// complex order). On failure the order keeps its soft holds and parks in
	// sourcing under the failing leg's code; next tick it re-enters via the BinID
	// branch and re-confirms (owner-idempotent).
	if err := s.dispatcher.ConfirmForDispatch(order, bin.ID, sourceNode, destNode); err != nil {
		s.logFn("fulfillment: confirm-at-dispatch for order %d failed: %v", order.ID, err)
		s.setQueueReason(order, protocol.QueueWaitingForMaterial, "claim-failed",
			dispatch.QueueParams{Payload: order.PayloadCode})
		if qerr := s.lifecycle.MoveToSourcing(order, "fulfillment", "confirm failed, retrying"); qerr != nil {
			s.logFn("fulfillment: order %d → sourcing after confirm fail: %v", order.ID, qerr)
		}
		return false
	}

	// Dispatch to fleet — use DispatchDirect which handles fleet creation.
	// On failure, DispatchDirect sets status to failed. We override back to sourcing
	// since this is a transient fleet issue, not a permanent failure, and release
	// the now-hard claim so the requeue re-soft-acquires next tick.
	vendorOrderID, err := s.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		s.logFn("fulfillment: fleet dispatch failed for order %d, re-queuing: %v", order.ID, err)
		if rerr := s.db.ReleaseClaimByOrder(order.ID); rerr != nil {
			s.logFn("fulfillment: release claim for order %d on fleet-fail rollback: %v", order.ID, rerr)
		}
		// Fleet rejected the dispatch — a transient robot-system issue. Park under
		// fleet_unavailable so the row carries that code.
		s.setQueueReason(order, protocol.QueueFleetUnavailable, "fleet-error", dispatch.QueueParams{})
		if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "fleet unavailable, retrying"); err != nil {
			s.logFn("fulfillment: order %d → sourcing after fleet fail: %v", order.ID, err)
		}
		return false
	}

	s.logFn("fulfillment: order %d fulfilled — bin %d (%s -> %s) vendor=%s",
		order.ID, bin.ID, sourceNode.Name, destNode.Name, vendorOrderID)
	s.notifyEdgeDispatched(order, sourceNode, vendorOrderID)
	return true
}

// dispatchHeldBin dispatches a plain order that already holds its source bin —
// a retrieve/move that soft-acquired its bin on a prior tick and parked in
// `sourcing`. It never re-finds or re-acquires (re-finding would shop a second
// bin — the finders exclude pending-reserved bins owner-blind, so the held bin's
// own reservation hides it). This is the owner-aware keep arm: the order's OWN
// held bin is reused. On a transient fleet failure it parks in `sourcing`,
// KEEPS its soft hold, and retries next tick (the same bin re-confirms and
// re-dispatches, owner-idempotent).
func (s *Scanner) dispatchHeldBin(order *orders.Order) bool {
	if order.BinID == nil {
		// The plain path only routes here when order.BinID != nil, so a nil here
		// is a construction bug — surface it, don't dispatch with no bin.
		s.logFn("fulfillment: order %d reached dispatchHeldBin with no claimed bin; skipping", order.ID)
		return false
	}
	sourceNode, err := s.db.GetNodeByDotName(order.SourceNode)
	if err != nil {
		s.logFn("fulfillment: held-bin order %d source node %q not found: %v", order.ID, order.SourceNode, err)
		return false
	}
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		s.logFn("fulfillment: held-bin order %d dest node %q not found: %v", order.ID, order.DeliveryNode, err)
		return false
	}
	// ★ (Re)secure the destination slot reserve-only before dispatch (node-driven;
	// a no-op for non-storage dests). Owner-idempotent, so a store that reserved at
	// intake passes through; a loser (or a slot that filled) requeues holding its
	// bin, never dropping into an occupied slot (#115/#117, generalized).
	if err := s.dispatcher.ReserveStorageDropoff(order); err != nil {
		s.setQueueReason(order, protocol.QueueWaitingForSlot, "slot-reserved",
			dispatch.QueueParams{Destination: order.DeliveryNode})
		if s.debugLog != nil {
			s.debugLog("fulfillment: held-bin order %d holding — destination slot not secured: %v", order.ID, err)
		}
		return false
	}
	if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "dispatching held bin"); err != nil {
		s.logFn("fulfillment: held-bin order %d → sourcing: %v", order.ID, err)
	}
	// Confirm-at-dispatch: the held bin is still SOFT (pending reservation from the
	// prior tick). Hard-claim the slot (if a storage dropoff) AND the bin here, one
	// step, before the fleet call — same Rule-1 step as the fresh path. On failure
	// keep the soft holds and park in sourcing; next tick re-confirms.
	if err := s.dispatcher.ConfirmForDispatch(order, *order.BinID, sourceNode, destNode); err != nil {
		s.logFn("fulfillment: held-bin order %d confirm-at-dispatch failed, re-queuing (hold kept): %v", order.ID, err)
		s.setQueueReason(order, protocol.QueueWaitingForMaterial, "claim-failed",
			dispatch.QueueParams{Payload: order.PayloadCode})
		if qerr := s.lifecycle.MoveToSourcing(order, "fulfillment", "confirm failed, retrying"); qerr != nil {
			s.logFn("fulfillment: held-bin order %d → sourcing after confirm fail: %v", order.ID, qerr)
		}
		return false
	}
	vendorOrderID, err := s.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		s.logFn("fulfillment: held-bin order %d fleet dispatch failed, re-queuing (claim released): %v", order.ID, err)
		if rerr := s.db.ReleaseClaimByOrder(order.ID); rerr != nil {
			s.logFn("fulfillment: release claim for held-bin order %d on fleet-fail rollback: %v", order.ID, rerr)
		}
		// Same fleet_unavailable code as the plain-path fleet failure; both are
		// transient robot-system issues. The hard claim is released so the order
		// re-soft-acquires next tick.
		s.setQueueReason(order, protocol.QueueFleetUnavailable, "fleet-error", dispatch.QueueParams{})
		if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "fleet unavailable, retrying"); err != nil {
			s.logFn("fulfillment: held-bin order %d → sourcing after fleet fail: %v", order.ID, err)
		}
		return false
	}
	s.logFn("fulfillment: held-bin order %d fulfilled — bin %d (%s -> %s) vendor=%s",
		order.ID, *order.BinID, sourceNode.Name, destNode.Name, vendorOrderID)
	s.notifyEdgeDispatched(order, sourceNode, vendorOrderID)
	return true
}

// setQueueReason is the scanner's one door onto the queue-reason columns. It
// generates the operator sentence from code+params (via the shared formatter),
// then writes sentence+code+cause together — so a wait parked here always
// records the structured code, never free text. No-ops when the sentence is
// unchanged (avoids a re-touch every tick a wait persists, which can re-trigger
// the scanner). cause is the engineer-only call-site tag; params carries the
// values the sentence is built from and is discarded after formatting.
func (s *Scanner) setQueueReason(order *orders.Order, code protocol.QueueCode, cause string, params dispatch.QueueParams) {
	reason := dispatch.FormatQueueSentence(code, params)
	if order.QueueReason == reason && order.QueueCode == string(code) && order.QueueCause == cause {
		return
	}
	if err := s.db.SetOrderQueueDetail(order.ID, reason, code, cause); err != nil {
		s.logFn("fulfillment: set queue_reason for order %d: %v", order.ID, err)
		return
	}
	order.QueueReason = reason
	order.QueueCode = string(code)
	order.QueueCause = cause
}

// notifyEdgeDispatched sends the ack + waybill to Edge after a successful
// dispatch. Shared by the retrieve/move path and the store path.
func (s *Scanner) notifyEdgeDispatched(order *orders.Order, sourceNode *nodes.Node, vendorOrderID string) {
	if order.StationID == "" {
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
