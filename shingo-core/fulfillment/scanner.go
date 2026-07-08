package fulfillment

import (
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
	// Re-check status. The scan set is {queued, sourcing} (commit 3b), so
	// re-verify the order is still acquiring (not cancelled/failed/dispatched
	// between listing and processing).
	current, err := s.db.GetOrder(order.ID)
	if err != nil || !protocol.IsAcquiring(current.Status) {
		return false
	}

	// Use the fresh copy for all subsequent operations.
	order = current

	// Precision point 2 (commit 3b): the widened scan set surfaces orders in
	// `sourcing`, but only COMPLEX orders are re-entrant from there today —
	// DispatchPreparedComplex re-resolves its steps and reclaims idempotently
	// (the same replay it already runs from `queued`). A simple retrieve / move /
	// store re-sourced from `sourcing` could double-claim (it may already hold a
	// bin from an interrupted attempt), so scope the sourcing re-attempt to
	// complex until commit 4's reserve-reconcile makes simple re-sourcing
	// idempotent. Simple orders are still processed normally from `queued`.
	if current.Status != protocol.StatusQueued && current.OrderType != protocol.OrderTypeComplex {
		return false
	}

	// Dropoff-capacity gate applies to SIMPLE orders only (retrieve,
	// move, retrieve_empty). Complex orders bypass entirely — their
	// resolved step plan is the gate. CheckDropoffCapacity blocks
	// any order whose destination has a bin currently sitting there,
	// but in two-robot consume the supply leg's destination (the
	// line) is by definition occupied by the bin a sibling evac
	// order is about to remove. Same shape for press-index, single-
	// robot swap, and any pattern where a complex order coordinates
	// with the destination's resident bin. Pre-fix the gate ran a
	// `pickupBeforeDropoffAt` bypass, but that only covered the
	// single-order swap pattern (same order picks up at the
	// destination); two-robot supply orders don't pick up at the
	// line and were left blocked. Plant 2026-05: two-robot supply
	// stuck queued.
	//
	// Simple orders keep the gate — single-leg, no choreography, the
	// gate prevents robot collisions on a shared destination.
	//
	// A7 (commit 3b): pass order.ID, not 0. The in-flight tally counts
	// `sourcing` orders, and with the scan set widened to include `sourcing`
	// a self-retrying order would count its OWN in-flight row and block itself
	// forever. Self-exclusion by order.ID prevents that (matches every
	// intake-side gate, which already passes order.ID).
	if order.OrderType != protocol.OrderTypeComplex {
		if blocked, reason := dispatch.CheckDropoffCapacity(s.db, order.DeliveryNode, order.ID); blocked {
			if order.QueueReason != reason {
				if err := s.db.SetOrderQueueReason(order.ID, reason); err != nil {
					s.logFn("fulfillment: set queue_reason for order %d: %v", order.ID, err)
				}
			}
			return false
		}
	}

	// Type-switch: complex orders flow through the dispatcher's prepared
	// path (resolved steps already on the order from intake). Simple
	// retrieve / retrieve_empty paths use the per-order bin-finding
	// logic below — unchanged from pre-Phase-4 except for the now-shared
	// capacity gate above.
	if order.OrderType == protocol.OrderTypeComplex {
		if err := s.dispatcher.DispatchPreparedComplex(order); err != nil {
			s.logFn("fulfillment: complex order %d dispatch failed: %v", order.ID, err)
			return false
		}
		// Clear stale queue_reason on success (DispatchPreparedComplex
		// also clears it after Lifecycle.Dispatch, but be defensive
		// against the rare path where it doesn't run to completion).
		if order.QueueReason != "" {
			s.db.SetOrderQueueReason(order.ID, "")
		}
		return true
	}

	// Store orders hold the bin they claimed at intake: planStore claims BEFORE
	// its capacity gate, then queues holding the claim (and terminal-fails if it
	// couldn't claim), so a queued store always owns its bin. Dispatch that bin;
	// never re-source. Re-finding would claim a second, wrong bin — FindSourceFIFO
	// excludes claimed bins, so it can't even return the order's own — or wedge
	// while still holding the first. [A5: the scanner had no store branch]
	if order.OrderType == protocol.OrderTypeStore {
		return s.dispatchClaimedStore(order)
	}

	payloadCode := order.PayloadCode
	// retrieve_empty and move are exempt. An empty is a generic carrier, so the
	// operator-agnostic loader request ("REQUEST EMPTY" on a manual_swap bin
	// loader) legitimately ships a blank payload_code; the empty finder sources
	// any compatible empty and leaves the order queued when none is available.
	// A payload-less MOVE is a direct relocation of the physical bin AT the
	// source node — the finder's concrete-node tier claims it regardless of
	// payload. Firing the guard for either turned a legitimate wait into a hard
	// "cannot match a source bin" failure: retrieve_empty spammed Springfield's
	// SMN_001 loader board, and a payload-less move was terminally failed the
	// moment its destination freed [A6]. A blank RETRIEVE stays a construction
	// bug (queued without the key the fulfiller needs).
	if payloadCode == "" && order.OrderType != protocol.OrderTypeRetrieveEmpty && order.OrderType != protocol.OrderTypeMove {
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
	if order.OrderType == protocol.OrderTypeRetrieveEmpty {
		intent = dispatch.IntentEmpty
	}
	res := s.finder.FindSource(order, intent)
	switch res.Outcome {
	case dispatch.OutcomeWait:
		s.setQueueReason(order, res.QueueReason)
		return false
	case dispatch.OutcomeReshuffle:
		// D27: the scanner does NOT spawn reshuffle compounds on replay yet
		// (tracked fast-follow — reshuffle planning still lives at intake). Stay
		// queued so the next tick re-evaluates once the lane clears; surface why.
		s.setQueueReason(order, "source bin buried; awaiting reshuffle")
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

	// D37: MoveToSourcing BEFORE the claim (commit-4 normalization — the intake
	// planners and the complex path both move-before-claim, so the simple family
	// matches). On claim failure the simple order MUST re-queue (sourcing→queued):
	// simple orders park in `queued` in 1c, and the scanner's complex-only scope
	// guard never retries one left in `sourcing` — that would be a permanent wedge.
	if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "bin found, claiming"); err != nil {
		s.logFn("fulfillment: order %d → sourcing: %v", order.ID, err)
	}

	// Claim the bin (reserve-then-claim; the guard requires a pending reservation).
	if err := s.claimer.ClaimForDispatch(bin.ID, order.ID, nil); err != nil {
		if s.debugLog != nil {
			s.debugLog("fulfillment: claim bin %d for order %d failed: %v", bin.ID, order.ID, err)
		}
		if qerr := s.lifecycle.Queue(order, "fulfillment", "claim contention, re-queued"); qerr != nil {
			s.logFn("fulfillment: order %d → queued after claim fail: %v", order.ID, qerr)
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

	// Resolve destination.
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		s.logFn("fulfillment: dest node %q not found for order %d: %v", order.DeliveryNode, order.ID, err)
		if rerr := s.db.ReleaseClaimByOrder(order.ID); rerr != nil {
			s.logFn("fulfillment: release claim for order %d on dest-node rollback: %v", order.ID, rerr)
		}
		if err := s.lifecycle.Queue(order, "fulfillment", "awaiting inventory"); err != nil {
			s.logFn("fulfillment: order %d → queued: %v", order.ID, err)
		}
		return false
	}

	// Dispatch to fleet — use DispatchDirect which handles fleet creation.
	// On failure, DispatchDirect sets status to failed. We override back to queued
	// since this is a transient fleet issue, not a permanent failure.
	vendorOrderID, err := s.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		s.logFn("fulfillment: fleet dispatch failed for order %d, re-queuing: %v", order.ID, err)
		if rerr := s.db.ReleaseClaimByOrder(order.ID); rerr != nil {
			s.logFn("fulfillment: release claim for order %d on fleet-fail rollback: %v", order.ID, rerr)
		}
		if err := s.lifecycle.Queue(order, "fulfillment", "fleet unavailable, re-queued"); err != nil {
			s.logFn("fulfillment: order %d → queued: %v", order.ID, err)
		}
		return false
	}

	s.logFn("fulfillment: order %d fulfilled — bin %d (%s -> %s) vendor=%s",
		order.ID, bin.ID, sourceNode.Name, destNode.Name, vendorOrderID)
	s.notifyEdgeDispatched(order, sourceNode, vendorOrderID)
	return true
}

// dispatchClaimedStore dispatches a queued store order using the bin it already
// claimed at intake (planStore). It never re-finds or re-claims — a store owns
// its bin across retries. On a transient fleet failure it re-queues WITHOUT
// releasing the claim (unlike a retrieve, whose bin is re-found next tick).
func (s *Scanner) dispatchClaimedStore(order *orders.Order) bool {
	if order.BinID == nil {
		// planStore terminal-fails when it can't claim, so a queued store always
		// holds a bin. A nil here is a construction bug — surface it, don't
		// dispatch a store with no bin (handleOrderCompleted would skip the
		// arrival update and the bin's location would go stale).
		s.logFn("fulfillment: store order %d reached the scanner with no claimed bin (planStore should have failed it); skipping", order.ID)
		return false
	}
	sourceNode, err := s.db.GetNodeByDotName(order.SourceNode)
	if err != nil {
		s.logFn("fulfillment: store order %d source node %q not found: %v", order.ID, order.SourceNode, err)
		return false
	}
	destNode, err := s.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		s.logFn("fulfillment: store order %d dest node %q not found: %v", order.ID, order.DeliveryNode, err)
		return false
	}
	// #115/#117: (re)secure the destination slot atomically before dispatching.
	// The winner claimed it at intake; a store that lost the intake race — or
	// whose slot filled — must NOT drop here. Requeue and wait politely, keeping
	// the claimed bin; the reserve→confirm re-attempts on the next tick.
	if err := s.dispatcher.SecureStoreSlot(order); err != nil {
		s.setQueueReason(order, "awaiting free storage slot")
		s.debugLog("fulfillment: store order %d holding — destination slot not secured: %v", order.ID, err)
		return false
	}
	if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "store dispatching held bin"); err != nil {
		s.logFn("fulfillment: store order %d → sourcing: %v", order.ID, err)
	}
	vendorOrderID, err := s.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		s.logFn("fulfillment: store order %d fleet dispatch failed, re-queuing (claim kept): %v", order.ID, err)
		if err := s.lifecycle.Queue(order, "fulfillment", "fleet unavailable, re-queued"); err != nil {
			s.logFn("fulfillment: store order %d → queued: %v", order.ID, err)
		}
		return false
	}
	s.logFn("fulfillment: store order %d fulfilled — bin %d (%s -> %s) vendor=%s",
		order.ID, *order.BinID, sourceNode.Name, destNode.Name, vendorOrderID)
	s.notifyEdgeDispatched(order, sourceNode, vendorOrderID)
	return true
}

// setQueueReason records the block reason on the order iff it changed (avoids a
// no-op write every tick a wait persists).
func (s *Scanner) setQueueReason(order *orders.Order, reason string) {
	if order.QueueReason == reason {
		return
	}
	if err := s.db.SetOrderQueueReason(order.ID, reason); err != nil {
		s.logFn("fulfillment: set queue_reason for order %d: %v", order.ID, err)
	}
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
