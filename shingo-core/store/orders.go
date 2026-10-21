package store

// Stage 2D delegate file: order CRUD and history live in store/orders/.
// Cross-aggregate methods (CreateCompoundChildren, FailOrderAtomic,
// CancelOrderAtomic) stay here because they mutate both the orders and
// bins tables in a single transaction.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// CreateOrder is the ordinary door onto orders.Create — every creator except the
// compound one, which has a transaction of its own (CreateCompoundChildren).
//
// IT OPENS A TRANSACTION BECAUSE Create WRITES TWO ROWS: the order and its birth
// history row. Handed a *sql.DB those are separate autocommits, so a failure
// between them leaves either an order with no birth certificate — nowhere for
// SetQueueDetail to stamp a cause — or a committed order the caller was told did
// not get created. Both rows, or neither.
func (db *DB) CreateOrder(o *orders.Order) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	if err := orders.Create(tx, o); err != nil {
		return err
	}
	return tx.Commit()
}

// CompoundChild describes a child order to create in a compound order
// transaction. Declared here (not in the orders sub-package) because
// CreateCompoundChildren is cross-aggregate.
type CompoundChild struct {
	Order *orders.Order
	BinID int64 // bin to claim for this child
	// Park is the parking slot this leg pencils at birth (a pending slot
	// reservation in its name), 0 for none. ParkByRank: the dig's walk offered
	// it by rank, so a stranger's pending promise on it is taken
	// (takeSlotByRankTx) under ParkNamed's demand, the holder told ParkNote.
	Park       int64
	ParkByRank bool
	ParkNamed  int64
	ParkNote   HolderNote
}

// ErrParkingTaken is a dig birth refused because the parking slot the walk found
// for one of its blockers was taken between the walk and the birth: another
// order's reservation is on it, and the dig cannot take it by rank. Nothing was
// written. Dispatch reads it as a short walk (no-shuffle-slot).
var ErrParkingTaken = errors.New("a dig's parking slot was taken before its birth")

// ParkingTakenError names the slot. It unwraps to ErrParkingTaken.
type ParkingTakenError struct {
	SlotID int64
	LegSeq int
}

func (e *ParkingTakenError) Error() string {
	return fmt.Sprintf("%s: slot %d for the leg at sequence %d", ErrParkingTaken, e.SlotID, e.LegSeq)
}

func (e *ParkingTakenError) Unwrap() error { return ErrParkingTaken }

// MarkCompoundLegsWaiting writes the turn wait on every pending leg of parentID
// after sequence first: one UPDATE (dispatch markLegsWaitingTheirTurn).
func (db *DB) MarkCompoundLegsWaiting(parentID int64, first int, reason, code, cause string) error {
	_, err := db.Exec(`UPDATE orders SET queue_reason=$3, queue_code=$4, queue_cause=$5, updated_at=$6
		WHERE parent_order_id=$1 AND sequence>$2 AND status=$7`,
		parentID, first, reason, code, cause, clock.Now().UTC(), string(protocol.StatusPending))
	if err != nil {
		return fmt.Errorf("mark dig %d's later legs waiting: %w", parentID, err)
	}
	return nil
}

// pencilParkingTx is a leg's parking pencil at its dig's birth: a pending slot
// reservation in the leg's name on c.Park, taken from a stranger's pending
// promise by rank when the walk offered it so. A slot already on someone's paper
// that the dig cannot take refuses the birth (*ParkingTakenError).
func pencilParkingTx(tx *sql.Tx, legID int64, c CompoundChild) error {
	err := reservations.AcquireSlot(tx, legID, c.Park, reservations.ByDigParking)
	if err == nil {
		return nil
	}
	if !errors.Is(err, reservations.ErrReservationConflict) {
		return fmt.Errorf("pencil parking slot %d for leg %d: %w", c.Park, legID, err)
	}
	if c.ParkByRank {
		_, tErr := takeSlotByRankTx(tx, c.Park, legID, c.ParkNamed, c.ParkNote)
		if tErr == nil {
			return nil
		}
		if !errors.Is(tErr, ErrNoSlotTake) {
			return tErr
		}
	}
	return &ParkingTakenError{SlotID: c.Park, LegSeq: c.Order.Sequence}
}

// CompoundLanes is a dig's lane step in the transaction that writes its legs:
// LaneIDs are locked (reservations.LockLanesTx) before the parent's row, and
// Admit writes the mouth rows for those lanes after it, taking no lock of its own
// (reservations.AdmitLockedLanesTx).
type CompoundLanes struct {
	LaneIDs []int64
	Admit   func(*sql.Tx) error
}

// ErrBlockerClaimed is the sentinel behind BlockerClaimedError, for callers that
// only need to know WHICH refusal this was.
var ErrBlockerClaimed = errors.New("bin held by an order outside the compound")

// BlockerClaimedError is the claim CAS refusing a leg because the bin it needs
// is held by an order OUTSIDE this compound.
//
// It is typed because its disposition differs from every other error
// CreateCompoundChildren can return. The others are faults — a failed write, a
// broken row. This one is CONGESTION: the holder is an ordinary live order that
// will finish and drop the claim, and the commonest holder by a wide margin is a
// dispatched retrieve whose robot is at that moment driving the blocker out of
// the lane. The blocker is in the process of ceasing to be a blocker.
//
// Terminally failing the digger for it violates wait-not-fail on traffic as
// ordinary as two demands on one lane minutes apart, so the dispatch callers map
// this to a transient planning error and park — see
// dispatch/planning_service.go planBuriedReshuffle.
//
// HolderID is 0 when the holding order could not be read back; the refusal
// stands either way, only the operator-facing detail is poorer.
type BlockerClaimedError struct {
	BinID    int64 // the bin the leg wanted
	ChildID  int64 // the leg that wanted it
	ParentID int64 // the compound the leg belongs to
	HolderID int64 // the order actually holding the claim, or 0 if unreadable
	// Promised distinguishes the RANKED refusal from the claimed one, and the
	// distinction is a releaser rather than a nuance. False: the holder has a hard
	// claim, so a robot is committed and the wait ends when it finishes its drive.
	// True: the holder has a PROMISE — a pending reservation, no robot — and it
	// outranked this dig, so the wait ends when that demand takes its bin or ends.
	// The dispatch caller reads this to pick between two queue causes; telling an
	// operator to wait for a drive that has not started is the wrong-name defect
	// class the cause vocabulary exists to prevent.
	Promised bool
}

func (e *BlockerClaimedError) Error() string {
	if e.Promised {
		return fmt.Sprintf("claim bin %d for child %d: promised to order %d, whose demand outranks "+
			"compound %d", e.BinID, e.ChildID, e.HolderID, e.ParentID)
	}
	if e.HolderID != 0 {
		return fmt.Sprintf("claim bin %d for child %d: held by order %d, outside compound %d",
			e.BinID, e.ChildID, e.HolderID, e.ParentID)
	}
	return fmt.Sprintf("claim bin %d for child %d: held by an order outside compound %d",
		e.BinID, e.ChildID, e.ParentID)
}

func (e *BlockerClaimedError) Unwrap() error { return ErrBlockerClaimed }

// ErrParentGone is CreateCompoundChildren refusing a compound whose parent is
// already terminal.
var ErrParentGone = errors.New("compound parent is terminal")

// ParentGoneError is the typed form of ErrParentGone. Its disposition is unlike
// every other error out of CreateCompoundChildren: nothing failed and nothing
// waits. The order the dig was for has ended (a cancel landed between the
// planner's read of it and this write), so the caller stops: it fails nothing,
// parks nothing, and releases the lane it took in the parent's name.
type ParentGoneError struct {
	ParentID int64
	Status   protocol.Status
}

func (e *ParentGoneError) Error() string {
	return fmt.Sprintf("compound parent %d is %s: no dig is written under it", e.ParentID, e.Status)
}

func (e *ParentGoneError) Unwrap() error { return ErrParentGone }

// DisplacedByHand names an order the steal took a bin from and DELIBERATELY did
// not repair: one whose caller is a person at a Core door.
//
// The ordinary victim is un-pointed and re-sources, which is a recalculation. A
// hand-placed one cannot be treated that way — un-pointing it hands it back to
// the finder, and for a node-local move that means Core silently substituting
// whatever bin is standing at that node now for the one somebody named. So the
// store leaves it pointed and REPORTS it, and the dispatch caller ends it out
// loud once the steal has committed. Ending it here is not available: the
// terminal chokepoint opens a transaction of its own.
type DisplacedByHand struct {
	OrderID  int64  // the hand-placed order that lost its bin
	BinID    int64  // the bin the dig took
	DigID    int64  // the compound whose dig took it
	ParkedAt string // where that dig is putting the bin; "" if the leg named nowhere
}

// CreateCompoundChildren creates all child orders in a single transaction and
// takes the foreign soft holds off their bins. Cross-aggregate (orders ↔ bins).
//
// A LEG IS BORN HOLDING NOTHING (v4 §2.6). It is `pending` until its turn, and a
// wait keeps soft holds only, so the leg's bin claim and its ledger row are taken
// at its handover, in its own hand (ClaimCompoundLegBinTx). What keeps the
// blockers the dig's in between is the parent's dig hold on the lane: the finders
// and the bin acquire ask the dig-exclusion question (reservations.DigExclusionSQL)
// and keep everyone but the compound out of a lane a dig holds.
//
// lanes, when not nil, is the parent's lane step: the dig's mouth hold, taken in
// this transaction so the dig is started whole or not at all. Its errors return
// as they are (reservations.ErrReservationConflict when another holder owns the
// lane).
//
// Returns the hand-placed orders whose bins it took — see DisplacedByHand. Empty
// on the ordinary path and on every error.
func (db *DB) CreateCompoundChildren(children []CompoundChild, lanes *CompoundLanes) ([]DisplacedByHand, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	displaced, stolen, err := CreateCompoundChildrenTx(tx, children, lanes)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// AFTER THE COMMIT, and only after it. A displaced order is a consequence of
	// a steal that HAPPENED; reporting one out of a transaction that then rolls
	// back would have the caller terminate a person's order over a dig that
	// never took anything. The LOG is the same fact and now keeps the same rule:
	// a steal reported from inside the transaction is a theft that a later
	// child's refusal can un-do, and soakstat counts those lines.
	LogSteals(stolen)
	return displaced, nil
}

// Steals is what a birth took from foreign soft holders, for LogSteals once
// the birth has committed.
type Steals []stolenBin

// LogSteals logs a committed birth's steals (see CreateCompoundChildren).
func LogSteals(s Steals) { logSteals(s) }

