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
			s.db.SetOrderQueueReason(order.ID, "")
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
	if blocked, reason := dispatch.CheckDropoffCapacity(s.db, order.DeliveryNode, order.ID); blocked {
		if order.QueueReason != reason {
			if err := s.db.SetOrderQueueReason(order.ID, reason); err != nil {
				s.logFn("fulfillment: set queue_reason for order %d: %v", order.ID, err)
			}
		}
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
	switch res.Outcome {
	case dispatch.OutcomeWait:
		s.setQueueReason(order, res.QueueReason)
		return false
	case dispatch.OutcomeReshuffle:
		// The scanner does NOT spawn reshuffle compounds on replay yet
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

	// MoveToSourcing BEFORE the claim (the normalized timing — the intake
	// planners and the complex path both move-before-claim, so the simple family
	// matches). On claim failure the simple order MUST re-queue (sourcing→queued):
	// simple orders park in `queued`, and the scanner's complex-only scope guard
	// never retries one left in `sourcing` — that would be a permanent wedge.
	if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "bin found, claiming"); err != nil {
		s.logFn("fulfillment: order %d → sourcing: %v", order.ID, err)
	}

	// Test seam: the deterministic claim-race hook fires at claim time (order now
	// in `sourcing`), between Find and Claim. Post the claim-move the intake
	// planner no longer claims, so this is the ONE find→claim window — a no-op in
	// production, injected by concurrency tests to prove a claim race re-queues
	// (never drops).
	// Guarded because some scanner unit tests wire a nil dispatcher for the claim-
	// fail path that never reaches dispatch; production always has a dispatcher.
	if s.dispatcher != nil {
		s.dispatcher.PostFindHook()
	}

	// Claim the bin (reserve-then-claim; the guard requires a pending reservation).
	// RemainingUOP is the operator's declared release-correction count, persisted on
	// the order at intake (planTransport) so this single claim point — which has no
	// envelope — seeds the same atomic claim+manifest-sync a move used to get at
	// intake. nil for retrieve/retrieve_empty (a plain claim).
	if err := s.claimer.ClaimForDispatch(bin.ID, order.ID, order.RemainingUOP); err != nil {
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

	// ★ Node-driven destination reserve (reserve-only) after the source claim,
	// before dispatch: a move to a concrete storage slot reserves it (closing the
	// move race that previously had only the capacity read); a no-op for a line
	// dest. On conflict, keep the just-claimed bin and requeue — next tick the
	// order re-enters as a held-bin dispatch (BinID set) and retries the reserve.
	if err := s.dispatcher.ReserveStorageDropoff(order); err != nil {
		s.setQueueReason(order, "awaiting free storage slot")
		if qerr := s.lifecycle.Queue(order, "fulfillment", "destination slot contended"); qerr != nil {
			s.logFn("fulfillment: order %d → queued after reserve conflict: %v", order.ID, qerr)
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

// dispatchHeldBin dispatches a plain order that already holds its source bin —
// a retrieve/move that reached `sourcing` on a prior tick — using the held bin.
// It never re-finds or
// re-claims (re-finding would claim a second, wrong bin — FindSource excludes
// claimed bins). This is the idempotent reuse that lets the sourcing-reentry
// guard dissolve. On a transient fleet failure it re-queues WITHOUT releasing
// the claim (the same bin re-dispatches next tick). The destination reserve was
// already node-driven by the plain path before this call.
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
		s.setQueueReason(order, "awaiting free storage slot")
		if s.debugLog != nil {
			s.debugLog("fulfillment: held-bin order %d holding — destination slot not secured: %v", order.ID, err)
		}
		return false
	}
	if err := s.lifecycle.MoveToSourcing(order, "fulfillment", "dispatching held bin"); err != nil {
		s.logFn("fulfillment: held-bin order %d → sourcing: %v", order.ID, err)
	}
	vendorOrderID, err := s.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		s.logFn("fulfillment: held-bin order %d fleet dispatch failed, re-queuing (claim kept): %v", order.ID, err)
		if err := s.lifecycle.Queue(order, "fulfillment", "fleet unavailable, re-queued"); err != nil {
			s.logFn("fulfillment: held-bin order %d → queued: %v", order.ID, err)
		}
		return false
	}
	s.logFn("fulfillment: held-bin order %d fulfilled — bin %d (%s -> %s) vendor=%s",
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
