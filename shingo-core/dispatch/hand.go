package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// hand.go — acquiring is one act, in one transaction.
//
// ── WHY ───────────────────────────────────────────────────────────────────
//
// An order that is about to go to the fleet takes everything it will hold hard:
// its destination slot, its bins, its lane mouths, the occupancy of the lanes it
// enters first, and the `dispatched` status. Until this file those were taken in
// separate transactions — the slot, then the bin, then (on the complex path) the
// lanes last — and a refusal in a later step left the earlier ones held. The
// order then waited, holding hard what it had not been able to use, which is
// exactly what a wait must not do (v4 §2.6: a wait keeps SOFT reservations only).
// Two live instances were pinned at the base: a slot kept after its bin's
// confirm failed, and bins kept after the complex lane admit refused.
//
// Now the whole hand commits or none of it does. A refusal anywhere rolls back
// the hard holds taken before it; the order keeps its soft reservations — they
// were taken before the hand, outside its transaction — and parks under the
// failing leg's cause. The fleet call comes after the commit.
//
// ── THE ORDER ─────────────────────────────────────────────────────────────
//
// Slots, then bins, then lane mouths, then occupancy, then the status CAS. Slots
// before bins is the order both old paths used, and keeping it keeps a
// slot-versus-bin claim cycle from forming between two hands. The mouth's
// advisory lock is transaction-scoped, so it is held until the hand commits:
// the lane is decided together with everything else.
//
// THE OWNER'S ROW IS THE FIRST ROW LOCK. The status CAS is the hand's first
// statement (acquireHand), and it takes this order's row for update, refusing an
// order that has ended (ConcurrentTransition: the caller yields). A terminalize
// running at the same moment waits for the hand, or the hand for it: the
// lock-order rule in reservations.OwnerLiveSQL's comment, with the hand as one
// more transaction in its list. The owner tests the later statements carry (the
// slot and bin claims, the mouth and occupancy writers) read a row this
// transaction already holds. One departure from that list's rule, recorded
// there: a hand with lanes takes their advisory locks after the order row.
//
// AN ENDED ORDER OR A GONE NAMED BIN IS NOT A WAIT. A leg refused with
// reservations.ErrOwnerEnded or ErrNotTheNamedBin comes back as that error, not
// as a HandRefusal: nothing is held, and the caller stops (the order ended) or
// skips the move (its bin has left), rather than parking an order that will
// never be served.
//
// ── WHAT IS NOT HERE ──────────────────────────────────────────────────────
//
// The decisions. Which slot, which bins, which lanes, and whether the lanes
// admit the order at all (the read-only admission verdict, with the dig it may
// propose) are all settled before a hand is built. A hand only makes them hard.

// Hand is what one order takes hard at dispatch.
type Hand struct {
	Slots []HandSlot
	Bins  []HandBin
	Lanes []laneHold
	// AfterCommit runs once the hand has committed — audit lines that must not
	// claim a hold the rollback took back.
	AfterCommit []func()
}

// HandSlot is one storage slot to hard-claim. TakenFirst is the bins at the
// node this order's own earlier pickups take before it drops there (see
// store.ConfirmSlotClaim); nil for every plain dropoff. Partner and PartnerTaken
// are a drop the vacated-slot rule admitted on the partner's lift (the step's
// partner stamp): the claim credits that bin only while the partner still holds
// it (store.ConfirmSlotClaimWithPartner). Partner 0 is a plain slot.
type HandSlot struct {
	Node         *nodes.Node
	TakenFirst   []int64
	Partner      int64
	PartnerTaken []int64
}

// HandBin is one soft-held bin to hard-claim. RemainingUOP is the operator's
// release-correction count, as ConfirmClaim takes it (nil = a plain claim).
type HandBin struct {
	BinID        int64
	RemainingUOP *int
}

// HandLeg names the part of a hand that refused.
type HandLeg string

const (
	HandLegSlot HandLeg = "slot"
	HandLegBin  HandLeg = "bin"
	HandLegLane HandLeg = "lane"
)

// HandRefusal is a hand that could not be taken. Nothing of it is held: the
// transaction rolled back. It is a WAIT, never a failure — the caller parks the
// order under Code/Cause and the next pass asks again.
type HandRefusal struct {
	OrderID int64
	Leg     HandLeg
	Code    protocol.QueueCode
	Cause   QueueCause
	Lane    string // the contended lane's name, for a lane refusal
	Err     error
}

func (r *HandRefusal) Error() string {
	return fmt.Sprintf("order %d: hand refused at the %s leg (%s): %v", r.OrderID, r.Leg, r.Cause, r.Err)
}

func (r *HandRefusal) Unwrap() error { return r.Err }

