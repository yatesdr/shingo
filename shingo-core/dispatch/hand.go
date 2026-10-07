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
// The lanes' advisory locks (sorted by id), then the status CAS, then slots,
// then bins, then the lane mouths, then occupancy. The advisory locks come first
// so that every transaction that takes a lane's lock and an order's row takes
// them in that order: HandOffLaneToPicker locks the lane and then, in its INSERT,
// the picker's row. A hand that took the row first and the lane second could
// meet a handoff on the same lane and order, and Postgres would abort one side
// (40P01). The locks are transaction-scoped, so they are held until the hand
// commits: the lane is decided together with everything else. Slots before bins
// is the order both old paths used, and keeping it keeps a slot-versus-bin claim
// cycle from forming between two hands.
//
// THE OWNER'S ROW IS THE FIRST ROW LOCK. Advisory locks are not row locks. The
// status CAS is the hand's first statement that takes a row lock (acquireHand),
// and it takes this order's row for update, refusing an order that has ended
// (ConcurrentTransition: the caller yields). A terminalize running at the same
// moment waits for the hand, or the hand for it: the lock-order rule in
// reservations.OwnerLiveSQL's comment, with the hand as one more transaction in
// its list. A terminalize takes no lane lock, so a hand waiting on a lane holds
// nothing it wants. The owner tests the later statements carry (the slot and bin
// claims, the mouth and occupancy writers) read a row this transaction already
// holds.
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
//
// DigLeg marks a dig leg's bin, which has no pending row behind it: a leg is born
// holding nothing and its blocker is the dig's because the parent holds the lane
// (store.CreateCompoundChildren). It is claimed by store.ClaimCompoundLegBinTx,
// the compound's sibling-scoped claim, and its refusal is a blocker wait.
type HandBin struct {
	BinID        int64
	RemainingUOP *int
	DigLeg       bool
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
	_, err := d.acquireHands([]handTake{{order: order, hand: h, entering: entering, actor: actor, reason: reason}})
	return err
}

// handTake is one order's part of a hand transaction: its hand, the nodes its
// first segment enters (their lanes' occupancy), and the `dispatched` write's
// actor and reason.
type handTake struct {
	order    *orders.Order
	hand     Hand
	entering []*nodes.Node
	actor    string
	reason   string
}

// acquireHands takes several orders' hands in ONE transaction, in the order
// given, and returns -1 and nil, or the index of the take that refused and its
// error (acquireHand's returns, for that order). A pair is one job: its legs are
// held together or not at all (C2b A.2; the owner's model, "release both or
// neither"; v4 §2.6). Every lane any take holds is locked first, sorted, before
// any order row, so the lock order is the solo hand's.
//
// TWO ORDER ROWS IN ONE TRANSACTION, and the deadlock rule still holds. Each
// take's CAS locks its own row; its later statements test only that row
// (OwnerLiveSQL). Nothing else takes both legs' rows: a terminalize takes one
// order's row and that order's own rows, and the partner cascade
// (HandleSwapPeerTerminal) runs after the terminalize commits, in a
// transaction of its own.
func (d *Dispatcher) acquireHands(takes []handTake) (int, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("hand: begin for order %d: %w", takes[0].order.ID, err)
	}
	defer tx.Rollback() // no-op once committed

	// THE LANES' ADVISORY LOCKS GO BEFORE ANY ORDER ROW (the header's order).
	// They are the same statements the lane step took; only their place moved,
	// and the lane step below does not take them again.
	var laneIDs []int64
	lockFor := -1
	for i, t := range takes {
		if len(t.hand.Lanes) > 0 {
			laneIDs = append(laneIDs, handLaneIDs(t.hand.Lanes)...)
			if lockFor < 0 {
				lockFor = i
			}
		}
	}
	if len(laneIDs) > 0 {
		if err := reservations.LockLanesTx(tx, laneIDs...); err != nil {
			return lockFor, &HandRefusal{OrderID: takes[lockFor].order.ID, Leg: HandLegLane,
				Code: protocol.QueueWaitingForMaterial, Cause: CauseLaneAcquireError, Err: err}
		}
	}
	finishes := make([]func(), 0, len(takes))
	for i, t := range takes {
		finish, err := d.takeHandTx(tx, t)
		if err != nil {
			return i, err
		}
		finishes = append(finishes, finish)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("hand: commit for order %d: %w", takes[0].order.ID, err)
	}
	for _, finish := range finishes {
		finish()
	}
	for _, t := range takes {
		for _, f := range t.hand.AfterCommit {
			f()
		}
	}
	return -1, nil
}