// CreateCompoundChildrenTx is CreateCompoundChildren on the caller's
// transaction, for a door that commits the children with other work — the home
// clear commits its parent, the partial's soft hold and the empty's zero with
// them. The caller commits, then reports displaced and calls LogSteals, the
// order CreateCompoundChildren keeps.
func CreateCompoundChildrenTx(tx *sql.Tx, children []CompoundChild, lanes *CompoundLanes) ([]DisplacedByHand, Steals, error) {
	// THE LANES' ADVISORY LOCKS COME FIRST, then the parent's row: the order every
	// transaction that takes a lane and an order row keeps (the hand,
	// HandOffLaneToPicker; reservations.OwnerLiveSQL's comment). The parent's
	// FOR UPDATE below stays the first ROW lock.
	if lanes != nil {
		if err := reservations.LockLanesTx(tx, lanes.LaneIDs...); err != nil {
			return nil, nil, err
		}
	}
	// THE PARENT IS READ UNDER LOCK, FIRST. A cancel's status write takes this
	// row, so the two serialise: either this transaction commits before the
	// cancel and the cancel's cascade lists these children and cancels them, or
	// the cancel commits first and nothing is written here. Without the lock the
	// cascade could list the children before this write and miss every one of
	// them: legs dispatched, bins claimed and a delivery made for an order that
	// had ended. "Live" is non-terminal, not "reshuffling": a gate-staged parent
	// stays `staged` and takes its dig as it stands (dispatch CreateCompoundOrder).
	if parentID := compoundParentID(children); parentID != 0 {
		var status protocol.Status
		if err := tx.QueryRow(`SELECT status FROM orders WHERE id=$1 FOR UPDATE`, parentID).Scan(&status); err != nil {
			return nil, nil, fmt.Errorf("read compound parent %d: %w", parentID, err)
		}
		if protocol.IsTerminal(status) {
			return nil, nil, &ParentGoneError{ParentID: parentID, Status: status}
		}
	}
	if lanes != nil {
		if err := lanes.Admit(tx); err != nil {
			return nil, nil, err
		}
	}
	var displaced []DisplacedByHand
	var stolen Steals

	for _, c := range children {
		o := c.Order
		// One writer for the orders table. orders.Create runs inside this
		// transaction because it takes a QueryRower rather than a *sql.DB, so
		// children carry the same columns as every other order and cannot drift
		// from them. It also owns the clock.Now() timestamps and the o.ID
		// write-back that the code below depends on.
		if err := orders.Create(tx, o); err != nil {
			return nil, nil, fmt.Errorf("create child order (seq %d): %w", o.Sequence, err)
		}

		// THE STEAL HAPPENS AT BIRTH; THE CLAIM AT THE HANDOVER. A blocker is
		// POSITIONAL — the dig has no choice about which bins are in its way — so
		// the foreign soft holds on it are settled now, while the dig can still
		// back out whole: stealSoftHolds ranks the two demands and REFUSES when
		// the holder wins (§7), which aborts this transaction and leaves the lane
		// free. A refusal at a later handover could not back out: the parent
		// would hold the lane and the holder would be refused by it.
		//
		// The holder's row and its bin_id go together, in the steal, so it
		// re-enters through the finder and re-resolves rather than confirming a
		// bin that is leaving. The leg's own claim and ledger row are written at
		// its handover (ClaimCompoundLegBinTx).
		if o.BinID != nil {
			parentID := int64(0)
			if o.ParentOrderID != nil {
				parentID = *o.ParentOrderID
			}
			byHand, took, err := stealSoftHolds(tx, *o.BinID, o.ID, parentID, o.DeliveryNode)
			if err != nil {
				return nil, nil, err
			}
			displaced = append(displaced, byHand...)
			stolen = append(stolen, took...)
		}

		// THE LEG PENCILS ITS PARKING, in the dig's transaction (C2b dig-parking
		// ruling): one pending slot reservation per blocker, so the dig's room is
		// held from its birth. uq_reservations_slot_active is what keeps two digs
		// from booking one slot; the loser's birth rolls back whole.
		if c.Park != 0 {
			if err := pencilParkingTx(tx, o.ID, c); err != nil {
				return nil, nil, err
			}
		}
	}

	return displaced, stolen, nil
}

// compoundParentID is the parent the children are written under, 0 if none
// names one.
func compoundParentID(children []CompoundChild) int64 {
	for _, c := range children {
		if c.Order != nil && c.Order.ParentOrderID != nil {
			return *c.Order.ParentOrderID
		}
	}
	return 0
}

// victim is one foreign soft holder of a dig's blocker.
type victim struct {
	orderID    int64
	handPlaced bool
	pointsHere bool
}