// AsHandRefusal reports whether err is a hand refusal, and returns it.
func AsHandRefusal(err error) (*HandRefusal, bool) {
	var r *HandRefusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// ParkParams is the QueueParams a refusal parks under, filled from the order.
func (r *HandRefusal) ParkParams(order *orders.Order) QueueParams {
	if r.Leg == HandLegLane && r.Cause != CauseLaneAcquireError {
		return QueueParams{Lane: r.Lane, Payload: order.PayloadCode}
	}
	if r.Code == protocol.QueueWaitingForSlot {
		return QueueParams{Destination: order.DeliveryNode}
	}
	return QueueParams{Payload: order.PayloadCode}
}

// acquireHand takes the hand, the occupancy of every lane `entering` lies in,
// and the `dispatched` status, in one transaction.
//
// Returns:
//   - nil: everything is held and the order is `dispatched` (in memory too);
//   - *HandRefusal: a leg refused; nothing is held;
//   - reservations.ErrOwnerEnded or ErrNotTheNamedBin (wrapped): the order ended,
//     or the one bin it names has left its source; nothing is held; not a wait;
//   - ConcurrentTransition: the order moved under us; nothing is held; yield;
//   - any other error: a database fault; nothing is held.
func (d *Dispatcher) acquireHand(order *orders.Order, h Hand, entering []*nodes.Node, actor, reason string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("hand: begin for order %d: %w", order.ID, err)
	}
	defer tx.Rollback() // no-op once committed

	// THE STATUS CAS GOES FIRST, so it is the detector (SYNTH §2.4). Two
	// dispatchers can reach one order: a hand door queues it, and a scanner pass
	// takes it through the held-bin arm before the door's own hand. When the CAS
	// came last, the loser was stopped earlier, at the bin leg, because the
	// winner already held the bin, and a bin refusal reads as "wait for
	// material": the door released the winner's reservation and the scanner
	// parked a dispatched order. First, the loser gets ConcurrentTransition and
	// yields. Same statements, same transaction; only the order moved.
	finish, err := d.lifecycle.transitionTx(tx, order, StatusDispatched, Event{Actor: actor, Reason: reason})
	if err != nil {
		if IsConcurrentTransition(err) {
			log.Printf("dispatch: order %d moved under us before the fleet order was created — "+
				"NOT dispatching (another caller owns it now): %v", order.ID, err)
		}
		return err
	}

	for _, s := range h.Slots {
		if err := store.ConfirmSlotClaimWithPartnerTx(tx, s.Node.ID, order.ID, s.TakenFirst, s.Partner, s.PartnerTaken); err != nil {
			if NotAWait(err) {
				return fmt.Errorf("hand: confirm slot %s for order %d: %w", s.Node.Name, order.ID, err)
			}
			return &HandRefusal{OrderID: order.ID, Leg: HandLegSlot, Code: protocol.QueueWaitingForMaterial,
				Cause: CauseClaimFailed, Err: fmt.Errorf("confirm slot %s for order %d: %w", s.Node.Name, order.ID, err)}
		}
	}
	for _, b := range h.Bins {
		if err := d.binManifest.ConfirmClaimTx(tx, b.BinID, order.ID, b.RemainingUOP); err != nil {
			if NotAWait(err) {
				return fmt.Errorf("hand: confirm bin %d for order %d: %w", b.BinID, order.ID, err)
			}
			return &HandRefusal{OrderID: order.ID, Leg: HandLegBin, Code: protocol.QueueWaitingForMaterial,
				Cause: CauseClaimFailed, Err: fmt.Errorf("confirm bin %d for order %d: %w", b.BinID, order.ID, err)}
		}
	}
	if len(h.Lanes) > 0 {
		conflict, lerr := d.acquireLanesTx(tx, order.ID, h.Lanes)
		if lerr != nil {
			if NotAWait(lerr) {
				return fmt.Errorf("hand: lanes for order %d: %w", order.ID, lerr)
			}
			return &HandRefusal{OrderID: order.ID, Leg: HandLegLane, Code: protocol.QueueWaitingForMaterial,
				Cause: CauseLaneAcquireError, Err: lerr}
		}
		if conflict {
			// Named after the rollback, so the refusal describes the lane as it
			// stands without this order's half-taken rows in it (laneRefusal).
			_ = tx.Rollback()
			cause, lane := d.laneRefusal(order.ID, h.Lanes)
			return &HandRefusal{OrderID: order.ID, Leg: HandLegLane, Code: protocol.QueueStorageRearranging,
				Cause: cause, Lane: lane, Err: reservations.ErrReservationConflict}
		}
	}
	for _, laneID := range d.lanesFor(entering...) {
		if _, err := reservations.AcquireOccupancy(tx, order.ID, laneID); err != nil {
			return fmt.Errorf("take occupancy for order %d on lane %d: %w", order.ID, laneID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("hand: commit for order %d: %w", order.ID, err)
	}
	finish()
	for _, f := range h.AfterCommit {
		f()
	}
	return nil
}

// NotAWait reports a hand refused because the order has ended or the bin it names
// has gone: the hand returns it as itself, not as a HandRefusal (see the header).
// Like a refusal it held nothing; unlike one, the order is not parked to wait.
func NotAWait(err error) bool {
	return errors.Is(err, reservations.ErrOwnerEnded) || errors.Is(err, reservations.ErrNotTheNamedBin)
}

// acquireLanesTx is acquireOrderLanes on the hand's transaction. A conflict is
// reported, not released: the caller's rollback releases every lane this call
// took, which is what acquireOrderLanes had to do by hand.
func (d *Dispatcher) acquireLanesTx(tx *sql.Tx, orderID int64, holds []laneHold) (conflict bool, err error) {
	byMode := map[reservations.Mode][]int64{}
	var modes []reservations.Mode
	for _, h := range holds {
		if _, ok := byMode[h.mode]; !ok {
			modes = append(modes, h.mode)
		}
		byMode[h.mode] = append(byMode[h.mode], h.laneID)
	}
	owner, _ := d.laneOwnerFor(orderID)
	asker := reservations.AskerFor(orderID, owner)
	for _, mode := range modes {
		lanes := byMode[mode]
		var staged reservations.StagedOutsideByLane
		if mode == reservations.ModeDig {
			staged = stagedAtMarkByLane(d.db, lanes...)
		}
		if aErr := reservations.AcquireLanesForTx(tx, orderID, mode, asker, staged, laneGateReservedBy, lanes...); aErr != nil {
			if errors.Is(aErr, reservations.ErrReservationConflict) {
				return true, nil
			}
			return false, aErr
		}
	}
	return false, nil
}