// takeHandTx is one take on the hand's transaction, after its lanes' advisory
// locks: the status CAS, slots, bins, lane mouths, occupancy. It returns the
// CAS's finish, to run after the commit.
func (d *Dispatcher) takeHandTx(tx *sql.Tx, t handTake) (func(), error) {
	order := t.order
	// THE STATUS CAS IS THE FIRST ROW LOCK: the detector (SYNTH §2.4). Two
	// dispatchers can reach one order: a hand door queues it, and a scanner pass
	// takes it through the held-bin arm before the door's own hand. When the CAS
	// came last, the loser was stopped earlier, at the bin leg, because the
	// winner already held the bin, and a bin refusal reads as "wait for
	// material": the door released the winner's reservation and the scanner
	// parked a dispatched order. First, the loser gets ConcurrentTransition and
	// yields. Same statements, same transaction; only the order moved.
	finish, err := d.lifecycle.transitionTx(tx, order, StatusDispatched, Event{Actor: t.actor, Reason: t.reason})
	if err != nil {
		if IsConcurrentTransition(err) {
			log.Printf("dispatch: order %d moved under us before the fleet order was created — "+
				"NOT dispatching (another caller owns it now): %v", order.ID, err)
			return nil, err
		}
		return nil, &StatusWriteFailed{OrderID: order.ID, Err: err}
	}

	for _, s := range t.hand.Slots {
		if err := store.ConfirmSlotClaimWithPartnerTx(tx, s.Node.ID, order.ID, s.TakenFirst, s.Partner, s.PartnerTaken); err != nil {
			if NotAWait(err) {
				return nil, fmt.Errorf("hand: confirm slot %s for order %d: %w", s.Node.Name, order.ID, err)
			}
			return nil, &HandRefusal{OrderID: order.ID, Leg: HandLegSlot, Code: protocol.QueueWaitingForMaterial,
				Cause: CauseClaimFailed, Err: fmt.Errorf("confirm slot %s for order %d: %w", s.Node.Name, order.ID, err)}
		}
	}
	for _, b := range t.hand.Bins {
		if b.DigLeg {
			var parentID int64
			if order.ParentOrderID != nil {
				parentID = *order.ParentOrderID
			}
			if err := store.ClaimCompoundLegBinTx(tx, b.BinID, order.ID, parentID); err != nil {
				var blocked *store.BlockerClaimedError
				if errors.As(err, &blocked) {
					return nil, &HandRefusal{OrderID: order.ID, Leg: HandLegBin, Code: protocol.QueueStorageRearranging,
						Cause: digBlockerCause(err), Err: err}
				}
				return nil, fmt.Errorf("hand: claim leg bin %d for order %d: %w", b.BinID, order.ID, err)
			}
			continue
		}
		if err := d.binManifest.ConfirmClaimTx(tx, b.BinID, order.ID, b.RemainingUOP); err != nil {
			if NotAWait(err) {
				return nil, fmt.Errorf("hand: confirm bin %d for order %d: %w", b.BinID, order.ID, err)
			}
			return nil, &HandRefusal{OrderID: order.ID, Leg: HandLegBin, Code: protocol.QueueWaitingForMaterial,
				Cause: CauseClaimFailed, Err: fmt.Errorf("confirm bin %d for order %d: %w", b.BinID, order.ID, err)}
		}
	}
	if len(t.hand.Lanes) > 0 {
		conflict, lerr := d.acquireLanesTx(tx, order.ID, t.hand.Lanes)
		if lerr != nil {
			if NotAWait(lerr) {
				return nil, fmt.Errorf("hand: lanes for order %d: %w", order.ID, lerr)
			}
			return nil, &HandRefusal{OrderID: order.ID, Leg: HandLegLane, Code: protocol.QueueWaitingForMaterial,
				Cause: CauseLaneAcquireError, Err: lerr}
		}
		if conflict {
			// Named after the rollback, so the refusal describes the lane as it
			// stands without this order's half-taken rows in it (laneRefusal).
			_ = tx.Rollback()
			cause, lane := d.laneRefusal(order.ID, t.hand.Lanes)
			return nil, &HandRefusal{OrderID: order.ID, Leg: HandLegLane, Code: protocol.QueueStorageRearranging,
				Cause: cause, Lane: lane, Err: reservations.ErrReservationConflict}
		}
	}
	for _, laneID := range d.lanesFor(t.entering...) {
		if _, err := reservations.AcquireOccupancy(tx, order.ID, laneID); err != nil {
			return nil, fmt.Errorf("take occupancy for order %d on lane %d: %w", order.ID, laneID, err)
		}
	}
	return finish, nil
}

// StatusWriteFailed is a hand whose `→ dispatched` write failed for a reason
// that is not a lost CAS (a database error). Nothing was taken and the fleet was
// never asked: it is not a fleet refusal, and a caller that took anything before
// the hand (a dig leg's lane occupancy) releases it — the rule is "release on
// every failure except a lost CAS" (commitToFleet).
type StatusWriteFailed struct {
	OrderID int64
	Err     error
}

func (e *StatusWriteFailed) Error() string {
	return fmt.Sprintf("hand: order %d → dispatched could not be written: %v", e.OrderID, e.Err)
}

func (e *StatusWriteFailed) Unwrap() error { return e.Err }

// NotAWait reports a hand refused because the order has ended or the bin it names
// has gone: the hand returns it as itself, not as a HandRefusal (see the header).
// Like a refusal it held nothing; unlike one, the order is not parked to wait.
func NotAWait(err error) bool {
	return errors.Is(err, reservations.ErrOwnerEnded) || errors.Is(err, reservations.ErrNotTheNamedBin)
}

// handLaneIDs is every lane the hand holds, in any mode: the locks the hand
// takes before its order row. LockLanesTx sorts them and drops duplicates.
func handLaneIDs(holds []laneHold) []int64 {
	ids := make([]int64, 0, len(holds))
	for _, h := range holds {
		ids = append(ids, h.laneID)
	}
	return ids
}

// acquireLanesTx is acquireOrderLanes on the hand's transaction, for lanes whose
// advisory locks the hand already holds (handLaneIDs, taken first). A conflict is
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
		if aErr := reservations.AdmitLockedLanesTx(tx, orderID, mode, asker, staged, laneGateReservedBy, lanes...); aErr != nil {
			if errors.Is(aErr, reservations.ErrReservationConflict) {
				return true, nil
			}
			return false, aErr
		}
	}
	return false, nil
}
