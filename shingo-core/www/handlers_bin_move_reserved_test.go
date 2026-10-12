//go:build docker

package www

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// claimForRobot holds a bin the way a dispatched order does: claimed_by, and a
// confirmed reservation behind it.
func claimForRobot(t *testing.T, db *store.DB, orderID, binID int64) {
	t.Helper()
	if _, err := db.DB.Exec(`INSERT INTO reservations (order_id, resource_kind, bin_id, state, reserved_by)
		VALUES ($1, 'bin', $2, 'confirmed', 'test')`, orderID, binID); err != nil {
		t.Fatalf("confirm the holder's reservation: %v", err)
	}
	if _, err := db.DB.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, orderID, binID); err != nil {
		t.Fatalf("claim the bin for the holder: %v", err)
	}
}

// A bin is held in two stages: a soft reservation taken at planning, and a hard
// claim taken immediately before dispatch. The person is authoritative over the
// first and never over the second (the orc's Q1 ruling, 2026-09-26; v4 §2.7): a
// person's move TAKES a bin another order has only promised itself, explicitly
// and with the trail, and REFUSES one a robot's order has claimed.
//
// Both used to be a 409. The soft half was a refusal on the argument that
// somebody else had it; under Q1 it is a take, because the holder can always
// re-find and a person standing at the bin cannot.

// TestBinMove_TakesABinAnotherOrderOnlyReserved: the operator's move goes, the
// holder's promise is gone, and the take is on the holder's history naming the
// move. The door then runs a scan pass (engine/bin_move.go), which re-asks the
// holder at once: with no other bin of its part here it waits, alive, under the
// finder's own sentence, not the take's.
//
// Converted (post-review C3): this asserted the holder still waiting with the
// bin-taken cause after the move, which pinned that nothing re-asked it until
// the 60 s floor. The holder now carries a part, as a real one does; the
// fixture's part-less order failed when re-asked.
func TestBinMove_TakesABinAnotherOrderOnlyReserved(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RESERVED")

	holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.PayloadCode = sd.Payload.Code
		o.DeliveryNode = sd.LineNode.Name
		o.SourceIntent = "full"
	})
	testdb.ReserveBin(t, db, holder.ID, bin.ID)
	if err := db.UpdateOrderBinID(holder.ID, bin.ID); err != nil {
		t.Fatalf("point the holder at its bin: %v", err)
	}

	resp, status := submitRetrieveSpecific(t, h, bin.Label, sd.LineNode.Name)
	if status != http.StatusOK || resp.Status != string(protocol.StatusDispatched) {
		t.Fatalf("status %d %+v, want 200 dispatched: a person's move takes a promised bin", status, resp)
	}

	after, err := db.GetOrder(holder.ID)
	if err != nil {
		t.Fatalf("reload holder: %v", err)
	}
	if after.BinID != nil && *after.BinID == bin.ID {
		t.Errorf("the holder still points at bin %d, which is leaving on the operator's move", bin.ID)
	}
	if protocol.IsTerminal(after.Status) {
		t.Errorf("holder is %s (%s): losing a promised bin is a recalculation, not a failure", after.Status, after.ErrorDetail)
	}
	var trail int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM order_history WHERE order_id=$1 AND code='bin-taken'
		AND (ref->>'taken_by')::bigint = $2`, holder.ID, resp.OrderID).Scan(&trail); err != nil || trail != 1 {
		t.Errorf("holder history has %d bin-taken row(s) naming order #%d (%v), want 1: the take is an act with a trail",
			trail, resp.OrderID, err)
	}
}

// TestBinMove_RefusesABinARobotHasClaimed: a hard claim is a robot's, and a
// person's move is refused with the holder named and no row written.
func TestBinMove_RefusesABinARobotHasClaimed(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-CLAIMED")

	// A robot's order: in transit, holding the bin hard with its confirmed
	// reservation behind the claim.
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusInTransit })
	claimForRobot(t, db, holder.ID, bin.ID)

	resp, status := submitRetrieveSpecific(t, h, bin.Label, sd.LineNode.Name)
	if status != http.StatusConflict {
		t.Fatalf("status = %d (%q), want 409: a robot's bin is never taken", status, resp.Error)
	}
	if !strings.Contains(resp.Error, fmt.Sprintf("#%d", holder.ID)) {
		t.Errorf("message %q does not name the holding order #%d", resp.Error, holder.ID)
	}
	rows, err := db.ListOrdersByStation("core-operator", 50)
	if err != nil {
		t.Fatalf("list core-operator orders: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d core-operator order row(s) written despite the refusal", len(rows))
	}
}
