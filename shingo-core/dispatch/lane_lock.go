package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"shingocore/dispatch/plan"
	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// LaneLock prevents concurrent reshuffle operations on the same lane.
//
// IT LIVES IN dispatch, NOT binresolver (B1): it writes dig rows, and binresolver
// is a part of the pure planner, which holds nothing. A production dig takes its
// lane in the parent's hand (digLaneTake), not through TryLockFor.
//
// THE DURABLE ROW IS THE LOCK. There is no in-memory map: a lane is held iff a
// dig mouth reservation row exists for it, and every question and every change
// goes to that row. LaneLock is a named wrapper over those reads and writes,
// kept because "does a dig hold this lane" reads better than an inline query at
// the call sites that ask it.
//
// SINCE §R.101 THAT SENTENCE IS TWO QUESTIONS, and the methods below are split
// accordingly: DigOwner / IsLocked / LockedBy ask whether the lane is held
// EXCLUSIVELY, which is what every keep-out decision needs and what a source
// hold answers yes to; ExcavationOwner asks whether what holds it is a
// reshuffle, which is what a REPORT needs. See reservations.IsExcavation.
//
// It used to be a map with the rows mirrored alongside, memory authoritative for
// the grant. That arrangement had TWO WRITERS FOR ONE FACT, and it failed
// exactly the way two writers do: the per-block early release deleted a dig's
// row at its first unbury leg while memory went on believing the lane was held,
// and nothing noticed because memory was the one being asked. The fix for the
// deletion was a mode exemption; the fix for the CLASS is this — one writer.
//
// The grant decision is now AcquireLanes' transaction, which takes a
// transaction-scoped advisory lock on the lane before reading its rows. That is
// strictly stronger than the mutex it replaces: the mutex serialized one
// process, the advisory lock serializes every writer against the same row.
//
// Two consequences worth knowing at the call sites:
//
//   - A dig can now be refused by a NON-dig hold. The map only ever knew about
//     digs, so a lane an ordinary order was inside looked free to it. Rows know
//     about every mode, and AcquireLanes refuses dig-versus-anything. This is a
//     behaviour change and it is the safe direction.
//
//   - Depth-1 lanes ARE NOW GATED LIKE ANY OTHER, and this bullet used to say
//     the opposite: "Depth-1 lanes take no mouth row at all (AcquireLanes
//     exempts them — a single-slot lane is already serialized by its slot
//     reservation), so TryLock reports success and IsLocked then reports false.
//     Digs never touch depth-1 lanes: nothing can be buried in a lane with one
//     slot."
//
//     The last sentence is false. It is true of the lane a dig EXCAVATES and
//     false of the lane it PARKS A BLOCKER IN — the lane-stress seed's LS_S1,
//     LS_S2 and LS_S3 are single-slot lanes it calls "the cheapest shuffle
//     destinations in the group". So digs touch them constantly, and every one
//     of those visits was ungated. The consequence the bullet described is gone
//     with it: TryLock on a single-slot lane now writes a row, and IsLocked
//     then reports true.
type LaneLock struct {
	db *sql.DB
}

// NewLaneLockWithDB constructs the lane lock. The db is not optional — the rows
// ARE the lock, so there is nothing for a memory-only variant to be.
func NewLaneLockWithDB(db *sql.DB) *LaneLock {
	return &LaneLock{db: db}
}

// reservedBy tags the dig mouth rows the lane lock writes. It is no longer only
// forensics: since §R.101 gave every demand's source hold the dig MODE, this tag
// is what separates an excavation from a source lock, and reservations.
// IsExcavation is the one reader of it. Every production dig takes its lane
// through digLaneTake, so every excavation carries it.
const mirrorReservedBy = reservations.ByExcavation

// digLaneTake is a dig's lane, taken IN THE PARENT'S HAND (v4 §2.6; SYNTH Fork
// 1): the dig-mode mouth row is written in the transaction that writes the legs
// (store.CreateCompoundChildren), after the parent's row is read under lock. A
// dig is started whole or not at all — there is no lane held by a dig whose legs
// were refused, and no standalone lock transaction ahead of the legs.
//
// TWO PLANS ARE TWO BELIEFS (§2.3). The planner's DigAdmissible is the cheap
// pre-check; the lane's advisory lock and admitMouth inside this transaction are
// the arbiter, so of two digs planned against one lane exactly one commits and
// the other's transaction rolls back holding nothing (digLaneRefused).
//
// asker and staged are what TryLockFor carried: the order the dig is raised for
// (its own holds on the lane are upgraded, not refused) and the orders standing
// at the lane's group marks, whose robots are outside the corridor.
//
// aim, when not zero, is the bin a coordinated demand's own dig uncovers
// (aimForDig). The demand soft-reserves it in the same transaction.
type digLaneTake struct {
	laneID int64
	asker  reservations.DigAsker
	staged reservations.StagedOutside
	aim    int64
}