// stealSoftHolds releases the FOREIGN soft holds on a bin a dig is about to
// claim, and clears each holder's pointer to it, so the holders recalculate
// instead of following a plan that is no longer true.
//
// Foreign means: not this compound's. A row belonging to the parent or to a
// sibling is the same demand holding its own bin across steps, and the leg's
// handover moves it (supersedeBinLedger) rather than reporting a theft.
//
// ── SET-VALUED, AND IT WAS A COIN FLIP ────────────────────────────
//
// This read one holder, with an unordered QueryRow, while the ledger write that
// followed it deleted the bin's WHOLE book. The two halves
// agreed only because uq_reservations_bin_active makes one active row per bin
// structural. The day that index narrows, the singular read leaves every holder
// but one with a bin_id pointing at a bin the dig owns and no reservation behind
// it — and dispatchHeldBin never re-acquires, so each of them confirms by id
// against a row that does not exist, every tick, with no releaser. The pointer
// and the book are swept by the same rule now, and the rule is "every holder".
//
// ── PLANNING, NOT PHYSICS ────────────────────────────────────
//
// Only PENDING rows are victims. A confirmed row is a hold that has been acted
// on — the claim and the confirm move together in one transaction — so a robot
// is on its way and the hold is physics, not a plan anybody may rewrite. The
// leg's claim at its handover refuses such a bin (ClaimCompoundLegBinTx); it is
// a different guard for a different reason, and a set-valued sweep must not rely
// on being saved by one.
//
// ── AND A PERSON'S ORDER IS NOT REPAIRED THIS WAY AT ALL ──────────────
//
// Un-pointing an order hands it back to the finder, which is the right answer
// for a demand Core sourced and the wrong one for a demand a person placed by
// hand: a node-local move re-sourced from its own node takes whatever bin is
// standing there now, so the instruction "move THAT bin" quietly becomes "move
// some bin". Those keep their pointer and come back as DisplacedByHand for the
// caller to end out loud. It is the same rule redirectStoreOffDugLane already
// keeps at the destination end — re-aiming a person's order is not a
// recalculation, it is Core overruling somebody.
//
// THE LOG LINE IS THE POINT. A steal that leaves no trace is how this behaviour
// survived unexamined for so long: the holder limped after its bin by id, it
// usually worked, and nothing anywhere said a dig had taken somebody's bin. Now
// it says so, by name, once per victim, at the moment it happens.
func stealSoftHolds(tx *sql.Tx, binID, childID, parentID int64, parkedAt string) ([]DisplacedByHand, []stolenBin, error) {
	// ONE READ, THE BIN'S CLAIM AND ITS FOREIGN SOFT HOLDS. The bin row leads, so
	// the claim is read even when nobody has a soft hold; a row per soft holder
	// follows it (r.order_id NULL when there is none). The claim is the birth's
	// refusal of a blocker held outside the compound — the leg's own claim waits
	// for its handover (ClaimCompoundLegBinTx), and a dig must not be started
	// over a bin a robot is already carrying.
	rows, err := tx.Query(`SELECT b.claimed_by,
		       (b.claimed_by = $2 OR b.claimed_by = $3
		        OR b.claimed_by IN (SELECT id FROM orders WHERE parent_order_id = $3)),
		       r.order_id, COALESCE(o.origin_class,''), COALESCE(o.bin_id,0)
		FROM bins b
		LEFT JOIN reservations r ON r.bin_id = b.id AND r.resource_kind='bin'
		  AND `+reservations.BlockingStateSQL("r.")+`
		  AND r.order_id <> $2
		  AND r.order_id <> $3
		  AND r.order_id NOT IN (SELECT id FROM orders WHERE parent_order_id = $3)
		LEFT JOIN orders o ON o.id = r.order_id
		WHERE b.id = $1
		ORDER BY r.order_id`, binID, childID, parentID)
	if err != nil {
		return nil, nil, fmt.Errorf("read soft holds on bin %d: %w", binID, err)
	}
	// READ THE WHOLE SET BEFORE WRITING ANY OF IT. A transaction runs one
	// statement at a time, so an UPDATE issued while these rows are open kills
	// the query it is iterating.
	var victims []victim
	var claimedBy sql.NullInt64
	var claimOurs sql.NullBool
	found := false
	for rows.Next() {
		found = true
		var holder sql.NullInt64
		var originClass string
		var pointsAt int64
		if err := rows.Scan(&claimedBy, &claimOurs, &holder, &originClass, &pointsAt); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan soft hold on bin %d: %w", binID, err)
		}
		if !holder.Valid {
			continue
		}
		victims = append(victims, victim{
			orderID:    holder.Int64,
			handPlaced: originClass == protocol.OriginClassNoDemand,
			pointsHere: pointsAt == binID,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("read soft holds on bin %d: %w", binID, err)
	}
	rows.Close()
	if !found {
		return nil, nil, &BlockerClaimedError{BinID: binID, ChildID: childID, ParentID: parentID}
	}

	// ── THE RANKED TAKE (§7): A DIG IS NOT A PRIVILEGED MOVE ──────────────
	//
	// A blocker is POSITIONAL — the dig has no choice about which bins are in its
	// way — and that has always been the argument for the steal. It is an argument
	// about WHICH bin, not about whose turn. So the take goes by the demand
	// ranking like every other take, and yielding costs less than it looks: a
	// promise on a bin is always a plan to REMOVE it (stores promise slots, not
	// bins), so the wait ends with the blocker leaving on the winner's drive.
	//
	// BOTH SIDES RESOLVE THROUGH LoadDemandRank, so a leg presents its parent's
	// demand — its own row is priority 0 and the plant's youngest timestamp, and
	// would lose every contest (T2).
	//
	// A REFUSAL IS AN ERROR, NOT A SKIP (T3). The leg's claim only refuses bins
	// whose claimed_by is set, and a promise-holder has none — so a gate that
	// merely declined to un-point a holder would fall through, and the leg's
	// supersedeBinLedger at its handover would evict the winner's row while
	// leaving its pointer stamped: the pointer wedge, built for the winner. The
	// error aborts the transaction and everything unwinds on the caller's defer.
	//
	// SCOPE: EVERY dig, gate-staged included. The gate sits in the one
	// compound-creation door — CreateCompoundOrder → writeCompoundChildren → here
	// (dispatch/compound.go) — and there is no second one, so a robot standing at
	// a mark digging its own lane open contests its blockers on the same ranking
	// as a dig planned from scratch. A gate-staged dig that yields parks under
	// dig-blocker-promised and KEEPS its lane lock, which §R.104 requires: a
	// corridor with a robot in its mouth is not released.
	if err := yieldToOutrankingHolder(tx, victims, binID, childID, parentID); err != nil {
		return nil, nil, err
	}

	// A HARD CLAIM OUTSIDE THE COMPOUND REFUSES THE DIG, after the ranked take
	// and before any write. The holder is a live order whose robot is carrying
	// the blocker out; the dig backs out whole and waits for it, as it always
	// has. Asked here because the leg no longer claims at birth.
	if claimedBy.Valid && !claimOurs.Bool {
		return nil, nil, &BlockerClaimedError{BinID: binID, ChildID: childID, ParentID: parentID,
			HolderID: claimedBy.Int64}
	}

	var displaced []DisplacedByHand
	var stolen []stolenBin
	for _, v := range victims {
		if v.handPlaced && v.pointsHere {
			// The person's order keeps its pointer and loses its row, as below,
			// and only a pending row is taken.
			res, err := tx.Exec(`DELETE FROM reservations WHERE bin_id=$1 AND order_id=$2 AND resource_kind='bin' AND state=$3`,
				binID, v.orderID, string(reservations.StatePending))
			if err != nil {
				return nil, nil, fmt.Errorf("release bin %d from hand-placed holder %d: %w", binID, v.orderID, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				if err := refuseIfConfirmed(tx, binID, childID, parentID, v.orderID); err != nil {
					return nil, nil, err
				}
				continue // the holder let go first: nothing of its was taken
			}
			stolen = append(stolen, stolenBin{
				binID: binID, holderID: v.orderID, digID: parentID, handPlaced: true,
			})
			displaced = append(displaced, DisplacedByHand{
				OrderID: v.orderID, BinID: binID, DigID: parentID, ParkedAt: parkedAt,
			})
			continue
		}
		// THE ROW AND THE POINTER GO IN ONE STATEMENT, so there is no window where
		// a holder's pointer is cleared and its row is not. The row used to be left
		// to supersedeBinLedger, which cleared the bin's whole book in this same
		// transaction when the leg was born claiming it; the leg now takes its own
		// row at its handover (ClaimCompoundLegBinTx), so the steal releases what it
		// takes.
		//
		// The pointer is scoped to a holder still pointing AT THIS BIN: an order
		// that has already moved on to another bin must keep the one it moved to.
		//
		// updated_at IS STAMPED FROM THE INJECTED CLOCK, like every other writer of
		// this column. It was `NOW()` — the one Postgres-clock writer among ~20
		// Go-clock ones, so the rows a dig took a bin from carried a foreign stamp on
		// a column whose readers all assume the other domain. Under the rig's clamp
		// the two agree and nothing showed; the moment the sim clock genuinely runs
		// ahead they do not, and this row's staleness reads as fresh forever. Two
		// readers now care: ListAnomalies' runtime-stuck detector, and the stale-dig
		// disposition's liveness test.
		//
		// ONLY A PENDING ROW IS TAKEN, AND THE POINTER GOES ONLY WITH A ROW TAKEN.
		// A confirmed row is a hand's claim: the robot is coming for this bin,
		// and the dig backs out naming the holder. The count is what tells.
		var taken int
		if err := tx.QueryRow(
			`WITH released AS (DELETE FROM reservations WHERE bin_id=$2 AND order_id=$1 AND resource_kind='bin' AND state=$4 RETURNING 1),
			 unpointed AS (UPDATE orders SET bin_id=NULL, updated_at=$3 WHERE id=$1 AND bin_id=$2 AND EXISTS (SELECT 1 FROM released))
			 SELECT COUNT(*) FROM released`,
			v.orderID, binID, clock.Now().UTC(), string(reservations.StatePending)).Scan(&taken); err != nil {
			return nil, nil, fmt.Errorf("clear bin %d off holder %d: %w", binID, v.orderID, err)
		}
		if taken == 0 {
			if err := refuseIfConfirmed(tx, binID, childID, parentID, v.orderID); err != nil {
				return nil, nil, err
			}
			continue // the holder let go first (ended, released): nothing was taken
		}
		stolen = append(stolen, stolenBin{binID: binID, holderID: v.orderID, digID: parentID})
	}
	return displaced, stolen, nil
}

// yieldToOutrankingHolder is the ranked take (§7, stealSoftHolds): a holder whose
// demand the dig does not outrank keeps the bin, and the dig is refused naming it.
func yieldToOutrankingHolder(tx *sql.Tx, victims []victim, binID, childID, parentID int64) error {
	digRank, err := orders.LoadDemandRank(tx, childID)
	if err != nil {
		return err
	}
	for _, v := range victims {
		// THE HOLDER'S ROW IS LOCKED HERE, in the read the take already makes.
		// The hand claiming this bin for the holder takes that row first (its
		// status CAS), so whichever of the two reaches it second waits for the
		// other and then sees what it did: a hand that confirmed leaves no
		// pending row for the DELETE below, and a steal that took the row leaves
		// the hand's claim no reservation to stand on. No bin row is locked here:
		// the hand holds its order row before the bin, and a bin lock in this
		// read would take them the other way round (LOCKS.md §3).
		holderRank, rerr := orders.LoadDemandRankLocked(tx, v.orderID)
		if rerr != nil {
			return rerr
		}
		if digRank.Outranks(holderRank) {
			continue
		}
		// Outranked, or tied and younger: the incumbent keeps the bin. A tie going
		// to the challenger would let two demands at one priority take the bin from
		// each other on alternate passes.
		log.Printf("dispatch: dig %d YIELDED bin %d to order %d — the holder's demand outranks it "+
			"(holder priority %d since %s, dig priority %d since %s). Digs are not special: the dig "+
			"backs out whole and waits, and the holder taking that bin out is what clears the lane "+
			"for it", parentID, binID, v.orderID,
			holderRank.Priority, holderRank.CreatedAt.Format(time.RFC3339),
			digRank.Priority, digRank.CreatedAt.Format(time.RFC3339))
		return &BlockerClaimedError{
			BinID:    binID,
			ChildID:  childID,
			ParentID: parentID,
			HolderID: v.orderID,
			Promised: true,
		}
	}
	return nil
}

// refuseIfConfirmed is the steal's answer when the holder's pending row was not
// there to take. Read after the holder's order row is held, so it is settled:
// a confirmed row is a hand's claim (a robot is coming for the bin) and the dig
// backs out naming the holder; no row at all is a holder that let go first,
// and there is nothing to take. One read, on that path only.
func refuseIfConfirmed(tx *sql.Tx, binID, childID, parentID, holderID int64) error {
	var confirmed bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM reservations
		WHERE bin_id=$1 AND order_id=$2 AND resource_kind='bin' AND state=$3)`,
		binID, holderID, string(reservations.StateConfirmed)).Scan(&confirmed); err != nil {
		return fmt.Errorf("read holder %d's row on bin %d: %w", holderID, binID, err)
	}
	if confirmed {
		return &BlockerClaimedError{BinID: binID, ChildID: childID, ParentID: parentID, HolderID: holderID}
	}
	return nil
}

// HolderNote is what a taken holder's row says afterwards: the queue detail
// (formatted by the caller, which owns the sentence vocabulary) and the actor the
// history row records.
type HolderNote struct {
	Reason string
	Code   protocol.QueueCode
	Cause  string
	Actor  string
}

// TakeSoftHolds is a person's explicit take of a bin other orders have only
// PROMISED themselves (Q1, owner 2026-09-26; v4 §2.7 Rule 4, "taking a held
// resource is an explicit act, never an accident"). It runs in the caller's
// transaction, the one that births the person's move, so the take and the
// taker's own hold land together or not at all.
//
// ONLY PENDING ROWS. A confirmed row is a robot's; nobody takes it, and the
// planner refuses a claimed bin before this is reached. No rank is asked: the
// person is authoritative over soft holds (the 08-28 ruling). The dig's ranked
// steal (stealSoftHolds) is the other taker, and C2b folds it onto this.
//
// EACH HOLDER'S ROW IS DELETED, and a holder still pointing at the bin is
// un-pointed and told why: its queue detail names the taker, and an
// order_history row at its current status (code bin-taken, ref taken_by) keeps
// the take after the queue columns are overwritten by its next wait. Un-pointed,
// it re-finds through the finder on the next pass instead of confirming a bin
// that is leaving (the pointer wedge stealSoftHolds documents).
//
// A HOLDER THAT IS ITSELF A PERSON'S MOVE is not un-pointed: re-finding a move at
// its node would carry whatever bin stands there now, not the one somebody
// named. Its row is still deleted (the taker's own reservation needs the bin's
// one active row), and it is returned for the caller to end by name after the
// commit, as the dig's steal does with DisplacedByHand.
func TakeSoftHolds(tx *sql.Tx, binID, takerID int64, note HolderNote) (handPlaced []int64, err error) {
	type holder struct {
		orderID    int64
		status     string
		handPlaced bool
		pointsHere bool
	}
	// The holders' order rows are locked here, before their promises are deleted
	// below: a terminalize takes an order row, then its reservations, and a take
	// that deleted the promise first and wrote the order after deadlocked with it
	// (LOCKS.md §3). Ordered by id, so two takes lock holders in one order too.
	rows, err := tx.Query(`SELECT r.order_id, o.status, COALESCE(o.origin_class,''), COALESCE(o.bin_id,0)
		FROM reservations r
		JOIN orders o ON o.id = r.order_id
		WHERE r.bin_id=$1 AND r.resource_kind='bin' AND r.state=$2 AND r.order_id <> $3
		ORDER BY r.order_id
		FOR UPDATE OF o`, binID, string(reservations.StatePending), takerID)
	if err != nil {
		return nil, fmt.Errorf("read soft holds on bin %d: %w", binID, err)
	}
	var holders []holder
	for rows.Next() {
		var h holder
		var originClass string
		var pointsAt int64
		if err := rows.Scan(&h.orderID, &h.status, &originClass, &pointsAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan soft hold on bin %d: %w", binID, err)
		}
		h.handPlaced = originClass == protocol.OriginClassNoDemand
		h.pointsHere = pointsAt == binID
		holders = append(holders, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read soft holds on bin %d: %w", binID, err)
	}
	rows.Close()

	now := clock.Now().UTC()
	ref, _ := json.Marshal(map[string]int64{"taken_by": takerID})
	for _, h := range holders {
		// COUNTED. The holder's row was read pending, but its hand may have
		// confirmed while this take waited on the holder's order row: a confirmed
		// row is a robot coming for the bin, and the take is refused. A row gone
		// is a holder that let go: nothing taken, nobody un-pointed or told.
		res, err := tx.Exec(`DELETE FROM reservations
			WHERE order_id=$1 AND bin_id=$2 AND resource_kind='bin' AND state=$3`,
			h.orderID, binID, string(reservations.StatePending))
		if err != nil {
			return nil, fmt.Errorf("release order %d's hold on bin %d: %w", h.orderID, binID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var confirmed bool
			if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM reservations
				WHERE order_id=$1 AND bin_id=$2 AND resource_kind='bin' AND state=$3)`,
				h.orderID, binID, string(reservations.StateConfirmed)).Scan(&confirmed); err != nil {
				return nil, fmt.Errorf("read order %d's row on bin %d: %w", h.orderID, binID, err)
			}
			if confirmed {
				return nil, fmt.Errorf("bin %d: order %d's claim was confirmed first: %w", binID, h.orderID, reservations.ErrReservationConflict)
			}
			continue
		}
		if !h.pointsHere {
			continue
		}
		if h.handPlaced {
			handPlaced = append(handPlaced, h.orderID)
			continue
		}
		if _, err := tx.Exec(`UPDATE orders SET bin_id=NULL, queue_reason=$3, queue_code=$4, queue_cause=$5, updated_at=$6
			WHERE id=$1 AND bin_id=$2`,
			h.orderID, binID, note.Reason, string(note.Code), note.Cause, now); err != nil {
			return nil, fmt.Errorf("un-point order %d from bin %d: %w", h.orderID, binID, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO order_history (order_id, status, detail, code, actor, ref, created_at)
			 VALUES ($1, $2, $3, 'bin-taken', NULLIF($4,''), $5, $6)`,
			h.orderID, h.status, note.Reason, note.Actor, ref, now); err != nil {
			return nil, fmt.Errorf("record the take on order %d: %w", h.orderID, err)
		}
	}
	return handPlaced, nil
}

// ErrNoSlotTake is TakeSlotByRank declining: no stranger holds the slot by a
// pending reservation, or the taker's demand does not outrank the holder's.
// Nothing was written.
var ErrNoSlotTake = errors.New("no slot to take by rank")

// TakeSlotByRank is a dig leg's take of a slot another order has only promised
// itself (C2b A.3; Rule 2 ⚖, the rank steal of SYNTH Fork 2; P0 ruling Q2). In
// one transaction:
//
//   - the slot's pending holder is read under lock (a confirmed row is a claim
//     acted on, never taken; a holder already sent to the fleet is a robot
//     coming, never taken either), and both demands' ranks are compared under that
//     lock (orders.DemandRank.Outranks; a leg presents its parent's demand), so
//     a rank read before the lock cannot decide it;
//   - the holder's row is deleted, its wait set to the note (slot-taken, naming
//     the demand that took it, named) and a history row written at its status
//     (code slot-taken, ref taken_by) — the bin take's shape (TakeSoftHolds);
//   - the slot is reserved for the taker.
//
// Returns the holder. ErrNoSlotTake when there is no pending holder or the
// taker does not outrank it; a tie keeps the slot with the incumbent, as the bin
// steal does. Cost per take: the locked read, two rank reads, one DELETE, the
// cause write (UPDATE + history INSERT), the reservation INSERT.
func (db *DB) TakeSlotByRank(nodeID, takerID, named int64, note HolderNote) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	holder, err := takeSlotByRankTx(tx, nodeID, takerID, named, note)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return holder, nil
}

// takeSlotByRankTx is TakeSlotByRank on the caller's transaction: a dig leg's
// release (TakeSlotByRank) or its birth's parking pencil (pencilParkingTx).
func takeSlotByRankTx(tx *sql.Tx, nodeID, takerID, named int64, note HolderNote) (int64, error) {
	return takeSlotTx(tx, nodeID, takerID, named, note, SlotTakeByRank, "", "reserveSlot")
}

// SlotTakeMode is the right a take of a stranger's pending slot is made under
// (C2c ruling 2: paper "is taken by rank or yields to a committed robot").
type SlotTakeMode int

const (
	// SlotTakeByRank: the taker's demand outranks the holder's, compared under
	// the lock (a tie keeps the incumbent). The dig leg's take, and since C2c any
	// chooser's (DESIGN-C2c §7.2 generalises the door from dig legs).
	SlotTakeByRank SlotTakeMode = iota
	// SlotTakeByCommitment: the taker is a robot at the group's waiting spot (a
	// committed robot, D13 F5) and the holder was never sent. No rank is asked:
	// "a plan never blocks a committed robot" (HANDOFF §5). The caller vouches for
	// the commitment; the holder's unsent state is re-read under the lock.
	SlotTakeByCommitment
)

// TakeGroupRoomByRank is TakeSlotByRank for a planned group drop's paper at
// acquiring (C2c ruling 2): the same door, the slot reserved for the taker as
// paper (reservations.GroupRoomTag) rather than as a dig leg's parking.
func (db *DB) TakeGroupRoomByRank(nodeID, takerID, named int64, note HolderNote) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	holder, err := takeSlotTx(tx, nodeID, takerID, named, note, SlotTakeByRank, reservations.GroupRoomTag,
		reservations.GroupRoomTag)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return holder, nil
}

// SlotTake asks ConfirmReleaseDrop to take the chosen slot from the stranger
// whose paper is on it, under mode, naming named in the holder's wait.
type SlotTake struct {
	Mode  SlotTakeMode
	Named int64
	Note  HolderNote
}

// ConfirmReleaseDrop is the RELEASE HAND of a gate-staged drop (C2c ruling 3:
// "at release the hand takes the chosen slot and its lane's mouth as armor"),
// one transaction:
//
//   - the lane's advisory lock first, the hand's lock order (dispatch/hand.go);
//   - take, when the chooser answered a stranger's paper: the holder's pending
//     row deleted and its wait written, re-checked under lock (takeSlotTx);
//   - the chosen slot's row for the order (its paper moves with the choice, so
//     the row is acquired when it does not already hold one), then the hard
//     claim and the confirm — paper to armor;
//   - the slot the order leaves, if any and if different: its claim cleared and
//     its row released, take-before-release as the in-lane re-bind did.
//
// Returns the holder taken from, or 0. A refusal anywhere rolls the whole hand
// back, so the order keeps exactly the paper it had. The lane's OCCUPANCY row is
// not taken here: the tail append takes it immediately before the fleet call
// and gives back only what it took when the append fails (appendGateTail), and
// taking it here would hand that rollback a row it did not take.
func (db *DB) ConfirmReleaseDrop(laneID, slotID, orderID, leaveID int64, take *SlotTake) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	// laneID 0 is a group's flat child: there is no corridor to lock, and the
	// slot claim below is what keeps two releases off one child.
	if laneID != 0 {
		if err := reservations.LockLanesTx(tx, laneID); err != nil {
			return 0, fmt.Errorf("lock lane %d for order %d's release: %w", laneID, orderID, err)
		}
	}
	var holder int64
	if take != nil {
		if holder, err = takeSlotTx(tx, slotID, orderID, take.Named, take.Note, take.Mode,
			reservations.GroupRoomTag, reservations.GroupRoomTag); err != nil {
			return 0, err
		}
	} else {
		var held bool
		if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM reservations
			WHERE order_id = $1 AND node_id = $2 AND resource_kind = 'slot')`, orderID, slotID).Scan(&held); err != nil {
			return 0, fmt.Errorf("read order %d's row on slot %d: %w", orderID, slotID, err)
		}
		if !held {
			if err := reservations.AcquireSlot(tx, orderID, slotID, reservations.GroupRoomTag); err != nil {
				return 0, fmt.Errorf("reserve slot %d for order %d: %w", slotID, orderID, err)
			}
		}
	}
	if err := ConfirmSlotClaimTx(tx, slotID, orderID, nil); err != nil {
		return 0, fmt.Errorf("confirm slot %d for order %d: %w", slotID, orderID, err)
	}
	if leaveID != 0 && leaveID != slotID {
		if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE id=$1 AND claimed_by=$2`,
			leaveID, orderID); err != nil {
			return 0, fmt.Errorf("clear order %d's claim on slot %d: %w", orderID, leaveID, err)
		}
		if err := reservations.ReleaseSlot(tx, orderID, leaveID); err != nil {
			return 0, fmt.Errorf("release order %d's row on slot %d: %w", orderID, leaveID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return holder, nil
}

// GiveBackReleaseDrop undoes ConfirmReleaseDrop's armor when the release's tail
// never reached the fleet (RULINGS-C2c-run-through §3.2, the occupancy pin): the
// lane's occupancy is taken at the tail append, after the release transaction,
// and an append refused there leaves the robot at the group's spot. Its claim
// on the slot is cleared and its row is paper again, in one transaction, so it
// waits holding what it held before the release and the next pass chooses
// afresh. A stranger whose paper the release took keeps its re-plan: the take
// is not undone.
func (db *DB) GiveBackReleaseDrop(slotID, orderID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE id=$1 AND claimed_by=$2`,
		slotID, orderID); err != nil {
		return fmt.Errorf("clear order %d's claim on slot %d: %w", orderID, slotID, err)
	}
	if err := reservations.DemoteSlot(tx, orderID, slotID); err != nil {
		return fmt.Errorf("give order %d's slot %d back as paper: %w", orderID, slotID, err)
	}
	return tx.Commit()
}

