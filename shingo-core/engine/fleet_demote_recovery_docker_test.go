//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// TestFleetRecovery_TwoDemotedOrdersToOneLineOnlyOneGoes pins what keeps a fleet
// outage from double-booking a delivery node, now that the dropoff count reads
// holders rather than statuses.
//
// A fleet refusal demotes an order: its armor comes off (claimed_by cleared), its
// paper is demoted to pending, it keeps its bin pointer and parks in `sourcing`
// (DemoteAfterFleetRefusal). A demoted order therefore holds no claimed bin and
// counts for nothing at the dropoff gate (orders.InFlightForDropoffSQL), so a
// second order to the same line is admitted while the first is parked — both
// re-compete when the fleet comes back.
//
// What stops them both going is not the count at demotion time; it is the gate
// re-run. fulfillment.Scanner.tryFulfill runs the dropoff gate before it routes a
// held-bin order to dispatchHeldBin, and dispatchHeldBin takes the hard claim at
// ConfirmForDispatch before the fleet call. So on the recovery pass the first
// order to re-dispatch claims its bin, and the second order's gate counts that
// claim and parks it under a dropoff cause, whose releaser is the first order's
// bin arriving and then leaving.
//
// Driven through the real engine, because the gate re-run lives in the
// fulfillment scanner and the dispatch package cannot host it.
//
// MUTATIONS, both run before this landed:
//   - make the count read status again (InFlightForDropoffSQL back to "not
//     terminal and not queued"). The demoted order counts, the second order is
//     refused at the gate while the fleet is down, and the "admitted while the
//     first is demoted" assertion fires.
//   - route a held-bin order to dispatchHeldBin before the gate in tryFulfill. On
//     recovery both demoted orders re-dispatch, and the "exactly one" assertion
//     fires.
func TestFleetRecovery_TwoDemotedOrdersToOneLineOnlyOneGoes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewTrackingBackend()
	backend.SetFail(true) // the fleet is refusing creates
	eng := newTestEngine(t, db, backend)

	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "FDR-BIN-A")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "FDR-BIN-B")

	submit := func(uuid string) {
		eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID: uuid, OrderType: dispatch.OrderTypeRetrieve, PayloadCode: sd.Payload.Code,
			SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name, Quantity: 1,
		})
		eng.RunFulfillmentScan()
	}

	// ── A: the fleet refuses it, and it is demoted ─────────────────────────
	submit("fdr-a")
	a := testdb.RequireOrder(t, db, "fdr-a")
	if a.VendorOrderID != "" || a.Status != protocol.StatusSourcing ||
		a.QueueCause != string(dispatch.CauseFleetRefusedCreate) || a.BinID == nil {
		t.Fatalf("fixture: order A is %q, cause %q, bin %v, vendor %q — want it demoted by the fleet "+
			"refusal: sourcing, %s, holding its bin pointer, with no vendor order",
			a.Status, a.QueueCause, a.BinID, a.VendorOrderID, dispatch.CauseFleetRefusedCreate)
	}
	assertHoldsNoClaim(t, eng, a, "order A after the fleet refused it")

	// ── B arrives while A is demoted, and is admitted ──────────────────────
	submit("fdr-b")
	b := testdb.RequireOrder(t, db, "fdr-b")
	if b.QueueCause != string(dispatch.CauseFleetRefusedCreate) || b.BinID == nil {
		t.Fatalf("order B is %q under cause %q with bin %v while order A is demoted — want B admitted "+
			"through the dropoff gate and refused by the fleet (%s). A demoted order holds no claimed bin, "+
			"so it counts for nothing at the gate; a status-based count would park B behind it",
			b.Status, b.QueueCause, b.BinID, dispatch.CauseFleetRefusedCreate)
	}

	// ── The fleet recovers ─────────────────────────────────────────────────
	backend.SetFail(false)
	eng.RunFulfillmentScan()
	eng.RunFulfillmentScan() // a second pass must change nothing

	a, b = testdb.RequireOrder(t, db, "fdr-a"), testdb.RequireOrder(t, db, "fdr-b")
	var winner, loser *orders.Order
	switch {
	case a.VendorOrderID != "" && b.VendorOrderID == "":
		winner, loser = a, b
	case b.VendorOrderID != "" && a.VendorOrderID == "":
		winner, loser = b, a
	default:
		t.Fatalf("after the fleet recovered: A vendor %q (%s/%s), B vendor %q (%s/%s) — want exactly one "+
			"dispatch to %s. The dropoff gate re-runs before every held-bin re-dispatch, and the first "+
			"order to re-dispatch holds a claimed bin bound there before the second is asked",
			a.VendorOrderID, a.Status, a.QueueCause, b.VendorOrderID, b.Status, b.QueueCause, sd.LineNode.Name)
	}
	if n := len(backend.CreateRequests()); n != 1 {
		t.Errorf("the fleet was asked to create %d order(s) after it recovered, want 1", n)
	}
	if n, err := db.CountInFlightOrdersByDeliveryNodeExcluding(sd.LineNode.Name, 0); err != nil || n != 1 {
		t.Errorf("orders holding a claimed bin bound for %s = %d (err %v), want exactly 1", sd.LineNode.Name, n, err)
	}

	// The loser waits, at the gate, under a dropoff cause with a releaser.
	if !protocol.IsAcquiring(loser.Status) {
		t.Fatalf("the order that did not go is %q, want it still acquiring", loser.Status)
	}
	switch dispatch.QueueCause(loser.QueueCause) {
	case dispatch.CauseDropoffInflight, dispatch.CauseDropoffOccupied, dispatch.CauseDropoffCapacity:
	default:
		t.Fatalf("the order that did not go parked under %q, want a dropoff-family cause naming the order "+
			"already inbound to %s", loser.QueueCause, sd.LineNode.Name)
	}
	declared := false
	for _, c := range dispatch.DeclaredQueueCauses() {
		declared = declared || c == loser.QueueCause
	}
	if !declared {
		t.Errorf("cause %q has no releaser row — a wait nothing is declared to end", loser.QueueCause)
	}
	assertHoldsNoClaim(t, eng, loser, "the order parked behind the winner")

	// ── And the releaser fires: the winner's bin lands and then leaves ─────
	winnerBin := *winner.BinID
	testutil.MustNoErr(t, db.MoveBinClearingStaging(winnerBin, sd.LineNode.ID, false), "the winner's bin lands")
	_, err := db.TerminalizeOrder(winner.ID, protocol.StatusConfirmed, "delivered")
	testutil.MustNoErr(t, err, "the winner completes")
	eng.RunFulfillmentScan()
	if still := testdb.RequireOrder(t, db, loser.EdgeUUID); still.VendorOrderID != "" {
		t.Fatalf("the parked order went while the winner's bin still stands on %s", sd.LineNode.Name)
	}
	testutil.MustNoErr(t, db.MoveBinClearingStaging(winnerBin, sd.StorageNode.ID, false), "the line is cleared")
	eng.RunFulfillmentScan()
	if done := testdb.RequireOrder(t, db, loser.EdgeUUID); done.VendorOrderID == "" {
		t.Fatalf("the line cleared and the parked order still did not go (%s/%s) — a wait with no releaser",
			done.Status, done.QueueCause)
	}
}

// assertHoldsNoClaim fails when o hard-claims any bin — the armor a fleet
// refusal takes off and a parked order must not keep.
func assertHoldsNoClaim(t *testing.T, eng *Engine, o *orders.Order, when string) {
	t.Helper()
	claimed, err := eng.db.ListBinsByClaim(o.ID)
	testutil.MustNoErr(t, err, "list claimed bins")
	if len(claimed) != 0 {
		t.Errorf("%s: order %s hard-claims %d bin(s)", when, o.EdgeUUID, len(claimed))
	}
}
