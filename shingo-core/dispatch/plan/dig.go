package plan

import (
	"errors"
	"fmt"

	"shingocore/domain"
)

// dig.go — does this dig fit? The plan-time verdict on an excavation, asked
// before the dig takes a lane, lifts a bin or writes a leg (C2a, v4 §2.3).
//
// THE VERDICT IS len(blockers) + own <= reachable, and the blockers' slots are
// then HELD. blockers is what this dig must park; own is the demand's own drops
// into the dug group that hold no slot there yet (domain.OwnDrop); reachable is
// the shuffle pool as dispatch's walk reads it, which passes by every slot
// already on paper.
//
// ROOM IS HELD, NOT COUNTED (C2b dig-parking ruling, 2026-10-09). The walk's
// slots for the blockers are pencilled at the dig's issue, one pending slot
// reservation per blocker's leg (dispatch planUnbury, store
// HoldLegTx). Running digs' parking is on paper, and the walk
// passes it by, so nothing is "owed" on top: the owed count this verdict carried
// (C2a) counted legs without destinations and held nothing. Two digs 90 ms apart
// counted the same free slots, a store took one, and each lifted leg needed a
// slot only inside the other's locked lane (c2b-demo-visible-923254b1-20m,
// 959/963). A dig that cannot hold a slot per blocker does not start.
//
// OWN IS COUNTED LIKE A BLOCKER (demo, 2026-10-08). A demand whose drop is bound
// for the group it digs in needs a slot there after the dig's shuffles; a
// verdict that counted only the blockers let the shuffles spend the group's
// last free slot, and the demand, holding the bin its dig uncovered, waited on
// a slot its own dig had spent. The demand's own pass reserves that slot; the
// dig keeps room for it.
//
// SHORT IS SHORT, WHATEVER MADE IT SHORT. A pool that is short because the
// room is in a lane a robot is inside answers ErrNoShuffleSlot here, the same
// as a pool with no room (owner, 2026-09-26). The robot's exit from that lane
// already fires the scan that asks again, so nothing is lost at plan time; the
// dwell, a lifted blocker looking for a slot, keeps the distinction, because
// there the robot is loaded and waiting on that exit (dispatch/dig_dwell.go).
//
// AND A GRANTED MOUTH IS NOT DIGGABLE (Lane F's study; the orc's amendment,
// ruling 1). A lane whose mouth slot the gate has granted to another order — a
// placer released into it, its slot claimed, its bin not yet landed — has a
// bin about to stand in front of every leg this dig would lift (s1-after
// 660fd228: order 233 landed bin 10 at LSD_001 in front of dig 242's legs). The
// dig waits for the placer to land or leave; asked first, because no count
// makes it diggable. Only a slot IN FRONT of the deepest blocker counts: one
// behind them is where the dig's requester is waiting to put its bin. And only
// a placer that has been released: an order staged at a mark holds its claim
// without being on its way in, and may be waiting on this very dig.
//
// Owner's standing ruling (dispatch/reshuffle.go, planUnbury): if the count
// cannot be satisfied, that is an engineering and configuration capacity
// problem, not a dispatch one. Short is a wait, never a failure.

// ErrNoShuffleSlot is the verdict "this dig does not fit the group's parking
// now". Dispatch parks the demand under CauseNoShuffleSlot; a slot freeing in
// the group releases it.
var ErrNoShuffleSlot = errors.New("no free shuffle slot")

// ErrLaneOccupiedParking is the shuffle walk's Hold B refusal: the parking a
// dig needs is in a lane a robot is inside. DigFits answers it as short; the
// dwell reads it as itself.
var ErrLaneOccupiedParking = errors.New("the parking this dig needs is in a lane a robot is inside")

// ErrLaneMouthHeld is a lane whose mouth another order holds. The shuffle walk
// says it of a parking lane (dispatch.LaneMouthHeldParkingError); the verdict
// says it of the dug lane itself (DugMouthGrantedError).
var ErrLaneMouthHeld = errors.New("a lane this dig needs has its mouth held by another order")

// DigDB is the verdict's own read: one statement (store.DB.DigFitFacts).
type DigDB interface {
	DigFitFacts(groupID, laneID int64, frontOf int, asker domain.DigAsker, demandID int64) (domain.DigFitFacts, error)
}

// Parking is dispatch's shuffle walk, asked whether need slots are reachable
// now: nil when they are, else why not. It reads; it writes nothing.
type Parking func(need int) error

