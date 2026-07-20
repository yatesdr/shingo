package orders

import (
	"fmt"
	"log"
	"sync"
	"time"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/orders"
)

type LifecycleService struct {
	db      *store.DB
	emitter EventEmitter
	debug   DebugLogFunc

	// deliveredSeeds carries the bin's uop+epoch snapshot from the
	// OrderDelivered envelope into the (generic) applyTransition emit,
	// keyed by order id. HandleDelivered Stores it immediately before
	// driving the StatusDelivered transition (same goroutine) and
	// applyTransition LoadAndDeletes it in the delivered branch. Other
	// paths to StatusDelivered (force/recovery) find no entry, so Edge
	// falls back to its role-default seed — the pre-existing behavior.
	// A sync.Map keeps it zero-value-ready (no constructor change) and
	// safe if deliveries for different orders ever interleave.
	deliveredSeeds sync.Map
}

// deliveredSeed is the bin snapshot Core stamps on the OrderDelivered
// envelope; nil uop means "not provided" (older Core) → role default.
type deliveredSeed struct {
	uop   *int
	epoch int64
}

func newLifecycleService(db *store.DB, emitter EventEmitter, debug DebugLogFunc) *LifecycleService {
	return &LifecycleService{db: db, emitter: emitter, debug: debug}
}

func (s *LifecycleService) Transition(orderID int64, newStatus protocol.Status, detail string) error {
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order: %w", err)
	}
	if !IsValidTransition(order.Status, newStatus) {
		if order.Status == newStatus || IsTerminal(order.Status) {
			return nil // idempotent: already in target state or terminal
		}
		return fmt.Errorf("invalid transition from %s to %s", order.Status, newStatus)
	}
	return s.applyTransition(order, newStatus, detail, false)
}

func (s *LifecycleService) ForceTransition(orderID int64, newStatus protocol.Status, detail string) error {
	order, err := s.db.GetOrder(orderID)
	if err != nil {
		return err
	}
	if order.Status == newStatus {
		return nil
	}
	return s.applyTransition(order, newStatus, detail, true)
}

// ApplyCoreStatus maps a status Core pushes onto the Edge order row. It is the
// one function shared by the live-push path (HandleOrderUpdate →
// HandleCoreStatusPush) and the boot-reconcile path (ApplyCoreStatusSnapshot),
// so live and boot can never diverge. It replaces the older partial mapping
// that branched on `queued` alone and discarded every other status.
//
// The operator is shown the truth of whichever machine owns the order at that
// moment: pre-fleet statuses (queued/sourcing) reflect Core's planning, fleet
// statuses (dispatched/in_transit/faulted) reflect the fleet. Mapping them all
// here means Edge no longer discards sourcing/dispatched/faulted pushes while a
// stale prior status (usually acknowledged) keeps rendering as "IN TRANSIT".
//
// Arms:
//   - dispatched/in_transit/faulted (FLEET statuses) → FORCE-adopt, the way the
//     boot-reconcile path does. The fleet (via Core) is authoritative on these;
//     the edge adopts, it does not gate. Validating them here is what permanently
//     desynced the mirror: when Core coalesces a fast dispatched→in_transit burst
//     and drops the intermediate `dispatched` push, sourcing→in_transit is not a
//     legal edge, so a validated transition is rejected and silently swallowed —
//     and every later push (staged) is rejected in turn, freezing the order on a
//     stale status while the robot is already staged (SPR order 2399, 2026-07). A
//     stale/out-of-order fleet push onto an already-TERMINAL edge row is ignored
//     rather than forced, so it can never resurrect a finished order.
//   - queued/sourcing (pre-fleet PLANNING statuses) → validated Transition. The
//     shared table already permits the legal re-plan edges (e.g. Dispatched→
//     Sourcing for redirect); an arbitrary backward force is not warranted, since
//     Core owns the fleet truth and the planner's queued/sourcing is advisory.
//   - staged/delivered/confirmed/failed/cancelled/skipped → NO-OP here. Those
//     statuses are owned by DEDICATED envelopes (order.staged, order.delivered,
//     order.error, order.skipped, order.cancelled) that carry extra fields
//     (bin snapshots, expiry, reason) this generic mapping does not have. Forcing
//     the fleet arm above still unblocks them: once the edge force-adopts
//     in_transit, the dedicated order.staged envelope's in_transit→staged
//     transition is legal again.
//
// Error handling: a planning-arm push that isn't a legal transition returns an
// error; both callers log-and-swallow it, a graceful no-op. The fleet arm no
// longer rejects — a dropped intermediate can no longer freeze the mirror.
func (s *LifecycleService) ApplyCoreStatus(order *orders.Order, coreStatus protocol.Status, detail string) error {
	switch coreStatus {
	case StatusDispatched, StatusInTransit, StatusFaulted:
		if IsTerminal(order.Status) {
			// A late/stale fleet push after the edge already reached a terminal
			// state — never resurrect it; Core's terminal envelopes own that edge.
			return nil
		}
		return s.ForceTransition(order.ID, coreStatus, detail)
	case StatusQueued, StatusSourcing:
		return s.Transition(order.ID, coreStatus, detail)
	default:
		// staged/delivered/terminal are owned by dedicated envelopes; unknown
		// statuses are ignored. No status write from this mapping.
		return nil
	}
}