// inTx is the lane step for ownerID's hand; nil for a nil take, which writes no
// lane (a test fixture's compound). The lane is locked before the parent's row
// and admitted after it (store.CompoundLanes).
//
// THE AIM IS THE DEMAND'S OWN SOFT HOLD, TAKEN ONE PASS EARLY (C2b A.1b). It is
// the pending bin row the demand's next pass writes anyway (reserveComplexPlan,
// through ReserveForDispatch: the same reservations.Acquire, the same tag), so
// that pass finds it held and writes nothing. What it buys is the gap between
// the dig's end and that pass: the burial guards read a live holder's pending row
// as "coming for this bin" (helpers.ComingForBinSQL), so the slots the dig just
// emptied cannot re-bury the bin, and the dig's lane row need not outlive the
// dig (handOffDugLane). The owner is the parent, which holds the dig, so the
// acquire's dig arm exempts it.
//
// A CONFLICT IS NOT A REFUSAL OF THE DIG. The bin already carries a live
// holder's row, and that row protects it the same way, whoever owns it. It must
// not surface either: digLaneRefused reads ErrReservationConflict as "the lane
// is someone else's".
//
// AND THE VERDICT IS RE-ASKED HERE (SYNTH Shape 4, C2.1), when fit is not nil:
// the facts the plan-time verdict passed on are read again on this transaction,
// after the lane's advisory lock and the parent's row, and a dig whose mouth was
// granted since, or whose group now owes running digs more than it did, is
// refused here holding nothing (plan.DigStillFits). +1 SELECT per dig start.
func (t *digLaneTake) inTx(ownerID int64, fit *DigFit) *store.CompoundLanes {
	if t == nil {
		return nil
	}
	return &store.CompoundLanes{LaneIDs: []int64{t.laneID}, Admit: func(tx *sql.Tx) error {
		if fit != nil {
			now, err := store.DigFitFactsTx(tx, fit.GroupID, fit.LaneID, fit.FrontOf, fit.Asker, fit.Own.DemandID)
			if err != nil {
				return &plan.DigShortError{Blockers: fit.Blockers,
					Walk: fmt.Errorf("could not read the dig's facts in its hand: %w", err)}
			}
			if err := plan.DigStillFits(fit.Facts, now, fit.Lane, fit.Blockers, fit.Own); err != nil {
				return err
			}
		}
		if err := reservations.AdmitLockedLanesTx(tx, ownerID, reservations.ModeDig, t.asker,
			reservations.StagedOutsideByLane{t.laneID: t.staged}, mirrorReservedBy, t.laneID); err != nil {
			return err
		}
		if t.aim == 0 {
			return nil
		}
		if err := reservations.Acquire(tx, ownerID, ownerID, t.aim, "reserveForDispatch"); err != nil &&
			!errors.Is(err, reservations.ErrReservationConflict) {
			return err
		}
		return nil
	}}
}

// aimForDig is the bin a dig raised for requester should soft-reserve in the
// requester's name: binID for a coordinated demand that has not been sent, 0
// otherwise. A plain buried retrieve carries its aim on its row and its fetch is
// one of its own legs; a gate-staged requester has been sent and holds what its
// hand took.
func aimForDig(requester *orders.Order, binID int64) int64 {
	if requester == nil || !IsCoordinated(requester) || IsGateStaged(requester) {
		return 0
	}
	return binID
}

// digLaneRefused reports a dig whose lane step refused: another holder owns the
// lane, or the parent ended under it. Nothing was written. Any other error out
// of the compound write is the store's (a blocker refusal, a fault).
func digLaneRefused(err error) bool {
	return errors.Is(err, reservations.ErrReservationConflict) || errors.Is(err, reservations.ErrOwnerEnded)
}

// TryLock attempts to lock a lane for a given order. Returns false if the lane
// is already held — by another dig, or by an ordinary order's mouth hold.
//
// A read/write error also returns false: FAIL CLOSED. Refusing to start a dig is
// recoverable (the caller queues and retries); starting one on a lane whose
// state could not be established is not.
func (l *LaneLock) TryLock(laneID, orderID int64) bool {
	// nil beside Anyone, and for the same reason: no asker, so no exemption. A
	// fixture parking a foreign dig means the owner-blind question.
	return l.TryLockFor(laneID, orderID, reservations.Anyone, nil)
}

