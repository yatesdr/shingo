package orders

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
)

// HoldAtRest is one order that is waiting while it holds something hard.
type HoldAtRest struct {
	OrderID int64
	Status  string
	Bins    int // bins.claimed_by = this order
	Slots   int // nodes.claimed_by = this order
	// Confirmed reservation rows, by kind. A pending reservation is a soft hold
	// and never counts.
	SlotRes   int // resource_kind 'slot'
	Mouths    int // resource_kind 'mouth', except a dig lock (DigLocksAtRestSQL)
	Occupancy int // resource_kind 'occupancy'
}

// Line renders the hold in the one form every reader prints.
func (h HoldAtRest) Line() string {
	return fmt.Sprintf("order %d is %s holding %d bin(s), %d slot(s), %d confirmed slot reservation(s), "+
		"%d confirmed mouth(s), %d confirmed occupancy row(s)",
		h.OrderID, h.Status, h.Bins, h.Slots, h.SlotRes, h.Mouths, h.Occupancy)
}

// HoldsAtRestSQL selects every order that has not reached `dispatched` and still
// holds something HARD: (id, status, bins, slots, slot reservations, mouths,
// occupancy).
//
// HARD MEANS (owner ruling, 2026-09-25):
//   - bins.claimed_by and nodes.claimed_by — the claim columns;
//   - a CONFIRMED slot reservation;
//   - a CONFIRMED lane mouth or occupancy row.
//
// A pending reservation never counts: that is the soft hold a waiting order is
// supposed to keep.
//
// A DIG LOCK IS COUNTED APART (owner, 2026-09-25). The reshuffle planner takes a
// lane for the order whose bin is buried before that order is re-parented onto
// its dig (proposeLaneClearDig: LaneLock.TryLockFor, then CreateCompoundOrder),
// and a demand that finished its own excavation keeps the lane as an outbound
// hold until it has its bin. Both are the dig parent holding its lane, which
// moves into the hand in C2; neither is taken by the acquire this invariant
// guards. They are the mouth rows whose reserved_by is one of DigLockTags, and
// DigLocksAtRestSQL reports them. Every other confirmed mouth counts here. The kinds are spelled as literals because store/orders may
// not import store/reservations; they are reservations.KindSlot, KindMouth and
// KindOccupancy, and state 'confirmed' is the only non-pending state the table
// allows.
//
// THE INVARIANT IS THAT THIS RETURNS NOTHING. A hard hold is taken for the fleet
// call; an order that is waiting has no fleet call to take it for, so whatever
// it holds hard is withheld from every other order for as long as it waits.
//
// Compound legs (parent_order_id set) are excepted: a dig leg is born holding
// its part of the parent's hand, and moving that hold to the handover is its own
// phase. The population is the pre-dispatch predicate from protocol, not a typed
// list, so a status joining that family joins this check with it.
//
// One spelling for its readers: soakstat's invariant, the engine
// characterisation and the verify-red pins. soakstat's books-over-time ledger
// derives the same flag from its own sample, and TestLedger_ReadingsEqualTheSQL
// holds the two equal (the sim rig's old psql probe is gone).
func HoldsAtRestSQL() string {
	return fmt.Sprintf(`
		SELECT o.id, o.status,
		       (SELECT COUNT(*) FROM bins b WHERE b.claimed_by = o.id),
		       (SELECT COUNT(*) FROM nodes n WHERE n.claimed_by = o.id),
		       (SELECT COUNT(*) FROM reservations r WHERE %[2]s AND r.resource_kind = 'slot'),
		       (SELECT COUNT(*) FROM reservations r WHERE %[2]s AND r.resource_kind = 'mouth' AND NOT %[3]s),
		       (SELECT COUNT(*) FROM reservations r WHERE %[2]s AND r.resource_kind = 'occupancy')
		FROM orders o
		WHERE o.status IN (%[1]s)
		  AND o.parent_order_id IS NULL
		  AND (EXISTS (SELECT 1 FROM bins b WHERE b.claimed_by = o.id)
		    OR EXISTS (SELECT 1 FROM nodes n WHERE n.claimed_by = o.id)
		    OR EXISTS (SELECT 1 FROM reservations r WHERE %[2]s
		               AND (r.resource_kind IN ('slot', 'occupancy')
		                    OR (r.resource_kind = 'mouth' AND NOT %[3]s))))
		ORDER BY o.id`, protocol.PreDispatchStatusSQLList(), confirmedResOf, digLockRow)
}

// DigLockTags are the reserved_by tags of a dig lock's mouth rows:
// reservations.ByExcavation (LaneLock.TryLockFor, the one door every production
// dig comes through) and reservations.ByDigHandoff (the lock a demand keeps once
// its own excavation is done). Literals because store/orders may not import
// store/reservations; a test in store/reservations holds them equal.
var DigLockTags = []string{"lanelock", "dighandoff"}

// digLockRow is the mouth-row filter for a dig lock.
const digLockRow = `COALESCE(r.reserved_by, '') IN ('lanelock', 'dighandoff')`

// DigLockAtRest is one order that is waiting while it holds a dig lock.
type DigLockAtRest struct {
	OrderID int64
	Status  string
	Lanes   int // confirmed mouth rows tagged as a dig lock
}

// Line renders the hold in the one form every reader prints.
func (h DigLockAtRest) Line() string {
	return fmt.Sprintf("order %d is %s holding a dig lock on %d lane(s)", h.OrderID, h.Status, h.Lanes)
}

// DigLocksAtRestSQL selects every order in HoldsAtRestSQL's population that holds
// a confirmed dig-lock mouth: (id, status, lanes). It is reported, not gated:
// nonzero is today's shape until C2 puts the dig parent's lane in the hand.
//
// A parent in `reshuffling` is not in the population, for either query: it has
// been re-parented onto its dig and is not pre-dispatch. What this sees is the
// lock held by an order still in `pending` or `queued` — the moment
// between the planner's lock and the re-parent, and the requeued demand that
// kept its lane after its own dig.
func DigLocksAtRestSQL() string {
	return fmt.Sprintf(`
		SELECT o.id, o.status,
		       (SELECT COUNT(*) FROM reservations r WHERE %[2]s AND r.resource_kind = 'mouth' AND %[3]s)
		FROM orders o
		WHERE o.status IN (%[1]s)
		  AND o.parent_order_id IS NULL
		  AND EXISTS (SELECT 1 FROM reservations r WHERE %[2]s AND r.resource_kind = 'mouth' AND %[3]s)
		ORDER BY o.id`, protocol.PreDispatchStatusSQLList(), confirmedResOf, digLockRow)
}

// ListDigLocksAtRest runs DigLocksAtRestSQL.
func ListDigLocksAtRest(db *sql.DB) ([]DigLockAtRest, error) {
	rows, err := db.Query(DigLocksAtRestSQL())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DigLockAtRest
	for rows.Next() {
		var h DigLockAtRest
		if err := rows.Scan(&h.OrderID, &h.Status, &h.Lanes); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// confirmedResOf is the reservation-row filter every column above shares.
const confirmedResOf = `r.order_id = o.id AND r.state = 'confirmed'`

// ListHoldsAtRest runs HoldsAtRestSQL.
func ListHoldsAtRest(db *sql.DB) ([]HoldAtRest, error) {
	rows, err := db.Query(HoldsAtRestSQL())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HoldAtRest
	for rows.Next() {
		var h HoldAtRest
		if err := rows.Scan(&h.OrderID, &h.Status, &h.Bins, &h.Slots, &h.SlotRes, &h.Mouths, &h.Occupancy); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