// takeSlotTx is the one take of a stranger's pending slot, under mode; the slot
// is reserved for the taker tagged reservedBy. onlyTag, when set, takes only a
// row of that tag: a group drop's paper (GroupRoomTag) is taken by a chooser,
// never another kind of pending row; the dig leg's take passes "" as before.
func takeSlotTx(tx *sql.Tx, nodeID, takerID, named int64, note HolderNote, mode SlotTakeMode,
	onlyTag, reservedBy string) (int64, error) {
	var holder int64
	var status string
	// THE HOLDER'S ORDER ROW IS LOCKED BEFORE ITS SLOT ROW (OF o, r: Postgres
	// takes a statement's row locks in the order its FOR UPDATE list names them).
	// The take writes that order row below, and a terminalize takes the same two
	// rows order first (TerminalizeOrderWithReason, then ReleaseByOrder): locked
	// the other way round, a take and a terminalize of one holder each waited for
	// the other until Postgres aborted one (LOCKS.md §3).
	err := tx.QueryRow(`SELECT r.order_id, o.status FROM reservations r JOIN orders o ON o.id = r.order_id
		WHERE r.node_id = $1 AND r.resource_kind = 'slot' AND r.state = $2 AND r.order_id <> $3
		  AND `+orders.AwaitingFleetSQL("o")+`
		  AND ($4::text = '' OR COALESCE(r.reserved_by, '') = $4::text)
		FOR UPDATE OF o, r`, nodeID, string(reservations.StatePending), takerID, onlyTag).Scan(&holder, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoSlotTake
	}
	if err != nil {
		return 0, fmt.Errorf("read the pending holder of slot %d: %w", nodeID, err)
	}
	if mode == SlotTakeByRank {
		takerRank, err := orders.LoadDemandRank(tx, takerID)
		if err != nil {
			return 0, err
		}
		holderRank, err := orders.LoadDemandRank(tx, holder)
		if err != nil {
			return 0, err
		}
		if !takerRank.Outranks(holderRank) {
			return 0, fmt.Errorf("%w: order %d's demand does not outrank holder %d's", ErrNoSlotTake, takerID, holder)
		}
	}
	if _, err := tx.Exec(`DELETE FROM reservations
		WHERE node_id = $1 AND order_id = $2 AND resource_kind = 'slot' AND state = $3`,
		nodeID, holder, string(reservations.StatePending)); err != nil {
		return 0, fmt.Errorf("release order %d's hold on slot %d: %w", holder, nodeID, err)
	}
	now := clock.Now().UTC()
	if _, err := tx.Exec(`UPDATE orders SET queue_reason=$2, queue_code=$3, queue_cause=$4, updated_at=$5
		WHERE id=$1`, holder, note.Reason, string(note.Code), note.Cause, now); err != nil {
		return 0, fmt.Errorf("tell order %d its slot was taken: %w", holder, err)
	}
	ref, _ := json.Marshal(map[string]int64{"taken_by": named})
	if _, err := tx.Exec(
		`INSERT INTO order_history (order_id, status, detail, code, actor, ref, created_at)
		 VALUES ($1, $2, $3, 'slot-taken', NULLIF($4,''), $5, $6)`,
		holder, status, note.Reason, note.Actor, ref, now); err != nil {
		return 0, fmt.Errorf("record the take on order %d: %w", holder, err)
	}
	if err := reservations.AcquireSlot(tx, takerID, nodeID, reservedBy); err != nil {
		return 0, fmt.Errorf("reserve slot %d for order %d: %w", nodeID, takerID, err)
	}
	return holder, nil
}