// DigShortError is a dig that does not fit: what it needed, and the walk's own
// account of the shortfall. It unwraps to ErrNoShuffleSlot and to nothing else,
// so no caller at plan time reads a short pool as one of the walk's narrower
// refusals.
type DigShortError struct {
	Blockers int
	Own      int
	Walk     error
}

func (e *DigShortError) Error() string {
	walk := e.Walk.Error()
	if !errors.Is(e.Walk, ErrNoShuffleSlot) {
		walk = ErrNoShuffleSlot.Error() + ": " + walk
	}
	return fmt.Sprintf("%s (this dig needs %d: %d blocker(s) + %d for the demand's own drop)",
		walk, e.Blockers+e.Own, e.Blockers, e.Own)
}

func (e *DigShortError) Unwrap() error { return ErrNoShuffleSlot }

// DugMouthGrantedError is a dig refused because its own lane's mouth is
// granted to a placer not yet landed. It unwraps to ErrLaneMouthHeld.
type DugMouthGrantedError struct {
	Lane   string
	Slot   string
	Holder int64
}

func (e *DugMouthGrantedError) Error() string {
	return fmt.Sprintf("%s: %s's slot %s is granted to order %d, not yet landed",
		ErrLaneMouthHeld, e.Lane, e.Slot, e.Holder)
}

func (e *DugMouthGrantedError) Unwrap() error { return ErrLaneMouthHeld }

// DigFits answers whether a dig of blockers bins out of lane fits now; frontOf
// is the depth of the deepest of them, behind which a granted slot is the
// dig's purpose, not its hazard. nil
// means it fits. A *DugMouthGrantedError means the lane's mouth is granted to
// another order. A *DigShortError means the group's parking is short. Any
// other error is the walk's narrower refusal (parking another dig or demand
// holds, a mouth held or unread), passed through for its own cause.
func DigFits(db DigDB, groupID int64, lane string, laneID int64, blockers, frontOf int, asker domain.DigAsker, own domain.OwnDrop, parking Parking) (domain.DigFitFacts, error) {
	defer observe(db, "dig-fits")()
	facts, err := db.DigFitFacts(groupID, laneID, frontOf, asker, own.DemandID)
	if err != nil {
		// Cannot tell what is owed or granted, so cannot say the dig fits: a
		// wait, the disposition every other read on this path takes.
		return facts, &DigShortError{Blockers: blockers, Walk: fmt.Errorf("could not read the dig's facts: %w", err)}
	}
	if facts.GrantedTo != 0 {
		return facts, &DugMouthGrantedError{Lane: lane, Slot: facts.GrantedSlot, Holder: facts.GrantedTo}
	}
	mine := own.Owed(facts)
	err = parking(blockers + mine)
	switch {
	case err == nil:
		return facts, nil
	case errors.Is(err, ErrNoShuffleSlot), errors.Is(err, ErrLaneOccupiedParking):
		return facts, &DigShortError{Blockers: blockers, Own: mine, Walk: err}
	}
	return facts, err
}

// DigStillFits is the verdict re-asked in the dig's hand (SYNTH Shape 4, C2.1):
// the facts read again on the transaction that starts the dig, against the ones
// the plan-time verdict passed on. nil means nothing it counted has changed
// against it. A *DugMouthGrantedError means the lane's mouth was granted since.
// A *DigShortError means the demand's own drop needs more room than it did.
//
// The blockers' parking is not re-asked here: it is held in this same
// transaction by the legs' pencils, and a slot another dig or a store took
// since the walk refuses the pencil's insert (store.ParkingTakenError), the
// same wait.
//
// The demand's own drop is counted on both sides (own.Owed): it grows only if
// the demand gave up a slot it held in the group since the plan.
func DigStillFits(planned, now domain.DigFitFacts, lane string, blockers int, own domain.OwnDrop) error {
	if now.GrantedTo != 0 {
		return &DugMouthGrantedError{Lane: lane, Slot: now.GrantedSlot, Holder: now.GrantedTo}
	}
	was, is := own.Owed(planned), own.Owed(now)
	if is > was {
		return &DigShortError{Blockers: blockers, Own: is, Walk: fmt.Errorf(
			"%w: the demand's own drop needs %d slot(s) in the group, %d more than when this dig was planned",
			ErrNoShuffleSlot, is, is-was)}
	}
	return nil
}
