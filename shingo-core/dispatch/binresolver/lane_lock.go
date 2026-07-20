package binresolver

import (
	"log"
	"sync"
)

// LaneLock prevents concurrent reshuffle operations on the same lane.
type LaneLock struct {
	mu    sync.Mutex
	lanes map[int64]int64 // laneID -> orderID
}

func NewLaneLock() *LaneLock {
	return &LaneLock{lanes: make(map[int64]int64)}
}

// TryLock attempts to lock a lane for a given order. Returns false if already locked.
func (l *LaneLock) TryLock(laneID, orderID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.lanes[laneID]; ok {
		return false
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