func (s *LifecycleService) applyTransition(order *orders.Order, newStatus protocol.Status, detail string, forced bool) error {
	oldStatus := order.Status
	if forced {
		s.debug.Log("force transition: id=%d uuid=%s %s->%s", order.ID, order.UUID, oldStatus, newStatus)
	} else {
		s.debug.Log("transition: id=%d uuid=%s %s->%s", order.ID, order.UUID, oldStatus, newStatus)
	}
	if err := s.db.UpdateOrderStatus(order.ID, string(newStatus)); err != nil {
		return fmt.Errorf("update order status: %w", err)
	}
	if err := s.db.InsertOrderHistory(order.ID, string(oldStatus), string(newStatus), detail); err != nil {
		log.Printf("insert order history: %v", err)
	}
	updated, _ := s.db.GetOrder(order.ID)
	eta := ""
	if updated != nil && updated.ETA != nil {
		eta = *updated.ETA
	}
	s.emitter.EmitOrderStatusChanged(order.ID, order.UUID, order.OrderType, string(oldStatus), string(newStatus), eta, nil, order.ProcessNodeID)
	// Delivered fires *before* the terminal-status branch so the runtime
	// UOP handler binds the cache to the actually-arrived bin the moment
	// the bin lands, independent of when (or whether) the operator
	// confirms. Order.BinID is read from the post-transition row because
	// HandleDelivered may have just stamped Core's resolved bin id.
	if newStatus == StatusDelivered {
		var binID *int64
		if updated != nil {
			binID = updated.BinID
		}
		// Bin snapshot stashed by HandleDelivered (same goroutine);
		// absent for force/recovery deliveries → Edge role-default seed.
		var binUOP *int
		var binEpoch int64
		if v, ok := s.deliveredSeeds.LoadAndDelete(order.ID); ok {
			seed := v.(deliveredSeed)
			binUOP, binEpoch = seed.uop, seed.epoch
		}
		s.emitter.EmitOrderDelivered(order.ID, order.UUID, order.OrderType, order.ProcessNodeID, binID, binUOP, binEpoch)
	}
	if IsTerminal(newStatus) {
		s.emitter.EmitOrderCompleted(order.ID, order.UUID, order.OrderType, nil, order.ProcessNodeID)
		if newStatus == StatusFailed {
			s.emitter.EmitOrderFailed(order.ID, order.UUID, order.OrderType, detail)
		}
	}
	return nil
}

