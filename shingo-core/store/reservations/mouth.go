package reservations

import (
	"database/sql"
	"fmt"
	"slices"
)

// Mode is a mouth reservation's work direction — the reservations.mode column
// (v51). Only mouth rows carry a mode; bin and slot rows leave it NULL.
//
//	inbound  — the owner drops into the lane (its in-lane blocks are dropoffs)
//	outbound — the owner picks from the lane (its in-lane blocks are pickups)
//	dig      — a reshuffle compound working the lane; ALWAYS exclusive (it does
//	           both directions at once), so a dig admits no other holder and no
//	           other holder admits a dig.
type Mode string

const (
	ModeInbound  Mode = "inbound"
	ModeOutbound Mode = "outbound"
	ModeDig      Mode = "dig"
)

// MouthHold is one active mouth row on a lane: the owning order and its mode.
type MouthHold struct {
	OrderID int64
	Mode    Mode
}

// AcquireLanes takes a mode-tagged mouth hold on each lane for owner, all-or-
// nothing. It is the only writer of mouth rows. DORMANT in P0 — exercised by
// tests only; the dispatch chokepoints wire it in at P4.
//
// The whole call runs in ONE transaction. Lanes are sorted by id and each is
// guarded by a transaction-scoped advisory lock BEFORE its rows are read — two
// acquirers contending for the same lane therefore serialize on the lock, and
// because every lane a call touches is locked in id order, the multi-lane case
// cannot deadlock against another acquirer. Sorting the ids is precisely why the
// design asks for it: the locks are held simultaneously inside the one tx.
//
// Admission, per lane, against the OTHER owners' active mouth rows:
//
//	free (no other rows)                     → admit (insert a row)
//	held, all same mode, neither side dig    → admit, share (insert a row)
//	held different-mode, or dig on either side → ErrReservationConflict
//
// Idempotent: a lane this owner already holds in this mode is skipped (no second
// row). All-or-nothing is structural — a conflict on any lane rolls the whole tx
// back, so there are no partial takes and no advisory lock survives (the durable
// state is the ROWS; the advisory lock is only the serializer and never outlives
// the tx). Depth-1 lanes are EXEMPT: a single-slot lane is already serialized by
// its slot reservation, so no mouth row is taken for it.
//
// A mouth row is inserted 'confirmed': it confirms at fleet-create ("robot
// committed"), with no pending phase — a mouth hold has no paired hard-claim step
// the way a bin or slot reservation does.
//
// On ErrReservationConflict the caller requeues (the queue-and-replay disposition
// every dispatch path already implements); any other error is a transient DB
// failure.
func AcquireLanes(db *sql.DB, owner int64, mode Mode, reservedBy string, laneIDs ...int64) error {
	lanes := sortedUniqueLanes(laneIDs)
	if len(lanes) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("reservations acquire-lanes: begin: %w", err)
	}
	defer tx.Rollback() // no-op once committed; the rollback path is what makes it all-or-nothing

	for _, lane := range lanes {
		exempt, err := laneDepth1Exempt(tx, lane)
		if err != nil {
			return err
		}
		if exempt {
			continue
		}
		// Transaction-scoped advisory lock on the lane id: serializes concurrent
		// acquirers of THIS lane. xact-scoped, so a crash cannot strand it.
		if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, lane); err != nil {
			return fmt.Errorf("reservations acquire-lanes: lock lane %d: %w", lane, err)
		}
		verdict, err := admitMouth(tx, lane, owner, mode)
		if err != nil {
			return err
		}
		switch verdict {
		case admitIdempotent:
			continue // owner already holds this lane in this mode
		case admitConflict:
			return ErrReservationConflict
		case admitFresh:
			if _, err := tx.Exec(
				`INSERT INTO reservations (order_id, resource_kind, node_id, state, reserved_by, mode)
				 VALUES ($1, 'mouth', $2, 'confirmed', $3, $4)`,
				owner, lane, reservedBy, string(mode),
			); err != nil {
				return fmt.Errorf("reservations acquire-lanes: insert lane %d: %w", lane, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reservations acquire-lanes: commit: %w", err)
	}
	return nil
}

type mouthVerdict int

const (
	admitFresh mouthVerdict = iota
	admitIdempotent
	admitConflict
)

// admitMouth reads the lane's active mouth rows and applies the admission rule
// for (owner, mode). It must be called with the lane's advisory lock held.
func admitMouth(tx *sql.Tx, laneID, owner int64, mode Mode) (mouthVerdict, error) {
	holders, err := activeMouthRows(tx, laneID)
	if err != nil {
		return admitConflict, err
	}
	ownerHolds := false
	for _, h := range holders {
		if h.OrderID == owner {
			if h.Mode == mode {
				ownerHolds = true
				continue // our own row — idempotent
			}
			// An order is one mode per lane (§2). Two modes on one lane is a
			// caller bug — surface it loudly rather than insert an incoherent
			// second row.
			return admitConflict, fmt.Errorf(
				"reservations acquire-lanes: order %d already holds lane %d as %s, cannot also hold as %s",
				owner, laneID, h.Mode, mode)
		}
		// A different owner holds the lane. dig excludes everyone (either side);
		// otherwise only an exact same-mode share is admitted.
		if mode == ModeDig || h.Mode == ModeDig || h.Mode != mode {
			return admitConflict, nil
		}
	}
	if ownerHolds {
		return admitIdempotent, nil
	}
	return admitFresh, nil
}

// activeMouthRows returns the active (pending or confirmed) mouth holds on laneID.
func activeMouthRows(q Queryer, laneID int64) ([]MouthHold, error) {
	rows, err := q.Query(
		`SELECT order_id, mode FROM reservations
		 WHERE resource_kind='mouth' AND node_id=$1 AND state IN ('pending','confirmed')
		 ORDER BY order_id`,
		laneID,
	)
	if err != nil {
		return nil, fmt.Errorf("reservations active-mouth-rows: %w", err)
	}
	defer rows.Close()
	var out []MouthHold
	for rows.Next() {
		var h MouthHold
		var mode sql.NullString
		if err := rows.Scan(&h.OrderID, &mode); err != nil {
			return nil, fmt.Errorf("reservations active-mouth-rows scan: %w", err)
		}
		h.Mode = Mode(mode.String)
		out = append(out, h)
	}
	return out, rows.Err()
}

// ActiveMouthRows returns the active mouth holds on laneID — the friction-surface
// read (who holds the lane, in what mode). Safe to call outside the acquire tx.
func ActiveMouthRows(q Queryer, laneID int64) ([]MouthHold, error) {
	return activeMouthRows(q, laneID)
}

// ReleaseLane deletes owner's mouth row on laneID — the per-visit release (§4).
// Owner-scoped by construction: the order_id predicate means a release aimed with
// the wrong owner deletes nothing, so the G3 foreign-release class is structurally
// dead for mouth rows. Idempotent (no row → no-op).
func ReleaseLane(db Execer, owner, laneID int64) error {
	_, err := db.Exec(
		`DELETE FROM reservations WHERE order_id=$1 AND resource_kind='mouth' AND node_id=$2`,
		owner, laneID,
	)
	if err != nil {
		return fmt.Errorf("reservations release-lane: %w", err)
	}
	return nil
}

// ReleaseLaneHandoff is ReleaseLane for the PER-BLOCK early handoff (§4) — it
// deletes owner's mouth row on laneID unless that row is a dig claim.
//
// The handoff and a dig claim have different lifetimes, and conflating them cost
// the dig its row on every reshuffle. A plain order's hold is per-visit: an
// outbound hold exists so nothing else enters while the bin is coming out, and
// once the bin has cleared the lane the hold has done its job. A dig's hold
// spans the WHOLE reshuffle — several legs, several pickups and dropoffs — and
// the first of those pickups is not the end of anything.
//
// It was one call. Because laneOwnerFor routes a child's block progress to the
// compound PARENT (deliberately — children never own rows), the first unbury
// leg's pickup arrived here owned by the parent, matched the parent's dig row,
// and deleted it. Every later leg then ran with no durable claim on the lane.
// Nothing caught it: memory was still the grant authority, the resolver skips
// dig-locked lanes from memory, and CheckDivergence had no production caller.
//
// The mode predicate is in the SQL rather than a read-then-delete in Go, so
// there is no window between deciding and deleting.
//
// LaneLock.Unlock keeps using ReleaseLane, which has no mode predicate: ending
// the dig IS its job, and that is the one caller allowed to drop the claim.
func ReleaseLaneHandoff(db Execer, owner, laneID int64) error {
	_, err := db.Exec(
		`DELETE FROM reservations
		 WHERE order_id=$1 AND resource_kind='mouth' AND node_id=$2
		   AND COALESCE(mode, '') <> $3`,
		owner, laneID, string(ModeDig),
	)
	if err != nil {
		return fmt.Errorf("reservations release-lane-handoff: %w", err)
	}
	return nil
}

// ReleaseLanesByOwner deletes all of owner's mouth rows (any mode, any lane) —
// the row dual of LaneLock.UnlockByOwner's cleanup path. Idempotent.
func ReleaseLanesByOwner(db Execer, owner int64) error {
	_, err := db.Exec(
		`DELETE FROM reservations WHERE order_id=$1 AND resource_kind='mouth'`, owner)
	if err != nil {
		return fmt.Errorf("reservations release-lanes-by-owner: %w", err)
	}
	return nil
}

// DigHold is one active dig mouth row: the lane node and its owning order.
type DigHold struct {
	LaneID  int64
	OrderID int64
}

// ListDigHolds returns every active dig mouth hold across all lanes — the bulk
// read the LaneLock repopulates its in-memory cache from at boot (making the rows
// the restart-durable authority), and the read the divergence check compares
// against.
func ListDigHolds(q Queryer) ([]DigHold, error) {
	rows, err := q.Query(
		`SELECT node_id, order_id FROM reservations
		 WHERE resource_kind='mouth' AND mode=$1 AND state IN ('pending','confirmed')
		 ORDER BY node_id`, string(ModeDig))
	if err != nil {
		return nil, fmt.Errorf("reservations list-dig-holds: %w", err)
	}
	defer rows.Close()
	var out []DigHold
	for rows.Next() {
		var h DigHold
		if err := rows.Scan(&h.LaneID, &h.OrderID); err != nil {
			return nil, fmt.Errorf("reservations list-dig-holds scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// DigHoldOwner returns the order holding a dig on laneID, or 0 if none — the
// single-lane read behind LaneLock.IsLocked / LockedBy.
//
// One row, not a loop: the callers that use it (the lane gate's park decision,
// the expose-mode extension's ownership check) each ask about ONE lane they
// already have in hand. The scanning callers do not come through here at all —
// they filter dig-held lanes out of their candidate query instead.
func DigHoldOwner(q Queryer, laneID int64) (int64, error) {
	rows, err := q.Query(
		`SELECT order_id FROM reservations
		 WHERE resource_kind='mouth' AND node_id=$1 AND mode=$2 AND state IN ('pending','confirmed')
		 ORDER BY order_id
		 LIMIT 1`, laneID, string(ModeDig))
	if err != nil {
		return 0, fmt.Errorf("reservations dig-hold-owner: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err() // no dig on this lane
	}
	var owner int64
	if err := rows.Scan(&owner); err != nil {
		return 0, fmt.Errorf("reservations dig-hold-owner scan: %w", err)
	}
	return owner, rows.Err()
}

// laneDepth1Exempt reports whether laneID is a single-slot (depth-1) lane, which
// is exempt from mouth rows: its one slot's reservation already serializes it
// (§8). Counts the lane's real (non-synthetic) child slots — a lane with 0 or 1
// slot cannot trap a robot, so it needs no mouth gate.
func laneDepth1Exempt(tx *sql.Tx, laneID int64) (bool, error) {
	var n int
	if err := tx.QueryRow(
		`SELECT count(*) FROM nodes WHERE parent_id=$1 AND is_synthetic=false`,
		laneID,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("reservations acquire-lanes: count slots lane %d: %w", laneID, err)
	}
	return n <= 1, nil
}

// sortedUniqueLanes returns the lane ids ascending with duplicates removed — the
// deadlock-free lock order AcquireLanes takes them in.
func sortedUniqueLanes(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	cp := append([]int64(nil), ids...)
	slices.Sort(cp)
	return slices.Compact(cp)
}
