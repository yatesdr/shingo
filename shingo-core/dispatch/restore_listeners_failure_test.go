//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// R04-4: if dispatching the restore fails, HandleBinEnteredTransit must KEEP the
// pending_restocks row so a Core restart can recover it — rather than deleting
// the row first and stranding the displaced bins in shuffle slots with no
// record. Failure is induced by breaking the order-creation the restore dispatch
// performs (the pending_restocks table itself is untouched).
func TestPendingRestocks_KeptWhenRestoreDispatchFails(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	testutil.MustNoErr(t, db.SetNodeProperty(grp.ID, PropReshuffleRestoreBlockers, "on"), "set toggle")

	parent := &orders.Order{EdgeUUID: "uuid-pr-disp-fail", StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusQueued}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-PR-DF-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-PR-DF-TGT")
	plan, _ := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.CreateCompoundOrder(parent, plan), "CreateCompoundOrder")
	d.scheduleRestoreIfEnabled(parent, grp.ID, lane.ID, plan, slots[1].ID)

	// Break the order-creation the restore dispatch performs so dispatch fails.
	_, err := db.DB.Exec(`ALTER TABLE orders RENAME COLUMN status TO status_x`)
	testutil.MustNoErr(t, err, "rename orders.status")

	d.HandleBinEnteredTransit(target.ID, slots[1].ID)

	if _, err := db.GetPendingRestockByComplexParent(parent.ID); err != nil {
		t.Error("pending_restock row was deleted even though the restore dispatch failed; want kept for recovery")
	}
}

// R04-3: if the synthetic parent can't be confirmed terminal after the cancel
// attempt, HandleComplexParentTerminal must KEEP the pending_restocks row rather
// than deleting it and leaving a synthetic parent stuck at Reshuffling with no
// recovery record. Failure is induced by breaking the post-cancel status
// re-check (the pending_restocks table itself is untouched).
func TestPendingRestocks_KeptWhenSyntheticParentNotConfirmedTerminal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	testutil.MustNoErr(t, db.SetNodeProperty(grp.ID, PropReshuffleRestoreBlockers, "on"), "set toggle")

	parent := &orders.Order{EdgeUUID: "uuid-pr-syn-keep", StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusQueued}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-PR-SK-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-PR-SK-TGT")
	plan, _ := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.CreateCompoundOrder(parent, plan), "CreateCompoundOrder")
	d.scheduleRestoreIfEnabled(parent, grp.ID, lane.ID, plan, slots[1].ID)

	// Break the synthetic-parent status re-check so terminality can't be confirmed.
	_, err := db.DB.Exec(`ALTER TABLE orders RENAME COLUMN status TO status_x`)
	testutil.MustNoErr(t, err, "rename orders.status")

	d.HandleComplexParentTerminal(parent.ID)

	if _, err := db.GetPendingRestockByComplexParent(parent.ID); err != nil {
		t.Error("pending_restock row was deleted even though the synthetic parent wasn't confirmed terminal; want kept for recovery")
	}
}