// TryLockFor is TryLock for a dig raised to rescue a particular order:
// beneficiary's own mouth holds do not refuse it.
//
// NO PRODUCTION DIG COMES THROUGH HERE ANY MORE: a dig takes its lane in the
// parent's hand, with its legs (digLaneTake), carrying the same asker. This is
// the standalone form a test fixture uses to park a dig on a lane; TryLock above
// is its beneficiary-less form.
//
// The rule and its limits live on reservations.AcquireLanesFor; this only
// carries the asker down to it.
func (l *LaneLock) TryLockFor(laneID, orderID int64, beneficiary reservations.DigAsker, stagedOutside reservations.StagedOutside) bool {
	// Single-lane by construction, so the by-lane map is this one entry. Keeping
	// the flat set in this signature is deliberate: a caller that has one lane in
	// hand should not have to build a map to say something about it.
	err := reservations.AcquireLanesFor(l.db, orderID, reservations.ModeDig, beneficiary,
		reservations.StagedOutsideByLane{laneID: stagedOutside}, mirrorReservedBy, laneID)
	if err == nil {
		return true
	}
	if !errors.Is(err, reservations.ErrReservationConflict) && !errors.Is(err, reservations.ErrOwnerEnded) {
		log.Printf("lanelock: acquire failed for lane %d order %d: %v (treated as held)", laneID, orderID, err)
	}
	return false
}

// Unlock releases the lane IF it is held by orderID. Owner-scoping is structural
// rather than checked: ReleaseLane's WHERE names the owner, so a release aimed
// at another order's lane deletes nothing. Releasing an unheld lane is a no-op.
//
// This is the ONE path allowed to drop a dig claim — ending the dig is its job.
// The per-block early handoff goes through ReleaseLaneHandoff, which exempts the
// dig mode precisely so it cannot do this by accident.
func (l *LaneLock) Unlock(laneID, orderID int64) {
	if err := reservations.ReleaseLane(l.db, orderID, laneID); err != nil {
		log.Printf("lanelock: release failed for lane %d order %d: %v", laneID, orderID, err)
	}
}

// UnlockByOwner releases every lane held by the given order, looked up by owner
// rather than lane id, and RETURNS THE LANES IT FREED. Safe no-op if the order
// holds none.
//
// The owner is the whole key on purpose. Resolving "which lane does this dig
// hold" from anywhere else — the order's children, a column, a plan struct — is
// deriving a fact the reservation row already IS, and the derivation was wrong
// in two ways at once: it answered with the FIRST child's lane, so a compound
// that took locks on more than one lane leaked the rest, and after a re-plan the
// first child belongs to a superseded generation and names a lane the dig no
// longer holds.
//
// The returned ids are what the caller must re-evaluate: a dropped lane lock is
// a lane-clearing event, and it is the one event the gate's trigger set cannot
// produce for itself (every other trigger fires from a bin or an order changing,
// and all of those have already fired by the time a dig releases).
//
// On a read/write failure it returns nothing, which costs the dwellers a wake-up
// rather than correctness — the rows are either gone or still there, and the
// next release or event re-asks.
func (l *LaneLock) UnlockByOwner(orderID int64) []int64 {
	freed, err := reservations.ReleaseLanesByOwner(l.db, orderID)
	if err != nil {
		log.Printf("lanelock: release-by-owner failed for order %d: %v", orderID, err)
		return nil
	}
	return freed
}

// LanesHeldBy snapshots the lanes this order holds, for a caller that will tear
// the order down and then needs to wake whoever was waiting behind those lanes.
//
// TAKE IT BEFORE THE TEARDOWN. Terminalizing an order deletes its reservations
// in the same transaction as the status write, so this read is empty for any
// order that has already been failed or confirmed — and an empty answer there
// is indistinguishable from "held nothing", which is how a dropped lane silently
// wakes nobody.
//
// A read failure returns nothing, which costs a wake-up rather than correctness.
func (l *LaneLock) LanesHeldBy(orderID int64) []int64 {
	held, err := reservations.LanesHeldByOwner(l.db, orderID)
	if err != nil {
		log.Printf("lanelock: lanes-held read failed for order %d: %v", orderID, err)
		return nil
	}
	return held
}

// DigOwner returns the order ID holding the dig on this lane, 0 if unheld, and
// the read error UNANSWERED. One row read, and the only one: IsLocked and
// LockedBy are dispositions over this, not second spellings of the query.
//
// It exists because the two dispositions below disagree, on purpose, and a
// caller that can PROPAGATE the failure wants neither of them. The lane-gate
// retrieve classifier is that caller: its own error return already means "leave
// the order parked", so handing it a fabricated answer would replace a correct
// disposition with a guess.
func (l *LaneLock) DigOwner(laneID int64) (int64, error) {
	return reservations.DigHoldOwner(l.db, laneID)
}

