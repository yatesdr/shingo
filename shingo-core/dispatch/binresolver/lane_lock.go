package binresolver

import (
	"database/sql"
	"fmt"
	"log"
	"sync"

	"shingocore/store/reservations"
)

// LaneLock prevents concurrent reshuffle operations on the same lane.
//
// The in-memory map is the fast read path for IsLocked/LockedBy (called inside
// the group resolver's slot-picking scans) and is authoritative for grant
// decisions. When constructed with a db, every grant and release is ALSO
// write-through to a durable dig mouth reservation row (owner = the complex
// parent) — the dual-write that lets a later phase make the rows the restart-
// durable authority. A nil db (the memory-only constructor) skips the mirror,
// so unit tests exercise the map without a database.
type LaneLock struct {
	mu    sync.Mutex
	lanes map[int64]int64 // laneID -> orderID
	db    *sql.DB         // nil => memory-only (no durable mirror)
}

// NewLaneLock constructs a memory-only lane lock (no durable mirror). Used by
// tests that exercise the map logic directly.
func NewLaneLock() *LaneLock {
	return &LaneLock{lanes: make(map[int64]int64)}
}

// NewLaneLockWithDB constructs a lane lock whose holds are mirrored to durable
// dig mouth rows — the production constructor.
func NewLaneLockWithDB(db *sql.DB) *LaneLock {
	return &LaneLock{lanes: make(map[int64]int64), db: db}
}

// mirrorReservedBy tags the dig mouth rows the lane lock writes, for forensics.
const mirrorReservedBy = "lanelock"

// TryLock attempts to lock a lane for a given order. Returns false if already locked.
//
// With a db, the grant is mirrored to a durable dig mouth row. The in-memory map
// stays authoritative for the grant DECISION (this dual-write phase): a mirror
// conflict or error is logged as a divergence but does not change the answer, so
// behavior is identical to the memory-only lock.
func (l *LaneLock) TryLock(laneID, orderID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.lanes[laneID]; ok {
		return false
	}
	if l.db != nil {
		if err := reservations.AcquireLanes(l.db, orderID, reservations.ModeDig, mirrorReservedBy, laneID); err != nil {
			log.Printf("lanelock: mirror acquire diverged for lane %d order %d: %v (memory grants; rows out of sync)",
				laneID, orderID, err)
		}
	}
	l.lanes[laneID] = orderID
	return true
}

// Unlock releases the lane IF it is held by orderID. A release aimed at a lane
// held by a DIFFERENT order — the G3 foreign-release class — is REFUSED and
// logged; a caller passing the wrong owner can no longer free another order's
// lane. Releasing an unheld lane stays a harmless no-op. The structural fix
// (owner-scoped reservation rows) arrives at P2; this owner-check kills the class
// for every caller during the migration window and surfaces any that still pass
// the wrong owner.
func (l *LaneLock) Unlock(laneID, orderID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if owner, ok := l.lanes[laneID]; ok && owner != orderID {
		log.Printf("lanelock: refused foreign release of lane %d by order %d (held by %d)",
			laneID, orderID, owner)
		return
	}
	delete(l.lanes, laneID)
	if l.db != nil {
		// ReleaseLane is owner-scoped, so the mirror delete removes exactly this
		// order's dig row (and heals a divergence where a row lingers with no
		// memory hold).
		if err := reservations.ReleaseLane(l.db, orderID, laneID); err != nil {
			log.Printf("lanelock: mirror release failed for lane %d order %d: %v", laneID, orderID, err)
		}
	}
}

// UnlockByOwner releases any lane held by the given order, looked up by owner
// rather than lane id. Used on failure/cleanup paths where the caller knows the
// owning order but can't resolve the lane id from the order's children (e.g. a
// DB read failed or the children are gone). Safe no-op if the order holds none.
func (l *LaneLock) UnlockByOwner(orderID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for laneID, owner := range l.lanes {
		if owner == orderID {
			delete(l.lanes, laneID)
		}
	}
	if l.db != nil {
		if err := reservations.ReleaseLanesByOwner(l.db, orderID); err != nil {
			log.Printf("lanelock: mirror release-by-owner failed for order %d: %v", orderID, err)
		}
	}
}

// IsLocked returns true if the lane is currently locked.
func (l *LaneLock) IsLocked(laneID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.lanes[laneID]
	return ok
}

// LockedBy returns the order ID holding the lock, or 0 if unlocked.
func (l *LaneLock) LockedBy(laneID int64) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lanes[laneID]
}

// RebuildFromRows repopulates the in-memory map from the durable dig mouth rows —
// the boot step that makes the rows the restart authority. It REPLACES the
// per-order re-acquire the old boot recovery did: a single bulk read cannot lose
// a lane to a race the way a per-order TryLock could, so the old lost-race window
// is gone. Called once at boot, before any dispatch runs. A no-op without a db.
func (l *LaneLock) RebuildFromRows() error {
	if l.db == nil {
		return nil
	}
	holds, err := reservations.ListDigHolds(l.db)
	if err != nil {
		return fmt.Errorf("lanelock rebuild: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lanes = make(map[int64]int64, len(holds))
	for _, h := range holds {
		l.lanes[h.LaneID] = h.OrderID
	}
	return nil
}

// CheckDivergence compares the in-memory lane holds against the durable dig mouth
// rows and logs any mismatch, returning the count (0 == in sync). A no-op without
// a db. Because write-through keeps memory and rows in lock-step, a non-zero
// result signals a mirror bug or a leaked row — a cheap standing tripwire. (A
// depth-1 lane held in memory has no row by design, but digs never touch depth-1
// lanes, so that benign case does not arise in practice.)
func (l *LaneLock) CheckDivergence() int {
	if l.db == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	holds, err := reservations.ListDigHolds(l.db)
	if err != nil {
		log.Printf("lanelock: divergence check read failed: %v", err)
		return 0
	}
	rowByLane := make(map[int64]int64, len(holds))
	for _, h := range holds {
		rowByLane[h.LaneID] = h.OrderID
	}
	var diverged int
	for laneID, owner := range l.lanes {
		if rowOwner, ok := rowByLane[laneID]; !ok {
			log.Printf("lanelock: divergence — lane %d held by %d in memory but no dig row", laneID, owner)
			diverged++
		} else if rowOwner != owner {
			log.Printf("lanelock: divergence — lane %d held by %d in memory but %d in rows", laneID, owner, rowOwner)
			diverged++
		}
	}
	for laneID, owner := range rowByLane {
		if _, ok := l.lanes[laneID]; !ok {
			log.Printf("lanelock: divergence — lane %d held by %d in rows but not in memory", laneID, owner)
			diverged++
		}
	}
	return diverged
}
