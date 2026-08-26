//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/reservations"
)

// The lane lock's contracts, moved from binresolver/lane_lock_test.go when the
// in-memory map was deleted and the durable rows became the lock itself.
//
// Every assertion below is the one the map-based test made. What changed is the
// fixture: a lane is now a real node, an owner is now a real order (mouth rows
// carry a foreign key to orders), and the answers come from the database rather
// than from a mutex-guarded map. That is the whole point — the property was
// never "the map says so", it was "the lane is held", and only one of those two
// survives a restart or a second writer.
//
// They live in dispatch/ rather than binresolver/ because they need a Postgres
// container, and the docker fixtures (testdb, mirrorLane) are here.

// TestLaneLockRows_TryLockUnlock: mutual exclusion, ownership, and reuse after
// release.
func TestLaneLockRows_TryLockUnlock(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-LLROW-1", 3)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	ll := NewLaneLockWithDB(db.DB)
	if !ll.TryLock(lane, a.ID) {
		t.Fatal("first TryLock on free lane must succeed")
	}
	if ll.TryLock(lane, b.ID) {
		t.Fatal("second TryLock on held lane must fail")
	}
	if got := ll.LockedBy(lane); got != a.ID {
		t.Fatalf("LockedBy = %d, want %d", got, a.ID)
	}
	ll.Unlock(lane, a.ID)
	if ll.IsLocked(lane) {
		t.Fatal("lane must be free after Unlock")
	}
	if !ll.TryLock(lane, b.ID) {
		t.Fatal("TryLock after Unlock must succeed")
	}
}

// TestLaneLockRows_IndependentLanes: holds do not leak between lanes.
func TestLaneLockRows_IndependentLanes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneA := mirrorLane(t, db, "LANE-LLROW-A", 3)
	laneB := mirrorLane(t, db, "LANE-LLROW-B", 3)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	ll := NewLaneLockWithDB(db.DB)
	if !ll.TryLock(laneA, a.ID) || !ll.TryLock(laneB, b.ID) {
		t.Fatal("distinct lanes must be lockable concurrently")
	}
	if ll.LockedBy(laneA) != a.ID || ll.LockedBy(laneB) != b.ID {
		t.Fatal("owners leaked across lanes")
	}
	ll.Unlock(laneA, a.ID)
	if ll.IsLocked(laneA) || !ll.IsLocked(laneB) {
		t.Fatal("unlocking one lane must not affect the other")
	}
}

// TestLaneLockRows_RefusesForeignRelease is the G3 characterization test,
// carried over. Order A holds lane L; order B's completion path calls Unlock(L)
// naming B. Under the old unconditional Unlock the lane freed — the
// foreign-release bug — and it must not.
//
// It is stronger against rows than it was against the map. The map compared
// owners in Go and refused a mismatch; ReleaseLane names the owner in its WHERE,
// so a foreign release does not match a row to delete in the first place. The
// class is structurally dead rather than guarded against.
func TestLaneLockRows_RefusesForeignRelease(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-LLROW-G3", 3)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	ll := NewLaneLockWithDB(db.DB)
	if !ll.TryLock(lane, a.ID) {
		t.Fatal("A must acquire the free lane")
	}

	ll.Unlock(lane, b.ID) // B tries to release A's lane
	if !ll.IsLocked(lane) {
		t.Fatal("foreign release by B freed A's lane (G3 bug); it must be refused")
	}
	if got := ll.LockedBy(lane); got != a.ID {
		t.Fatalf("after refused foreign release, owner = %d, want A (%d)", got, a.ID)
	}

	ll.Unlock(lane, a.ID)
	if ll.IsLocked(lane) {
		t.Fatal("owner A's own release must free the lane")
	}
}

// TestLaneLockRows_UnknownLane: an unheld lane reports unheld, and an unknown
// lane id is not an error.
func TestLaneLockRows_UnknownLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-LLROW-FREE", 3)

	ll := NewLaneLockWithDB(db.DB)
	if ll.IsLocked(lane) {
		t.Fatal("an unheld lane must not report locked")
	}
	if got := ll.LockedBy(lane); got != 0 {
		t.Fatalf("LockedBy on an unheld lane = %d, want 0", got)
	}
	if got := ll.LockedBy(999999); got != 0 {
		t.Fatalf("LockedBy on a nonexistent lane = %d, want 0", got)
	}
}

// TestLaneLockRows_NonDigHoldBlocksADig is NEW, and it is the behaviour change
// the map could not have.
//
// The map only ever knew about digs, so a lane an ordinary order was already
// inside looked free to it and a dig would start straight into occupied
// territory. Rows know every mode, and AcquireLanes refuses dig-versus-anything.
func TestLaneLockRows_NonDigHoldBlocksADig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-LLROW-MODE", 3)
	plain := testdb.CreateOrder(t, db)
	digger := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, plain.ID, reservations.ModeOutbound, "test", lane); err != nil {
		t.Fatalf("acquire plain outbound hold: %v", err)
	}

	ll := NewLaneLockWithDB(db.DB)
	if ll.TryLock(lane, digger.ID) {
		t.Fatal("a dig started on a lane an ordinary order already holds — rows know every mode, " +
			"and dig excludes everyone")
	}
}
