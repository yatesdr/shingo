package binresolver

import "testing"

func TestLaneLock_TryLockUnlock(t *testing.T) {
	t.Parallel()
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
	ll.Unlock(1, 100)
	if ll.IsLocked(1) {
		t.Fatal("lane must be free after Unlock")
	}
	if !ll.TryLock(1, 200) {
		t.Fatal("TryLock after Unlock must succeed")
	}
}

func TestLaneLock_IndependentLanes(t *testing.T) {
	t.Parallel()
	ll := NewLaneLock()
	if !ll.TryLock(1, 100) || !ll.TryLock(2, 200) {
		t.Fatal("distinct lanes must be lockable concurrently")
	}
	if ll.LockedBy(1) != 100 || ll.LockedBy(2) != 200 {
		t.Fatal("owners do not leak across lanes")
	}
	ll.Unlock(1, 100)
	if ll.IsLocked(1) || !ll.IsLocked(2) {
		t.Fatal("unlocking one lane must not affect the other")
	}
}

// TestLaneLock_RefusesForeignRelease is the G3 characterization test. Order A
// holds lane L; order B's completion path calls Unlock(L) with B as the owner.
// Under the old unconditional Unlock the lane freed (the foreign-release bug);
// now the release is refused and the lane stays held by A. A's own release still
// works.
func TestLaneLock_RefusesForeignRelease(t *testing.T) {
	t.Parallel()
	ll := NewLaneLock()
	const laneL, orderA, orderB = int64(1), int64(100), int64(200)

	if !ll.TryLock(laneL, orderA) {
		t.Fatal("A must acquire the free lane")
	}

	// B tries to release A's lane — refused; the lane stays held by A.
	ll.Unlock(laneL, orderB)
	if !ll.IsLocked(laneL) {
		t.Fatal("foreign release by B freed A's lane (G3 bug); it must be refused")
	}
	if got := ll.LockedBy(laneL); got != orderA {
		t.Fatalf("after refused foreign release, owner = %d, want A (%d)", got, orderA)
	}

	// A's own release works.
	ll.Unlock(laneL, orderA)
	if ll.IsLocked(laneL) {
		t.Fatal("owner A's own release must free the lane")
	}
}

func TestLaneLock_LockedByUnknown(t *testing.T) {
	t.Parallel()
	ll := NewLaneLock()
	if ll.IsLocked(42) {
		t.Fatal("unknown lane must not report locked")
	}
	if got := ll.LockedBy(42); got != 0 {
		t.Fatalf("LockedBy on unknown lane = %d, want 0", got)
	}
}