// stolenBin is one steal, held back for the log that runs AFTER the commit.
//
// The log lines used to fire inside the transaction, at the moment of the write,
// which reported steals that a later rollback un-did — the ranked gate refusing
// a LATER child is enough, and every one of those printed a theft that never
// happened. soakstat counts one line per steal, so an over-count there is a
// measurement of a shape the plant is not in.
type stolenBin struct {
	binID      int64
	holderID   int64
	digID      int64
	handPlaced bool
}

// logSteals emits the steal lines once the transaction they describe has
// committed. The sentences are unchanged: soakstat matches "outranked the holder
// on a positional blocker" across both, one line per event.
func logSteals(stolen []stolenBin) {
	for _, s := range stolen {
		if s.handPlaced {
			log.Printf("dispatch: dig %d took bin %d from HAND-PLACED order %d — the dig outranked "+
				"the holder on a positional blocker, but a person named this bin, so the order keeps its "+
				"pointer and is failed by name rather than re-aimed at whatever is standing there "+
				"next", s.digID, s.binID, s.holderID)
			continue
		}
		log.Printf("dispatch: dig %d took bin %d from order %d — the dig outranked the holder on a "+
			"positional blocker; order %d keeps its demand and re-resolves (the bin is findable at "+
			"its new home)",
			s.digID, s.binID, s.holderID, s.holderID)
	}
}

// ClaimCompoundLegBinTx is a dig leg's bin claim, taken at its handover in the
// leg's own hand (dispatch.acquireHand), not at its birth: a leg waiting its
// turn holds nothing (v4 §2.6).
//
// SIBLING-SCOPED COMPARE-AND-SET. The predicate is deliberately NOT the one
// bins.Claim uses (`claimed_by IS NULL OR claimed_by = $1`). A multi-step plan
// touches one bin more than once — an unbury followed by a retrieve of the same
// bin — and each leg claims it in turn; engine/wiring_completion.go skips the
// delivery-arrival teleport guard for children because of it. THE PARENT IS IN
// THE SET: the planners emit a `retrieve` step carrying the buried target bin,
// which the parent itself may hold by the time a re-planned dig runs.
//
// A refusal is a *BlockerClaimedError naming the holder: the bin is held by an
// order outside the compound. The birth already refused such a bin
// (stealSoftHolds reads the claim), and the parent's dig hold keeps the finders
// and the bin acquire out of the lane after it, so this is the backstop, not the
// rule.
//
// The ledger row follows the claim (supersedeBinLedger): the last leg to claim
// the bin holds its one active row.
func ClaimCompoundLegBinTx(tx *sql.Tx, binID, legID, parentID int64) error {
	// THE STRANGERS' ORDER ROWS FIRST. A hold that landed on this bin after the
	// dig was born (its insert raced the birth's victim read) is a stranger's,
	// and supersedeBinLedger below takes its row and un-points it. Their order
	// rows are locked by the subquery this UPDATE's WHERE evaluates before it
	// reaches the bin row, so the order is the hand's: order rows, then the bin,
	// then reservations (LOCKS.md §3). Same statement.
	res, err := tx.Exec(`WITH strangers AS (
		    SELECT o.id FROM orders o JOIN reservations r ON r.order_id = o.id
		     WHERE r.bin_id = $2 AND r.resource_kind = 'bin'
		       AND o.id <> $3 AND o.parent_order_id IS DISTINCT FROM $3
		     ORDER BY o.id FOR UPDATE OF o)
		UPDATE bins SET claimed_by=$1
		WHERE id=$2
		  AND (SELECT COUNT(*) FROM strangers) >= 0
		  AND (claimed_by IS NULL
		       OR claimed_by = $3
		       OR claimed_by IN (SELECT id FROM orders WHERE parent_order_id = $3))`,
		legID, binID, parentID)
	if err != nil {
		return fmt.Errorf("claim bin %d for leg %d: %w", binID, legID, err)
	}
	if n, rErr := res.RowsAffected(); rErr == nil && n == 0 {
		var holder sql.NullInt64
		if qErr := tx.QueryRow(`SELECT claimed_by FROM bins WHERE id=$1`, binID).Scan(&holder); qErr != nil {
			holder = sql.NullInt64{}
		}
		return &BlockerClaimedError{BinID: binID, ChildID: legID, ParentID: parentID, HolderID: holder.Int64}
	}
	return supersedeBinLedger(tx, binID, legID, parentID)
}

// supersedeBinLedger makes the reservation books say what the claim says: one
// active row on this bin, owned by the child that now holds it.
//
// Delete-then-insert rather than an upsert because the row may currently belong
// to a sibling or to the parent, and the target of uq_reservations_bin_active is
// the BIN — there is no (order, bin) row to update into place. Written CONFIRMED
// because the claim it records is already a hard claim, not a plan: a pending row
// would say the dig is still deciding.
//
// A STRANGER WHOSE ROW GOES IS UN-POINTED WITH IT, in the same statement: a row
// owned by neither the parent nor one of its children is a hold that landed
// after the dig was born, and leaving its pointer would have the stranger
// confirm a bin it holds no row on (the pointer wedge). Its order row is
// already held (ClaimCompoundLegBinTx).
func supersedeBinLedger(tx *sql.Tx, binID, childID, parentID int64) error {
	if _, err := tx.Exec(
		`WITH gone AS (DELETE FROM reservations WHERE bin_id=$1 AND resource_kind='bin' RETURNING order_id)
		 UPDATE orders SET bin_id = NULL, updated_at = $3
		  WHERE id IN (SELECT order_id FROM gone) AND bin_id = $1
		    AND id <> $2 AND parent_order_id IS DISTINCT FROM $2`,
		binID, parentID, clock.Now().UTC()); err != nil {
		return fmt.Errorf("clear bin ledger for bin %d: %w", binID, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO reservations (order_id, resource_kind, bin_id, state, reserved_by, created_at)
		 SELECT $1, 'bin', $2, 'confirmed', 'compound-child', $3 WHERE `+reservations.OwnerLiveSQL(1),
		childID, binID, clock.Now().UTC()); err != nil {
		return fmt.Errorf("write bin ledger for bin %d child %d: %w", binID, childID, err)
	}
	return nil
}

// ListChildOrders returns all child orders for a parent order.
func (db *DB) ListChildOrders(parentOrderID int64) ([]*orders.Order, error) {
	return orders.ListChildren(db.DB, parentOrderID)
}

// ListChildrenCarryingALiftedBin returns the ids of a parent's children that hold
// a claimed bin at _TRANSIT: a robot has lifted it and not yet set it down. The
// lift itself is not audited, so the bin's position is the fact. One statement.
func (db *DB) ListChildrenCarryingALiftedBin(parentOrderID int64) (map[int64]bool, error) {
	rows, err := db.Query(`
		SELECT DISTINCT b.claimed_by FROM bins b
		JOIN orders o ON o.id = b.claimed_by
		WHERE o.parent_order_id = $1
		  AND b.node_id IN (SELECT id FROM nodes WHERE name = '_TRANSIT')`, parentOrderID)
	if err != nil {
		return nil, fmt.Errorf("children of %d carrying a lifted bin: %w", parentOrderID, err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("children of %d carrying a lifted bin: scan: %w", parentOrderID, err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// RetireReshuffleRestoreOrders cancels any non-terminal reshuffle_restore
// housekeeping orders (and their non-terminal children) left over from the
// retired restore-blockers subsystem. One-shot and idempotent: a clean DB — or a
// second run — finds none and returns 0. Cancellation goes through the terminal
// chokepoint (TerminalizeOrder) so any holds are released the same way a normal
// cancel releases them; a raw status write would trip the state-machine guard.
// Returns the number of orders cancelled. Called once at boot.
func (db *DB) RetireReshuffleRestoreOrders() (int, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT id FROM orders WHERE order_type=$1 AND status IN (%s) ORDER BY id`,
		protocol.NonTerminalStatusSQLList()), string(protocol.OrderTypeReshuffleRestore))
	if err != nil {
		return 0, fmt.Errorf("retire reshuffle_restore: list: %w", err)
	}
	var parents []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("retire reshuffle_restore: scan: %w", err)
		}
		parents = append(parents, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	const detail = "retired: restore-blockers subsystem removed"
	cancelled := 0
	for _, pid := range parents {
		children, cErr := db.ListChildOrders(pid)
		if cErr != nil {
			return cancelled, fmt.Errorf("retire reshuffle_restore: children of %d: %w", pid, cErr)
		}
		for _, c := range children {
			if protocol.IsTerminal(c.Status) {
				continue
			}
			// TerminalizeOrder is a compare-and-swap on "still live" and reports
			// whether THIS call is the one that landed it. The IsTerminal check
			// above reads a snapshot, so an order can go terminal underneath us —
			// a concurrent operator cancel is the everyday case. A declined swap
			// is not an error and is not our cancellation, so it is not counted:
			// the returned tally means "orders this sweep terminated", which is
			// what makes the second run report 0.
			swapped, tErr := db.TerminalizeOrder(c.ID, protocol.StatusCancelled, detail)
			if tErr != nil {
				return cancelled, fmt.Errorf("retire reshuffle_restore: cancel child %d: %w", c.ID, tErr)
			}
			if swapped {
				cancelled++
			}
		}
		swapped, tErr := db.TerminalizeOrder(pid, protocol.StatusCancelled, detail)
		if tErr != nil {
			return cancelled, fmt.Errorf("retire reshuffle_restore: cancel parent %d: %w", pid, tErr)
		}
		if swapped {
			cancelled++
		}
	}
	return cancelled, nil
}

// GetNextChildOrder returns the next pending child order for a parent.
func (db *DB) GetNextChildOrder(parentOrderID int64) (*orders.Order, error) {
	return orders.GetNextChild(db.DB, parentOrderID)
}

// SetCompoundOpen is the one writer of a compound parent's sealedness — see
// orders.SetCompoundOpen. Sealed is !open; nothing derives it.
func (db *DB) SetCompoundOpen(parentOrderID int64, open bool) error {
	return orders.SetCompoundOpen(db.DB, parentOrderID, open)
}

func (db *DB) UpdateOrderStatus(id int64, status, detail string) error {
	return orders.UpdateStatus(db.DB, id, status, detail)
}

// UpdateOrderStatusFromWithReason is the CAS status write plus the typed
// reason for the history row (migration 55). The →queued row is the important
// caller: orders.queue_code is overwritten in place, so the history row is the
// only place a queue reason becomes a time series.
func (db *DB) UpdateOrderStatusFromWithReason(id int64, from, to, detail string, reason HistoryReason) (bool, error) {
	return orders.UpdateStatusFromWithReason(db.DB, id, from, to, detail, reason.Code, reason.Actor, reason.refJSON())
}

