package binresolver

import "testing"

func TestLaneLock_TryLockUnlock(t *testing.T) {
	ll := NewLaneLock()
	if !ll.TryLock(1, 100) {
		t.Fatal("first TryLock on free lane must succeed")
	}
	if ll.TryLock(1, 200) {
		t.Fatal("second TryLock on held lane must fail")
	}
	if got := ll.LockedBy(1); got != 100 {
		t.Fatalf("LockedBy(1) = %d, want 100", got)
	}
	ll.Unlock(1)
	if ll.IsLocked(1) {
		t.Fatal("lane must be free after Unlock")
	}
	if !ll.TryLock(1, 200) {
		t.Fatal("TryLock after Unlock must succeed")
	}
}

func TestLaneLock_IndependentLanes(t *testing.T) {
	ll := NewLaneLock()
	if !ll.TryLock(1, 100) || !ll.TryLock(2, 200) {
		t.Fatal("distinct lanes must be lockable concurrently")
	}
	if ll.LockedBy(1) != 100 || ll.LockedBy(2) != 200 {
		t.Fatal("owners do not leak across lanes")
	}
	ll.Unlock(1)
	if ll.IsLocked(1) || !ll.IsLocked(2) {
		t.Fatal("unlocking one lane must not affect the other")
	}
}

func TestLaneLock_LockedByUnknown(t *testing.T) {
	ll := NewLaneLock()
	if ll.IsLocked(42) {
		t.Fatal("unknown lane must not report locked")
	}
	if got := ll.LockedBy(42); got != 0 {
		t.Fatalf("LockedBy on unknown lane = %d, want 0", got)
	}
}