func (s *LifecycleService) HandleDelivered(order *orders.Order, statusDetail string, stagedExpireAt *time.Time, binID *int64, uop *int, epoch int64) error {
	if stagedExpireAt != nil {
		if err := s.db.UpdateOrderStagedExpireAt(order.ID, stagedExpireAt); err != nil {
			log.Printf("lifecycle: update staged_expire_at for order=%d: %v", order.ID, err)
		}
	}
	// Capture Core's bin id at delivery so the PLC tick path can
	// attribute deltas to the right bin. Nil for multi-bin orders /
	// older Core builds. uop+epoch are Core's snapshot of that bin at
	// delivery (from the OrderDelivered envelope) — stashed here for the
	// delivered emit in applyTransition so Edge seeds its runtime cache
	// and active_bin_epoch without an HTTP pull.
	if binID != nil {
		if err := s.db.UpdateOrderBinID(order.ID, binID); err != nil {
			log.Printf("update order bin_id: %v", err)
		}
	}
	s.deliveredSeeds.Store(order.ID, deliveredSeed{uop: uop, epoch: epoch})
	defer s.deliveredSeeds.Delete(order.ID)
	return s.Transition(order.ID, StatusDelivered, statusDetail)
}

func (s *LifecycleService) ApplyCoreStatusSnapshot(snapshot protocol.OrderStatusSnapshot) error {
	order, err := s.db.GetOrderByUUID(snapshot.OrderUUID)
	if err != nil {
		return err
	}
	snapStatus := protocol.Status(snapshot.Status)
	if !snapshot.Found || snapStatus == "" || snapStatus == order.Status {
		return nil
	}

	// Persist the queue_reason + queue_code the snapshot carries so an Edge
	// resync doesn't lose them — the live-push path writes both, and the boot
	// reconcile must mirror it (the order's status is about to change, and the
	// blocking signal Core reports should land with it). Best-effort: a failed
	// write is logged and swallowed, matching the live-push path's disposition.
	if snapshot.QueueReason != "" || order.QueueReason != "" {
		if qerr := s.db.SetOrderQueueReason(order.UUID, snapshot.QueueReason, snapshot.QueueCode); qerr != nil {
			log.Printf("lifecycle: snapshot set queue_reason for %s: %v", order.UUID, qerr)
		}
	}

	detail := "startup reconciliation with core"
	switch snapStatus {
	case StatusConfirmed:
		if order.Status == StatusDelivered {
			return s.Transition(order.ID, StatusConfirmed, detail)
		}
		return s.ForceTransition(order.ID, StatusConfirmed, detail)
	case StatusCancelled:
		return s.ForceTransition(order.ID, StatusCancelled, detail)
	case StatusFailed:
		return s.ForceTransition(order.ID, StatusFailed, detail)
	case StatusSkipped:
		return s.ForceTransition(order.ID, StatusSkipped, detail)
	case StatusDelivered:
		return s.ForceTransition(order.ID, StatusDelivered, detail)
	case StatusStaged:
		return s.ForceTransition(order.ID, StatusStaged, detail)
	case StatusInTransit:
		return s.ForceTransition(order.ID, StatusInTransit, detail)
	case StatusAcknowledged:
		return s.ForceTransition(order.ID, StatusAcknowledged, detail)
	case StatusQueued:
		return s.ForceTransition(order.ID, StatusQueued, detail)
	case StatusSourcing, StatusDispatched, StatusFaulted:
		// The live-push mapping (ApplyCoreStatus) stores these, so a boot
		// snapshot must reconcile them too — an edge restart while Core reports
		// sourcing/faulted/dispatched would otherwise leave the mirror stuck on
		// a stale status. Force, matching the other non-terminal snapshot arms
		// (Core is authoritative at boot).
		return s.ForceTransition(order.ID, snapStatus, detail)
	case StatusReshuffling:
		// Complex-order reshuffles can sit in Reshuffling for minutes
		// while the compound runs. Without this arm an edge restart in
		// that window would leave the edge mirror stuck on a stale
		// pre-reshuffle status. Same fix lets simple-retrieve
		// reshuffles surface correctly after edge reconnect.
		return s.ForceTransition(order.ID, StatusReshuffling, detail)
	default:
		return nil
	}
}
