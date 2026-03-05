package dispatch

import "sync"

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

// Unlock releases the lock on a lane.
func (l *LaneLock) Unlock(laneID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.lanes, laneID)
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
