//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// mirrorLane creates a LANE node with slotCount depth-ordered slots and returns
// its id (a real node so the mouth row's node_id FK is satisfied).
func mirrorLane(t *testing.T, db *store.DB, name string, slotCount int) int64 {
	t.Helper()
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	if err != nil {
		t.Fatalf("get LANE type: %v", err)
	}
	lane := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	for i := 0; i < slotCount; i++ {
		d := i
		slot := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), Enabled: true, ParentID: &lane.ID, Depth: &d}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot: %v", err)
		}
	}
	return lane.ID
}

func digRowCount(t *testing.T, db *store.DB, laneID int64) int {
	t.Helper()
	rows, err := reservations.ActiveMouthRows(db.DB, laneID)
	if err != nil {
		t.Fatalf("ActiveMouthRows: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Mode == reservations.ModeDig {
			n++
		}
	}
	return n
}

// TestLaneLockMirror_WritesAndClearsDigRow: a db-backed lane lock mirrors its
// hold to a durable dig mouth row on TryLock and clears it on Unlock, staying in
// sync (CheckDivergence == 0) throughout.
func TestLaneLockMirror_WritesAndClearsDigRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-MIRROR", 3)
	o := testdb.CreateOrder(t, db)

	ll := NewLaneLockWithDB(db.DB)
	if !ll.TryLock(lane, o.ID) {
		t.Fatal("TryLock on free lane must succeed")
	}
	if got := digRowCount(t, db, lane); got != 1 {
		t.Fatalf("dig rows after TryLock = %d, want 1 (mirror)", got)
	}
	if d := ll.CheckDivergence(); d != 0 {
		t.Fatalf("divergence after TryLock = %d, want 0", d)
	}

	ll.Unlock(lane, o.ID)
	if got := digRowCount(t, db, lane); got != 0 {
		t.Fatalf("dig rows after Unlock = %d, want 0 (mirror cleared)", got)
	}
	if d := ll.CheckDivergence(); d != 0 {
		t.Fatalf("divergence after Unlock = %d, want 0", d)
	}
}

// TestLaneLockMirror_UnlockByOwnerClearsRows: UnlockByOwner clears every dig row
// the order holds.
func TestLaneLockMirror_UnlockByOwnerClearsRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneA := mirrorLane(t, db, "LANE-MIRROR-A", 3)
	laneB := mirrorLane(t, db, "LANE-MIRROR-B", 3)
	o := testdb.CreateOrder(t, db)

	ll := NewLaneLockWithDB(db.DB)
	if !ll.TryLock(laneA, o.ID) || !ll.TryLock(laneB, o.ID) {
		t.Fatal("TryLock on two free lanes must succeed")
	}
	if digRowCount(t, db, laneA) != 1 || digRowCount(t, db, laneB) != 1 {
		t.Fatal("both lanes must have a dig row after locking")
	}
	ll.UnlockByOwner(o.ID)
	if got := digRowCount(t, db, laneA) + digRowCount(t, db, laneB); got != 0 {
		t.Fatalf("dig rows after UnlockByOwner = %d, want 0", got)
	}
}

// TestLaneLockMirror_ForeignUnlockLeavesRow: a wrong-owner Unlock is refused and
// the dig row (and the in-memory hold) stay put — G3, end to end through the
// mirror.
func TestLaneLockMirror_ForeignUnlockLeavesRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := mirrorLane(t, db, "LANE-MIRROR-G3", 3)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	ll := NewLaneLockWithDB(db.DB)
	if !ll.TryLock(lane, a.ID) {
		t.Fatal("A TryLock must succeed")
	}
	ll.Unlock(lane, b.ID) // foreign — refused
	if got := digRowCount(t, db, lane); got != 1 {
		t.Fatalf("dig rows after foreign Unlock = %d, want 1 (A still holds)", got)
	}
	if ll.LockedBy(lane) != a.ID {
		t.Fatalf("lane owner after foreign Unlock = %d, want A (%d)", ll.LockedBy(lane), a.ID)
	}
}
