//go:build docker

package store_test

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// pointer_wedge_assertion_docker_test.go — the detector for the shape this whole
// ownership stream exists to kill.
//
// Contract-v2 clause (iii): an order in the acquiring set, pointing at a bin,
// holding neither a reservation on it nor the claim of it. It is the only
// disagreement between the three ownership books that cannot heal itself —
// bin_id routes the order to dispatchHeldBin, which never re-acquires; the
// confirm underneath requires the reservation row that is missing; the owner is
// alive so no sweep reclaims anything; and reaping is owner-liveness, never age,
// so nothing ages it out. It parks under claim-failed and retries forever.
//
// The assertion has to exist and gate red BEFORE any reservation is ever allowed
// to overlap, because overlapping reservations are what make this shape ordinary
// rather than exotic.

// recorder captures what an assertion reports without failing the real test.
type recorder struct {
	testing.TB
	msgs []string
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

// TestPointerWedge_AssertionCatchesIt manufactures the wedge and asserts the
// detector sees it.
//
// MUTATION (verified): drop the NOT EXISTS arm from the sweep's query and it
// reports every healthy soft hold instead; drop the claimed_by arm and it
// reports every order mid-dispatch.
func TestPointerWedge_AssertionCatchesIt(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "this test builds the wedge on purpose, to pin the detector")
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "WEDGE-BIN")

	// The exact shape: soft hold, pointer stamped, hold then gone. This is what a
	// release that forgets its pointer leaves behind.
	wedged := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "wedge-victim"
		o.Status = protocol.StatusQueued
	})
	testdb.ReserveBin(t, db, wedged.ID, bin.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(wedged.ID, bin.ID), "stamp bin_id")
	_, err := db.DB.Exec(`DELETE FROM reservations WHERE order_id=$1 AND bin_id=$2`, wedged.ID, bin.ID)
	testutil.MustNoErr(t, err, "drop the reservation and leave the pointer")

	rec := &recorder{TB: t}
	testdb.AssertNoPointerWedge(rec, db)

	if len(rec.msgs) != 1 {
		t.Fatalf("the sweep reported %d finding(s), want exactly 1:\n%s",
			len(rec.msgs), strings.Join(rec.msgs, "\n"))
	}
	for _, want := range []string{"POINTER WEDGE", "dispatchHeldBin", "claim-failed"} {
		if !strings.Contains(rec.msgs[0], want) {
			t.Errorf("the finding does not mention %q — this message is the whole product, and it "+
				"is read by somebody who has just found an order that has not moved in an hour:\n%s",
				want, rec.msgs[0])
		}
	}
}

// TestPointerWedge_AssertionLeavesHealthyOwnershipAlone is the other half. A
// sweep that fires on the ordinary states would be turned off within a week, and
// three ordinary states look superficially like the wedge.
func TestPointerWedge_AssertionLeavesHealthyOwnershipAlone(t *testing.T) {
	t.Parallel()
	// The detector's OWN fixture: these rows are probe values chosen to sit either
	// side of one predicate, not a plant state. Before Open — cleanups are LIFO
	// and this registers one that clears the flag.
	testdb.DisableWedgeSweep(t, "this test pins the ownership assertions themselves; its rows are probe values on both sides of the predicate, not a scenario")
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	// 1. A soft hold: pointer AND reservation. The everyday parked order.
	soft := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "wedge-ok-soft" })
	softBin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "WEDGE-OK-SOFT")
	testdb.ReserveBin(t, db, soft.ID, softBin.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(soft.ID, softBin.ID), "stamp soft bin_id")

	// 2. A hard claim whose ledger row was rewritten under it — the dig's
	//    supersede leaves exactly this, and the claim is the live book.
	claimed := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "wedge-ok-claim" })
	claimBin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "WEDGE-OK-CLAIM")
	testdb.ClaimBinForTest(t, db, claimBin.ID, claimed.ID)
	testutil.MustNoErr(t, db.UpdateOrderBinID(claimed.ID, claimBin.ID), "stamp claimed bin_id")
	_, err := db.DB.Exec(`DELETE FROM reservations WHERE order_id=$1`, claimed.ID)
	testutil.MustNoErr(t, err, "rewrite the ledger under the claim")

	// 3. A dispatched order pointing at the bin a robot is carrying. Out of the
	//    acquiring set, and its pointer is a fact rather than a plan.
	gone := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "wedge-ok-dispatched"
		o.Status = protocol.StatusDispatched
	})
	goneBin := testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, "WEDGE-OK-GONE")
	testutil.MustNoErr(t, db.UpdateOrderBinID(gone.ID, goneBin.ID), "stamp dispatched bin_id")

	rec := &recorder{TB: t}
	testdb.AssertNoPointerWedge(rec, db)
	if len(rec.msgs) != 0 {
		t.Errorf("the sweep fired on healthy ownership:\n%s", strings.Join(rec.msgs, "\n"))
	}
}