// ClaimAppend is orders.ClaimAppend: the compare-and-set a fleet append is made
// under.
func (db *DB) ClaimAppend(id int64, waitIndex int, stepsJSON string) (bool, error) {
	return orders.ClaimAppend(db.DB, id, waitIndex, stepsJSON)
}

// UpdateOrderStatusFromWithReasonTx is UpdateOrderStatusFromWithReason on the
// caller's transaction (dispatch.acquireHand's status CAS).
func (db *DB) UpdateOrderStatusFromWithReasonTx(tx *sql.Tx, id int64, from, to, detail string, reason HistoryReason) (bool, error) {
	return orders.UpdateStatusFromWithReasonTx(tx, id, from, to, detail, reason.Code, reason.Actor, reason.refJSON())
}

// UpdateOrderWaitIndex increments the wait_index for a complex order after
// releasing one wait segment.
func (db *DB) UpdateOrderWaitIndex(id int64, waitIndex int) error {
	return orders.UpdateWaitIndex(db.DB, id, waitIndex)
}

// SetOrderQueueDetail stores the blocking reason on a queued order — the
// generated sentence, its structured queue code, and the engineer-only cause —
// in one write. code is typed (protocol.QueueCode) so a caller cannot pass free
// text: the dispatch formatter generates the sentence and is the sole caller.
// Pass "" / empty code to clear (on successful dispatch).
func (db *DB) SetOrderQueueDetail(id int64, reason string, code protocol.QueueCode, cause string) error {
	return orders.SetQueueDetail(db.DB, id, reason, string(code), cause)
}

// LinkOrderSiblingsByEdgeUUID records a two-robot swap pairing keyed on
// edge UUID — see orders.LinkSiblingsByEdgeUUID. Returns rows updated.
func (db *DB) LinkOrderSiblingsByEdgeUUID(uuidA, uuidB string) (int64, error) {
	return orders.LinkSiblingsByEdgeUUID(db.DB, uuidA, uuidB)
}

// OrderSiblingUUID returns the order's two-robot swap sibling edge UUID, or "".
// ReleaseTerminalEdgeUUID frees a deterministic edge_uuid held by a terminal
// order so it can be minted again. See orders.ReleaseTerminalEdgeUUID.
func (db *DB) ReleaseTerminalEdgeUUID(uuid string) (int64, error) {
	return orders.ReleaseTerminalEdgeUUID(db.DB, uuid)
}

func (db *DB) OrderSiblingUUID(id int64) (string, error) {
	return orders.SiblingUUID(db.DB, id)
}

func (db *DB) UpdateOrderVendor(id int64, vendorOrderID, vendorState, robotID string) error {
	return orders.UpdateVendor(db.DB, id, vendorOrderID, vendorState, robotID)
}

func (db *DB) UpdateOrderSourceNode(id int64, sourceNode string) error {
	return orders.UpdateSourceNode(db.DB, id, sourceNode)
}

func (db *DB) UpdateOrderDeliveryNode(id int64, deliveryNode string) error {
	return orders.UpdateDeliveryNode(db.DB, id, deliveryNode)
}

func (db *DB) UpdateOrderRemainingUOP(id int64, remainingUOP *int) error {
	return orders.UpdateRemainingUOP(db.DB, id, remainingUOP)
}

func (db *DB) UpdateOrderStepsJSON(id int64, stepsJSON string) error {
	return orders.UpdateStepsJSON(db.DB, id, stepsJSON)
}

func (db *DB) CompleteOrder(id int64) error             { return orders.Complete(db.DB, id) }
func (db *DB) GetOrder(id int64) (*orders.Order, error) { return orders.Get(db.DB, id) }

// OrderStationUnaware reports orders.station_unaware for one order: whether its
// station has no row for it, so a notice about it goes nowhere. One read, for
// the notice paths that do not already hold the order.
func (db *DB) OrderStationUnaware(id int64) (bool, error) {
	var unaware bool
	err := db.QueryRow(`SELECT station_unaware FROM orders WHERE id=$1`, id).Scan(&unaware)
	return unaware, err
}
func (db *DB) GetOrderByUUID(uuid string) (*orders.Order, error) {
	return orders.GetByUUID(db.DB, uuid)
}

// RecordIntakeRefusal stores Core's refusal of one leg of a pair at intake. See
// orders.IntakeRefusal.
func (db *DB) RecordIntakeRefusal(edgeUUID, stationID, code, detail string) error {
	return orders.RecordIntakeRefusal(db.DB, edgeUUID, stationID, code, detail)
}

// GetIntakeRefusal returns the refusal on record for a uuid, or nil.
func (db *DB) GetIntakeRefusal(edgeUUID string) (*orders.IntakeRefusal, error) {
	return orders.GetIntakeRefusal(db.DB, edgeUUID)
}

// GetOrderRecovering returns the return order that recovers the cancelled
// order cancelledID, or (nil, nil) when none does. See orders.GetRecovering.
func (db *DB) GetOrderRecovering(cancelledID int64) (*orders.Order, error) {
	return orders.GetRecovering(db.DB, cancelledID)
}

func (db *DB) GetOrderByVendorID(vendorOrderID string) (*orders.Order, error) {
	return orders.GetByVendorID(db.DB, vendorOrderID)
}

func (db *DB) ListOrders(status string, limit int) ([]*orders.Order, error) {
	return orders.List(db.DB, status, limit)
}

// ListOrdersFiltered is the orders board's read: status view, ids, search and
// page, in one query.
func (db *DB) ListOrdersFiltered(f orders.Filter) ([]*orders.Order, error) {
	return orders.ListFiltered(db.DB, f)
}

// CountOrdersFiltered is the total the same filter matches with no page applied.
func (db *DB) CountOrdersFiltered(f orders.Filter) (int, error) {
	return orders.CountFiltered(db.DB, f)
}

func (db *DB) ListActiveOrders() ([]*orders.Order, error) { return orders.ListActive(db.DB) }

// ListActiveBoardOrdersFiltered scopes the board to a set of station IDs.
// Empty stations = plant-wide (same as ListActiveBoardOrders).
func (db *DB) ListActiveBoardOrdersFiltered(stations []string) ([]*orders.Order, error) {
	return orders.ListActiveBoardFiltered(db.DB, stations)
}

// ListOrderStations returns the distinct station IDs seen on orders.
func (db *DB) ListOrderStations() ([]string, error) {
	return orders.ListDistinctStations(db.DB)
}

func (db *DB) ListOrderHistory(orderID int64) ([]*orders.History, error) {
	return orders.ListHistory(db.DB, orderID)
}

// GetFaultStats computes the /missions Faults card — see orders.GetFaultStats.
func (db *DB) GetFaultStats(r orders.LeadTimeRange, noticeAfter time.Duration, loc *time.Location) (*orders.FaultStats, error) {
	return orders.GetFaultStats(db.DB, r, noticeAfter, loc)
}

// LatestOrderHistoryForStatus returns the order's most recent row for a status,
// or nil if it never recorded one — see orders.LatestHistoryForStatus.
func (db *DB) LatestOrderHistoryForStatus(orderID int64, status protocol.Status) (*orders.History, error) {
	return orders.LatestHistoryForStatus(db.DB, orderID, status)
}

// LatestOrderHistoryTimesForStatus is the batch form: order id -> when each of
// those orders most recently reached the status, in one round trip. See
// orders.LatestHistoryTimesForStatus.
func (db *DB) LatestOrderHistoryTimesForStatus(orderIDs []int64, status protocol.Status) (map[int64]time.Time, error) {
	return orders.LatestHistoryTimesForStatus(db.DB, orderIDs, status)
}

// LatestOrderHistoryTimes is the status-less form: order id -> the instant of
// each order's most recent transition of ANY status. See
// orders.LatestHistoryTimes.
func (db *DB) LatestOrderHistoryTimes(orderIDs []int64) (map[int64]time.Time, error) {
	return orders.LatestHistoryTimes(db.DB, orderIDs)
}

// LaneWitnesses is orders.LaneWitnesses: Tier 2's read of never-dispatched
// orders against one lane.
func (db *DB) LaneWitnesses(laneID int64, orderIDs []int64) (map[int64]orders.LaneWitness, error) {
	return orders.LaneWitnesses(db.DB, laneID, orderIDs)
}

// OrderEverReachedStatus reports whether the order ever recorded the status —
// see orders.EverReachedStatus.
func (db *DB) OrderEverReachedStatus(orderID int64, status string) (bool, error) {
	return orders.EverReachedStatus(db.DB, orderID, status)
}

func (db *DB) UpdateOrderPriority(id int64, priority int) error {
	return orders.UpdatePriority(db.DB, id, priority)
}

func (db *DB) ListOrdersByStation(stationID string, limit int) ([]*orders.Order, error) {
	return orders.ListByStation(db.DB, stationID, limit)
}

// ListActiveOrdersByStation returns the non-terminal orders for one station —
// the set the order reconcile compares an Edge's own list against.
func (db *DB) ListActiveOrdersByStation(stationID string) ([]*orders.Order, error) {
	return orders.ListActiveByStation(db.DB, stationID)
}

// CountActiveOrdersByDeliveryNode counts non-terminal orders targeting a
// specific delivery node.
func (db *DB) CountActiveOrdersByDeliveryNode(nodeName string) (int, error) {
	return orders.CountActiveByDeliveryNode(db.DB, nodeName)
}

func (db *DB) CountActiveOrdersByDeliveryNodeExcluding(nodeName string, excludeID int64) (int, error) {
	return orders.CountActiveByDeliveryNodeExcluding(db.DB, nodeName, excludeID)
}

// ActiveLaneStores returns non-terminal orders whose delivery_node is one of the
// given slot names — the active stores targeting a lane (tiered-entry gate input).
func (db *DB) ActiveLaneStores(slotNames []string) ([]*orders.Order, error) {
	return orders.ActiveByDeliveryNodes(db.DB, slotNames)
}

// ActiveGateCandidates returns non-terminal orders carrying a plan and a vendor
// order — the set the lane-gate evaluator filters down to its candidates by
// reading the wait step. See orders.ActiveGateCandidates for why the lane is not
// part of the query.
//
// It replaced ActiveLaneRetrieves (and the evaluator's use of ActiveLaneStores),
// which found candidates by matching an ENDPOINT column against lane slot names
// and therefore could not see an order whose lane entry is interior to its plan.
func (db *DB) ActiveGateCandidates() ([]*orders.GateCandidate, error) {
	return orders.ActiveGateCandidates(db.DB)
}

// CountActiveOrders returns the number of non-terminal orders (dashboard
// "in flight" KPI).
func (db *DB) CountActiveOrders() (int, error) {
	return orders.CountActive(db.DB)
}

// ListStalledChapters returns reshuffling parents with an open leg whose whole
// family has been quiet since the cutoff — see orders.ListStalledChapters.
func (db *DB) ListStalledChapters(since time.Time, limit int) ([]int64, error) {
	return orders.ListStalledChapters(db.DB, since, limit)
}

