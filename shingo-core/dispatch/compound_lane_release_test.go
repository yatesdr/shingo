//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
)

// R04-2: HandleChildOrderFailure fires once (engine wiring, on the failure
// event) with no retry, so a DB error must not strand the lane lock. Here the
// owning parent can't be resolved (no such order), so the children-derived
// unlock can't find the lane — the owner-based fallback must still release it.
// Before the fix the handler bare-returned on the GetOrder error and left the
// lane locked forever.
func TestHandleChildOrderFailure_ReleasesLaneOnUnresolvableParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, testdb.NewFailingBackend())

	const laneID, missingParentID = 4242, 9999
	if !d.laneLock.TryLock(laneID, missingParentID) {
		t.Fatal("TryLock failed")
	}

	d.HandleChildOrderFailure(missingParentID, 1)

	if d.laneLock.IsLocked(laneID) {
		t.Error("lane still locked after HandleChildOrderFailure on an unresolvable parent; want released")
	}
}
