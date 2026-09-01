//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestBinMove_OperatorDoorRecordsItsOwnCreation pins that an operator's bin-move
// has a first entry in its own history.
//
// Order history is written by status TRANSITIONS, and the INSERT is not one — so
// an order created directly at pending had the right status and no entry saying
// it ever started, and its timeline began at whatever happened next. The
// engineer's door worked around that with a redundant pending→pending write
// whose only product was the row; the operator's door did not, so the one order
// class a person creates by hand was the class whose record did not say when it
// was created or by which door.
//
// orders.Create writes the birth row now, in the order's own transaction, so
// this holds structurally rather than per door. The assertion is unchanged and
// still the one that matters: the operator's door records its own creation.
func TestBinMove_OperatorDoorRecordsItsOwnCreation(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	free := &nodes.Node{Name: "LINE-FREE-FOR-HISTORY", Enabled: true}
	if err := db.CreateNode(free); err != nil {
		t.Fatalf("create free node: %v", err)
	}
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-HISTORY")

	resp, status := submitRetrieveSpecific(t, h, bin.Label, free.Name)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; err=%q", status, resp.Error)
	}

	history, err := db.ListOrderHistory(resp.OrderID)
	if err != nil {
		t.Fatalf("list order history: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("order has no history at all")
	}

	var sawPending bool
	for _, e := range history {
		if e.Status == "pending" {
			sawPending = true
		}
	}
	if !sawPending {
		var got []string
		for _, e := range history {
			got = append(got, string(e.Status))
		}
		t.Errorf("no pending entry in this order's history; it has %v.\n"+
			"The order was created at pending, so its own history should say so — otherwise its timeline starts at whatever happened next, and nothing records that a person made it.", got)
	}
}