// ListTrackedVendorOrderIDs returns the vendor order IDs Core must keep watching.
// (The old comment here said "all non-terminal orders", which it never was.)
// ListReleasedOpenVendorOrderIDs: see orders.ListReleasedOpenVendorOrderIDs.
func (db *DB) ListReleasedOpenVendorOrderIDs() ([]string, error) {
	return orders.ListReleasedOpenVendorOrderIDs(db.DB)
}

func (db *DB) ListTrackedVendorOrderIDs() ([]string, error) {
	return orders.ListTrackedVendorOrderIDs(db.DB)
}

// ListActiveOrdersBySourceRef returns orders in pre-dispatch states (pending,
// queued) whose source_node matches any of the provided names.
func (db *DB) ListActiveOrdersBySourceRef(names []string) ([]*orders.Order, error) {
	return orders.ListActiveBySourceRef(db.DB, names)
}

// ListAcquiringOrders returns all orders in an acquiring status (queued or
// sourcing) — the fulfillment scanner's retry set — priority then FIFO.
func (db *DB) ListAcquiringOrders() ([]*orders.Order, error) { return orders.ListAcquiring(db.DB) }

// UpdateOrderPayloadCode sets the payload_code on an order.
func (db *DB) UpdateOrderPayloadCode(orderID int64, payloadCode string) error {
	return orders.UpdatePayloadCode(db.DB, orderID, payloadCode)
}

// UpdateOrderBinID sets the bin_id on an order. Kept as a delegate even
// though the function lives in orders/ because every outer caller expects
// this name.
func (db *DB) UpdateOrderBinID(orderID, binID int64) error {
	return orders.UpdateBinID(db.DB, orderID, binID)
}

// ClearOrderBinID forgets which bin an order was going to take, so the scanner
// routes it back through the finder instead of the held-bin arm.
func (db *DB) ClearOrderBinID(orderID int64) error {
	return orders.ClearBinID(db.DB, orderID)
}

// ListOrdersByBin returns recent orders involving a specific bin.
// Cross-aggregate entry point: the query lives in orders/ (returns *orders.Order)
// but callers reach it via the bins-side delegate name.
func (db *DB) ListOrdersByBin(binID int64, limit int) ([]*orders.Order, error) {
	return orders.ListByBinID(db.DB, binID, limit)
}

// ActiveOrdersByBin returns non-terminal orders referencing a bin via bin_id.
// Cross-aggregate delegate for BinService's orphan-order guard on retire/move.
func (db *DB) ActiveOrdersByBin(binID int64) ([]*orders.Order, error) {
	return orders.ActiveByBinID(db.DB, binID)
}

// TerminalizeOrder transitions an order to a terminal status and releases ALL of
// its holds — bin claims, destination-slot claims, order_bins junction rows, and
// reservations (pending and confirmed) — in a single transaction. It is the one
// chokepoint that makes "reaching a terminal status releases everything" a
// structural invariant rather than several divergent write paths; transition()
// routes every IsTerminal target here (including the success terminal
// 'confirmed', whose reservation previously leaked through UpdateOrderStatus and
// bricked the bin via the uq_reservations_bin_active partial unique index).
//
// Any bin still claimed by this order and parked at _TRANSIT when the order
// terminalizes never arrived anywhere, so it is stamped anomalous (anomaly_at,
// the signal operator recovery picks up via ListAnomalousTransitBins) regardless
// of which terminal we reached. In the happy path this matches ZERO rows: a
// delivered bin was moved out of _TRANSIT and unclaimed at delivery time. It
// fires only when an arrival failed or was skipped — including the confirmed
// case where the operator confirmed receipt but the engine's delivery-arrival
// write never landed (the completion safety-net can't recover it because this
// chokepoint has, correctly, already cleared claimed_by). error_detail is
// persisted for every terminal except the clean success 'confirmed' (which would
// otherwise surface receipt text as an "error"); order_history keeps the full
// detail regardless. Cross-aggregate.
// HistoryReason is the typed reason recorded on an order_history row —
// order_history.code / actor / ref (migration 55).
//
// Zero value means "uncoded", which is correct for a transition with no
// category: most of them. Never invent a code to fill it.
type HistoryReason struct {
	Code  string           // protocol.TermCode on a terminal, protocol.QueueCode on a →queued row
	Actor string           // who caused it
	Ref   protocol.TermRef // what it concerns: node / payload / peer
}

// refJSON renders Ref for the JSONB column, or nil when it carries nothing —
// so an empty reference is SQL NULL rather than the string "{}", and the
// partial indexes stay small.
func (r HistoryReason) refJSON() any {
	if r.Ref.Empty() {
		return nil
	}
	b, err := json.Marshal(r.Ref)
	if err != nil {
		return nil
	}
	return string(b)
}

func (db *DB) TerminalizeOrder(orderID int64, status protocol.Status, detail string) (bool, error) {
	return db.TerminalizeOrderWithReason(orderID, status, detail, HistoryReason{})
}