// ExcavationOwner returns the order whose EXCAVATION holds this lane, or 0 when
// the lane's dig hold is §R.101's source lock (a demand owning the lane it
// resolved onto) or there is none.
//
// IT IS NOT A KEEP-OUT READ AND MUST NOT BECOME ONE. DigOwner above answers
// whether anything may enter, and a source lock excludes exactly as hard as a
// reshuffle — that is §R.101's ruling and gating on this instead would reverse
// it. This answers only what to CALL a refusal that has already been decided, at
// the two sites that put a word in front of an engineer: admission's cause and
// the lane-hold classifier. See reservations.IsExcavation.
//
// The read error is UNANSWERED, like DigOwner's, because both callers already
// have a refusal in hand and want to choose a name rather than be handed one.
func (l *LaneLock) ExcavationOwner(laneID int64) (int64, error) {
	return reservations.ExcavationOwner(l.db, laneID)
}

// IsLocked reports whether a dig holds this lane.
//
// FAILS CLOSED: an unreadable lane reports LOCKED. Every caller uses this to
// decide whether to keep out, and "I could not tell" must not read as "go
// ahead". The scanning callers do not come through here — they filter dig-held
// lanes out of their candidate query instead, so this is only ever asked about
// one lane the caller already has in hand.
func (l *LaneLock) IsLocked(laneID int64) bool {
	owner, err := l.DigOwner(laneID)
	if err != nil {
		log.Printf("lanelock: dig-hold read failed for lane %d: %v (treated as held)", laneID, err)
		return true
	}
	return owner != 0
}

// CanTake reports whether TryLock would currently succeed for this lane.
//
// IT IS THE PRE-CHECK IsLocked WAS BEING USED AS AND IS NOT. IsLocked asks "does
// a DIG hold this lane"; TryLock refuses on ANY other owner's mouth row. A caller
// that gates on IsLocked and then does expensive, DURABLE work before TryLock —
// the heal-dig path creates its parent order in between — pays that cost on every
// attempt against a lane an ordinary order is holding, forever, because the
// answer never changes. 16,947 cancelled orders and no dig at all, measured.
//
// FAILS CLOSED on a read error, like IsLocked and TryLock: "I could not tell"
// must not read as "go ahead".
//
// It does NOT replace TryLock and must not be treated as a claim: AcquireLanes
// under the lane's advisory lock is still the arbiter, and the window between
// this and that is real. What it removes is the certainty of losing.
func (l *LaneLock) CanTake(laneID int64) bool {
	// nil beside Anyone — see TryLock. The pre-check and the acquire keep the
	// same pairing in their blind forms as in their aware ones.
	return l.CanTakeFor(laneID, reservations.Anyone, nil)
}

// CanTakeFor is CanTake for a dig raised to rescue a particular order — the
// pre-check paired with the dig's lane step (digLaneTake), asking the same
// question with the same exemption. Asking the blind one here and the aware one at the acquire would
// leave the pre-check refusing digs the acquire would have admitted, which is
// the 16,947 shape with the two answers swapped round.
//
// THAT IS WHY stagedOutside IS A PARAMETER HERE AND NOT A LOOKUP INSIDE. Both
// halves of the pair take the set from the SAME caller, computed once for the
// lane in question, so the pre-check and the acquire cannot be looking at two
// different answers to "who is standing at the mark" — which is the same
// drift, one layer down, that the paragraph above is about.
func (l *LaneLock) CanTakeFor(laneID int64, beneficiary reservations.DigAsker, stagedOutside reservations.StagedOutside) bool {
	ok, err := reservations.DigAdmissible(l.db, laneID, beneficiary, stagedOutside)
	if err != nil {
		log.Printf("lanelock: dig-admissible read failed for lane %d: %v (treated as held)", laneID, err)
		return false
	}
	return ok
}

// LockedBy returns the order ID holding the dig, or 0 if unheld.
//
// Unlike IsLocked this returns 0 on a read error, because its callers compare
// the result against a specific order id and a fabricated non-zero owner would
// be a wrong ANSWER rather than a cautious one. A 0 fails those comparisons,
// which is the cautious outcome there.
func (l *LaneLock) LockedBy(laneID int64) int64 {
	owner, err := l.DigOwner(laneID)
	if err != nil {
		log.Printf("lanelock: dig-hold read failed for lane %d: %v (reporting unheld)", laneID, err)
		return 0
	}
	return owner
}
