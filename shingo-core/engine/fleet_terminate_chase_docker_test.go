//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// chaseFleet is the fleet as the chase asks it: one live state per vendor order.
type chaseFleet struct{ state map[string]string }

func (f *chaseFleet) GetVendorOrderDetail(id string) (*fleet.VendorOrderDetail, error) {
	s := f.state[id]
	return &fleet.VendorOrderDetail{State: s, IsTerminal: s == "STOPPED" || s == "FINISHED"}, nil
}

func (f *chaseFleet) ExecuteVendorCommand(fleet.VendorCommand) (*fleet.VendorCommandResult, error) {
	return nil, nil
}

// TestTerminateChase pins the noticing of an order shingo ended that the fleet
// still runs (a terminate it refused): three re-sends on the order's audit
// trail, then one row saying a person has to stop it, then nothing more — and
// once the fleet ends it, the row's fleet state is recorded so it drops out.
//
// MUTATION: make chaseUnterminatedOrders skip the CancelOrder call, or drop the
// d.IsTerminal arm, and the counts / the final vendor_state assertion fail.
func TestTerminateChase(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	o := &orders.Order{
		EdgeUUID: "chase-1", StationID: "line-1", OrderType: "retrieve",
		Status: protocol.StatusInTransit, Quantity: 1, PayloadCode: sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")
	testutil.MustNoErr(t, db.UpdateOrderVendor(o.ID, "sg-chase-1", "RUNNING", "AMR-06"), "vendor state")
	testdb.SeedOrderStatus(t, db, o.ID, string(protocol.StatusCancelled), "cancelled by admin")
	// Ended a minute ago: past the settle, inside the window.
	if _, err := db.DB.Exec(`UPDATE orders SET updated_at = $1 WHERE id = $2`, time.Now().UTC().Add(-time.Minute), o.ID); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	fl := &chaseFleet{state: map[string]string{"sg-chase-1": "RUNNING"}}
	attempts := map[int64]int{}
	rowsOf := func(action string) []string {
		rows, err := db.ListEntityAudit("order", o.ID)
		testutil.MustNoErr(t, err, "list audit")
		var out []string
		for _, r := range rows {
			if r.Action == action {
				out = append(out, r.NewValue)
			}
		}
		return out
	}
	count := func(action string) int { return len(rowsOf(action)) }

	for i := 0; i < 5; i++ {
		eng.chaseUnterminatedOrders(fl, attempts)
	}
	if got := count("fleet_terminate_resent"); got != terminateChaseAttempts {
		t.Errorf("re-sends = %d, want %d — bounded, not every pass forever", got, terminateChaseAttempts)
	}
	// The terminate was really sent: the simulator holds no such order and
	// refuses it, and the refusal is on the row beside the count. (Audit rows
	// list newest first, so the first send is the last row.)
	if sent := rowsOf("fleet_terminate_resent"); len(sent) == 0 ||
		sent[len(sent)-1] != "RDS still RUNNING (AMR-06); terminate re-sent 1/3: simulator: order sg-chase-1 not found" {
		t.Errorf("first re-send row = %q, want the count and the fleet's refusal", sent)
	}
	if got := count("fleet_still_running"); got != 1 {
		t.Errorf("still-running alerts = %d, want exactly 1", got)
	}

	fl.state["sg-chase-1"] = "STOPPED"
	eng.chaseUnterminatedOrders(fl, attempts)
	got, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload")
	if got.VendorState != "STOPPED" {
		t.Errorf("vendor_state = %q, want STOPPED once the fleet ended it (the row drops out)", got.VendorState)
	}
	if len(attempts) != 0 {
		t.Errorf("attempts kept for an order the fleet ended: %v", attempts)
	}
}