// TerminalizeOrderWithReason is TerminalizeOrder plus the typed reason for the
// history row. Event.ErrorCode and Event.Actor were being set at every Fail
// and Skip call site and dropped in transition(), which passed only the prose
// detail down here — so the categories existed in memory and never reached
// disk.
func (db *DB) TerminalizeOrderWithReason(orderID int64, status protocol.Status, detail string, reason HistoryReason) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// error_detail is suppressed only for the clean success 'confirmed' (which
	// would otherwise surface receipt text as an "error").
	errDetail := detail
	if status == protocol.StatusConfirmed {
		errDetail = ""
	}
	// COMPARE-AND-SWAP on "still live". Two callers can each hold a snapshot
	// showing a non-terminal order and both pass lifecycle's guard — an operator
	// cancel racing a scanner fail is the everyday pair. Unguarded, the second
	// write flipped an already-terminal order to a different terminal and fired a
	// second actionMap entry (fireCancelled AND fireFailed for one order).
	//
	// The predicate is "current status is not terminal" rather than
	// "current status = the caller's from". A cancel is operator intent that must
	// land from ANY live state, and an order legitimately moves non-terminally
	// (queued→sourcing) between snapshot and cancel; keying on `from` would
	// silently refuse that cancel, and CancelOrder returns no error to notice it
	// with. Terminal-absorbs-terminal is the property we actually want.
	//
	// The set is derived from the transition table (protocol.TerminalStatuses),
	// not hard-coded here, so adding a terminal status can't quietly bypass this.
	terminals := protocol.TerminalStatuses()
	terminalNames := make([]string, len(terminals))
	for i, t := range terminals {
		terminalNames[i] = string(t)
	}
	// updated_at from clock.Now(), not the database's NOW(): the daily
	// terminal-volume histogram buckets on COALESCE(completed_at, updated_at),
	// so a DB-clock stamp here puts a sim order's terminal in a different
	// (real-time) day from its creation.
	// THE WAIT GOES WITH THE ORDER. queue_reason/code/cause are a LIVE answer to
	// "what is this order waiting for right now", and nothing cleared them at the
	// end — so an order cancelled or failed while parked kept its last wait on the
	// row and every board went on saying a finished order was still waiting for a
	// slot. Seen in the sim: a dig leg dissolved mid-park, terminal, still reading
	// "Waiting for a slot". SetQueueDetail refuses to WRITE onto a terminal order;
	// this is the other half — the reason does not outlive the order.
	//
	// The history row keeps it. The order waited for that reason, and ending does
	// not unmake the fact; what is cleared is only the live column that claims it
	// is STILL waiting.
	res, err := tx.Exec(`UPDATE orders SET status=$1, error_detail=$2, updated_at=$5,
		queue_reason='', queue_code=NULL, queue_cause=NULL
		WHERE id=$3 AND status <> ALL($4)`, string(status), errDetail, orderID, terminalNames, clock.Now().UTC())
	if err != nil {
		return false, err
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	won := moved > 0
	// History only for the winner — a losing terminalize must not add a second
	// terminal row to the order's audit trail.
	if won {
		if _, err := tx.Exec(
			`INSERT INTO order_history (order_id, status, detail, code, actor, ref, created_at)
			 VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), $6, $7)`,
			orderID, string(status), detail,
			reason.Code, reason.Actor, reason.refJSON(), clock.Now().UTC()); err != nil {
			return false, err
		}
	}
	// EVERYTHING BELOW RUNS FOR THE LOSER TOO, and commits.
	//
	// The loser must not assume the winner released this order's holds. Every
	// release here is keyed on the ORDER id and is idempotent by construction
	// (WHERE claimed_by=$1 matches zero rows the second time; the DELETE and the
	// reservation release are set-based), so running them twice is a no-op — but
	// running them ZERO times because we returned early would strand a claim on a
	// bin forever, which is the exact leak this chokepoint exists to prevent.
	// Idempotent release is an invariant of this function, not an accident.
	// Anomaly mark MUST run before claim release: the WHERE filters on
	// claimed_by=$orderID, which the next statement clears. COALESCE preserves an
	// earlier stamp. Unconditional across terminals — a bin still claimed by this
	// order and parked at _TRANSIT never arrived, whether the order failed, was
	// skipped, or was confirmed with a lost arrival write. Zero rows on the happy
	// path (a delivered bin already left _TRANSIT and dropped its claim).
	if _, err := tx.Exec(`
		UPDATE bins SET anomaly_at=COALESCE(anomaly_at, NOW()), updated_at=NOW()
		WHERE claimed_by=$1
		  AND node_id IN (SELECT id FROM nodes WHERE name='_TRANSIT')`, orderID); err != nil {
		return false, err
	}
	if err := releaseOrderHoldingsTx(tx, orderID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return won, nil
}

// releaseOrderHoldingsTx gives an order's acquisitions back: its hard bin
// claims, its destination-slot claims, its order_bins junction rows, and its
// reservations (pending and confirmed). One transaction, one body, and
// idempotent by construction — every statement is keyed on the order id and
// matches zero rows the second time.
//
// ── WHY IT IS A FUNCTION AND NOT A PARAGRAPH INSIDE TerminalizeOrder ──────
//
// It was written for the terminal path and it is not ABOUT terminality. What it
// encodes is "this order is not going to use these, hand them back", and a
// PARKED order can be in exactly that position: the pair-as-one-job rule refuses
// to dispatch either leg of a coordinated swap unless both can go, so a leg that
// acquired everything it needed and then found its partner blocked must give its
// acquisitions back rather than squat on them while it waits.
//
// Copying the four statements to a second site is how the two would drift — a
// fifth class of holding added to the terminal path and not to the other, which
// is the leak the terminal chokepoint exists to prevent, reintroduced at a new
// door. One body, two callers.
//
// WHAT IT DELIBERATELY DOES NOT DO: the _TRANSIT anomaly stamp. That marks a bin
// that never arrived anywhere and it is a statement about an order that ENDED
// mid-flight. A parked order has dispatched nothing, so there is no flight to
// have been interrupted, and stamping here would put an anomaly on the operator
// recovery board for a swap that is merely waiting for a slot. The stamp stays
// at the terminal call site, where it also must keep running BEFORE this — its
// WHERE reads claimed_by, which the first statement here clears.
//
// LANE MOUTH HOLDS GO TOO. The last statement, reservations.ReleaseByOrder,
// deletes every reservation row the order holds, mouth rows included, on the
// terminal path and the pair release alike; dispatch's ReleaseLanesForOrder,
// which the pair release calls after this, then finds none of this order's
// left. So an order that must keep its lane must not come through here:
// dispatchPairInOnePass keeps a pivot away from releaseLegHoldings for exactly
// that reason (TestPairRule_ASupplyDiggingForItsBinHoldsItsEvac).
func releaseOrderHoldingsTx(tx *sql.Tx, orderID int64) error {
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	// Release this order's destination-slot claims too (store dual of the bin
	// release above); ReleaseOrphanedClaims is the defense-in-depth backstop.
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM order_bins WHERE order_id=$1`, orderID); err != nil {
		return err
	}
	// Release any reservations this order holds (pending or confirmed). Must run
	// in the same tx so no window exists where the order is terminal but its
	// reservation still blocks the bin. The owner-liveness reaper is the
	// defense-in-depth backstop for any row that leaks past this path.
	return reservations.ReleaseByOrder(tx, orderID)
}

// ReleaseOrderHoldings hands back everything an order acquired WITHOUT moving
// its status — the non-terminal twin of the release TerminalizeOrder performs.
//
// The pair-as-one-job rule is its caller: a coordinated swap dispatches both
// legs in one pass or neither, so a leg that reserved a destination, claimed its
// source bins and took its lanes, only for its partner to be refused, must not
// stay parked holding them. Rule 1's relay reading applied to the pair — the
// unit of work is the pair, and an incomplete pair holds nothing.
//
// The order stays exactly where it is (pending or queued) and the scanner
// replays it; this only drops what it was holding.
//
// ── AND THE POINTER GOES WITH THE HOLD ────────────────────────────────────
//
// confirmComplexPlan stamps bin_id when it claims. Handing the claim back and
// keeping the pointer left a parked order naming a bin it no longer held — the
// two books disagreeing, which the end-of-test pointer sweep calls a wedge — and
// anything that read bin_id before the next confirm re-stamped it was reading a
// bin some other order may now own. The next confirm writes it again. The
// terminal path keeps bin_id on purpose: there it is the record of what the
// order carried.
func (db *DB) ReleaseOrderHoldings(orderID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := releaseOrderHoldingsTx(tx, orderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE orders SET bin_id=NULL WHERE id=$1 AND bin_id IS NOT NULL`, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

// FailOrderAtomic transitions an order to "failed" and releases all its holds.
// A failure marks _TRANSIT bins anomalous (a claim released mid-flight is a leak
// to investigate). Thin wrapper over TerminalizeOrder.
func (db *DB) FailOrderAtomic(orderID int64, detail string) error {
	_, err := db.TerminalizeOrder(orderID, protocol.StatusFailed, detail)
	return err
}

// ReleaseClaimForBin is the inverse of a single ClaimForDispatch: it clears the
// bin's claim AND releases its reservation in one transaction. Dispatch-failure
// rollbacks route through this instead of a bare UnclaimBin, which would clear
// claimed_by only and orphan the CONFIRMED reservation ClaimForDispatch leaves
// on success — bricking the bin via uq_reservations_bin_active. Idempotent: a
// not-claimed / not-reserved bin is a harmless no-op.
//
// SCOPED TO THIS ORDER, BOTH HALVES. The claim clear is owner-scoped
// (claimed_by=orderID), and the reservation goes only when that clear actually
// matched, and only this order's row. Two of the three callers run AFTER
// failOrderAndEmit, whose terminalize already released this order's holds; by
// then the bin may be reserved by another order, and the old bin-keyed delete
// removed that order's reservation. Same shape as PlaceBinTx's handoff: the
// reservation lives exactly as long as the claim, released only when the claim
// ended HERE.
func (db *DB) ReleaseClaimForBin(binID, orderID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE id=$1 AND claimed_by=$2`, binID, orderID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if err := reservations.ReleaseByBinForOrder(tx, binID, orderID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DemoteHoldsAfterFleetRefusal takes the ARMOR off an order and leaves its PAPER
// standing, in one transaction. It is the store half of the one fleet-refusal
// door (dispatch.Dispatcher.DemoteAfterFleetRefusal) and has no other caller.
//
// A release is right for a rollback that ABANDONS an order's claims; a fleet
// refusal abandons nothing (§8). So the undo removes only what claimed a robot
// that never came:
//
//	ARMOR OFF. A hard claim means "a robot is committed", and keeping one through
//	a fleet refusal makes the books lie — a rank-proof squatter with no wheels,
//	and a dead corridor for the length of the outage. Owner-scoped both sides.
//
//	PAPER DEMOTED, NEVER DELETED. confirmed → pending, which is exactly what the
//	re-dispatch's confirm needs (it flips pending → confirmed and refuses without
//	a pending row). Deleting it while orders.bin_id stays stamped IS the pointer
//	wedge: the order re-enters through dispatchHeldBin, which confirms by id and
//	never re-acquires, so it parks under claim-failed and retries forever with a
//	live owner no sweep will touch. Bin and slot only — a mouth row has no pending
//	phase and an occupancy row is a measurement, not a promise.
//
//	POINTER AND JUNCTION KEPT. orders.bin_id is untouched: the bin is still spoken
//	for, and on a blip the order re-wins its own uncontested bin seconds later.
//	order_bins is untouched too — the fourth book. A release deletes those rows;
//	the re-dispatch reads them to answer which bin a STEP is about
//	(dispatch/bin_for_step.go), which bin_id cannot answer for a multi-bin order.
//
// releaseLanes is the CALLER's answer to "do these lane rows belong to this
// order", because the store cannot tell: a compound child's mouth rows are held
// by its PARENT (dispatch.laneOwnerFor), and releasing them on a leg's refusal
// tears the corridor out from under a live dig. The door asks laneOwnerFor and
// passes the answer.
//
// Occupancy is NOT touched: commitToFleet's failure arm already released it on
// every arm but a lost CAS, and a lost CAS is refused before this is reached.
//
// Idempotent — the plain path invokes the door twice per refusal — and returns
// the first error rather than logging and continuing, because a half-applied
// demote is the two-books drift this exists to prevent.
func (db *DB) DemoteHoldsAfterFleetRefusal(orderID int64, releaseLanes bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	if _, err := tx.Exec(`UPDATE bins SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return fmt.Errorf("demote: release bin claims for order %d: %w", orderID, err)
	}
	if _, err := tx.Exec(`UPDATE nodes SET claimed_by=NULL, updated_at=NOW() WHERE claimed_by=$1`, orderID); err != nil {
		return fmt.Errorf("demote: release slot claims for order %d: %w", orderID, err)
	}
	if err := reservations.DemoteConfirmedByOrder(tx, orderID); err != nil {
		return fmt.Errorf("demote: demote paper for order %d: %w", orderID, err)
	}
	if releaseLanes {
		if _, err := tx.Exec(
			`DELETE FROM reservations WHERE order_id=$1 AND resource_kind=$2`,
			orderID, string(reservations.KindMouth)); err != nil {
			return fmt.Errorf("demote: release lane holds for order %d: %w", orderID, err)
		}
	}
	return tx.Commit()
}

// CountInFlightOrdersByDeliveryNodeExcluding counts the orders delivering to a
// node that hold a claimed bin — status-blind, see orders.InFlightForDropoffSQL —
// excluding a specific order ID (the caller's own row, so an order re-checking
// after it has claimed does not count itself).
func (db *DB) CountInFlightOrdersByDeliveryNodeExcluding(deliveryNode string, excludeID int64) (int, error) {
	return orders.CountInFlightByDeliveryNodeExcluding(db.DB, deliveryNode, excludeID)
}

// CountLiveOrdersByOrigin counts a demand episode's own non-terminal orders —
// by status, `queued` included, where the dropoff count above reads holders. The
// sizing half of the replenishment bound. See orders.CountLiveByOrigin.
func (db *DB) CountLiveOrdersByOrigin(originID string) (int, error) {
	return orders.CountLiveByOrigin(db.DB, originID)
}

// CountLiveRootsByOrigin counts an episode's own non-terminal ROOT orders — the
// asks it made, not the legs those asks grew. The level keeper's "asked" tally.
// See orders.CountLiveRootsByOrigin for why a dig child must not count.
func (db *DB) CountLiveRootsByOrigin(originID string) (int, error) {
	return orders.CountLiveRootsByOrigin(db.DB, originID)
}

// CountTypedInboundToGroup counts carriers of one type already on their way into
// a group that the level keeper did not ask for. See
// orders.CountTypedInboundToGroup.
func (db *DB) CountTypedInboundToGroup(groupNodeID int64, groupNodeName, binTypeCode string) (int, error) {
	return orders.CountTypedInboundToGroup(db.DB, groupNodeID, groupNodeName, binTypeCode)
}

// ListTypedInboundToGroup is the inbound set CountTypedInboundToGroup counts,
// as rows. See orders.ListTypedInboundToGroup.
func (db *DB) ListTypedInboundToGroup(groupNodeID int64, groupNodeName, binTypeCode string) ([]orders.InboundCarrier, error) {
	return orders.ListTypedInboundToGroup(db.DB, groupNodeID, groupNodeName, binTypeCode)
}

// CountLiveCarrierRequestsByDeliveryNode counts every non-terminal order that
// asked for a CARRIER at a node — queued included, origin-blind, returns
// excluded. "Has a carrier already been asked for here". See
// orders.CountLiveCarrierRequestsByDeliveryNode.
func (db *DB) CountLiveCarrierRequestsByDeliveryNode(deliveryNode string) (int, error) {
	return orders.CountLiveCarrierRequestsByDeliveryNode(db.DB, deliveryNode)
}

func (db *DB) UpdateOrderRobotID(id int64, robotID string) error {
	return orders.UpdateRobotID(db.DB, id, robotID)
}

func (db *DB) ActiveOrderIDsByRobot() (map[string]int64, error) {
	return orders.ActiveIDsByRobot(db.DB)
}

// ListOrdersByOrigin returns a demand episode's own orders, oldest first, with a
// truncation flag. See orders.ListByOrigin.
func (db *DB) ListOrdersByOrigin(originID string, limit int) ([]*orders.Order, bool, error) {
	return orders.ListByOrigin(db.DB, originID, limit)
}

// VacateWaitCandidates: see orders.VacateWaitCandidates.
func (db *DB) VacateWaitCandidates(binID int64) ([]*orders.Order, error) {
	return orders.VacateWaitCandidates(db.DB, binID)
}

// LiveClearForHome: see orders.LiveClearForHome.
func (db *DB) LiveClearForHome(homeName string) (bool, error) {
	return orders.LiveClearForHome(db.DB, homeName)
}

// ClearPullCandidates: see orders.ClearPullCandidates.
func (db *DB) ClearPullCandidates(homeID int64, homeName string, loaderPositions []string) ([]int64, error) {
	return orders.ClearPullCandidates(db.DB, homeID, homeName, loaderPositions)
}
