package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestCancelLockPin_ACancelNeverConfirmsUnderItsCellLocks (LOCKS.md, E2): the
// changeover cancel holds the keep-staged cells' prime locks from its aborts to
// its reconcile. Aborting a task's leg ran the completion cascade on the same
// goroutine, and the orphan handler read the cancel's own abort as an operator
// cancelling one order: it ran the changeover's completion, which confirms a
// delivered linked order, and that confirm's keep-spot kick sends back a refill
// landed on a spot the incoming style does not keep, under the prime lock of
// the cell the cancel already holds. A sync.Mutex taken twice on one goroutine
// waits for itself: every REQUEST on the cell, and every release act that
// sweeps it, waits with it.
//
// It needs the active style already flipped while the changeover is active (an
// admin set-active-style mid-changeover has no guard), a live leg, and a
// delivered leg that landed on the kept spot carrying the outgoing part. RED at
// 545b8b57: the cancel never returns.
//
// The intent worker runs, as on a started Edge: with none, a release wake drains
// on the waking goroutine (release_act.go, drainIntentsInline) and the guard
// stops it first, for taking a node lock under the cell's.
func TestCancelLockPin_ACancelNeverConfirmsUnderItsCellLocks(t *testing.T) {
	t.Parallel()
	fx := seedLanding(t, protocol.ClaimRoleConsume, lineRows(protocol.ClaimRoleConsume))
	fx.eng.startIntentWorker() // as a started Edge runs: release acts on their worker
	coID := startKSChangeover(t, fx)
	task, err := fx.db.GetChangeoverNodeTaskByNode(coID, fx.nodeIDs["L1"])
	testutil.MustNoErr(t, err, "the line's task")

	line := fx.nodeIDs["L1"]
	live, err := fx.db.CreateOrder("pin-live-leg", protocol.OrderTypeRetrieve, &line, false, 1,
		"L1", "", "SRC-NEW", protocol.Height{}, false, "PART-NEW", "", "")
	testutil.MustNoErr(t, err, "a live leg")
	testutil.MustNoErr(t, fx.db.UpdateOrderStatus(live, string(protocol.StatusInTransit)), "flying")
	landed, err := fx.db.CreateOrder("pin-landed-leg", protocol.OrderTypeRetrieve, &line, false, 1,
		"SPOT", "", "SRC-OLD", protocol.Height{}, false, "PART-OLD", "", "")
	testutil.MustNoErr(t, err, "a landed leg")
	testutil.MustNoErr(t, fx.db.UpdateOrderStatus(landed, string(protocol.StatusDelivered)), "delivered")
	_, err = fx.db.DB.Exec(`UPDATE changeover_node_tasks SET next_material_order_id=?, old_material_release_order_id=? WHERE id=?`,
		live, landed, task.ID)
	testutil.MustNoErr(t, err, "link both legs to the task")
	testutil.MustNoErr(t, fx.db.SetActiveStyle(fx.processID, &fx.toStyleID), "the style flipped mid-changeover")

	done := make(chan error, 1)
	go func() { done <- fx.eng.CancelProcessChangeover(fx.processID) }()
	select {
	case err := <-done:
		testutil.MustNoErr(t, err, "cancel")
	case <-time.After(10 * time.Second):
		t.Fatal("the changeover cancel did not return in 10 s: it waits on a cell lock it holds")
	}
}
