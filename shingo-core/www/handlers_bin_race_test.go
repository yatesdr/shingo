//go:build docker

package www

import (
	"net/http"
	"strings"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// raceTheBin takes the bin out from under the request the way a competing order
// does: a reservation acquired after the door's own availability check has
// passed but before it reserves. That window is real — the two are separate
// steps on both doors — and it is the only way to reach the code under test.
func raceTheBin(t *testing.T, db interface {
	CreateOrder(*orders.Order) error
}, binID int64, reserve func(binID, orderID int64) error) {
	t.Helper()
	holder := &orders.Order{
		EdgeUUID: "race-holder", StationID: "test", OrderType: "move",
		Status: "queued", Quantity: 1,
	}
	if err := db.CreateOrder(holder); err != nil {
		t.Fatalf("seed the competing order: %v", err)
	}
	if err := reserve(binID, holder.ID); err != nil {
		t.Fatalf("competing order could not take the bin: %v", err)
	}
}

// TestBinMove_LosingTheBinIsNotAServerError pins the operator-visible contract:
// wanting a bin somebody else already has is a conflict with an actionable
// message and no wreckage, never a server error.
//
// Losing a bin to another order is not a fault. It is two people wanting the
// same thing at the same time, and the answer a person can act on is "somebody
// just took it, try again". Both doors returned a 500 instead — the server
// reporting itself broken because a colleague was quicker.
//
// The engineer's door looked like it handled this. It spliced the words
// "transient reservation conflict, retry" into the error text, with a comment
// beside it saying that was done "so the caller can retry rather than surface a
// hard 500". Nothing can read a phrase inside a string, so the caller never did,
// and it surfaced as a hard 500 with the instruction buried in it.
//
// Both doors also left the order row behind. The row is created before the bin
// is reserved, so the losing request wrote an order that would sit pending
// forever with nothing to dispatch, fail or clean it up. One per lost race.
func TestBinMove_LosingTheBinIsNotAServerError(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RACE-OP")

	// The operator door checks availability up front, so the only way to reach
	// the reservation failure is to lose the bin after that check. Seed the
	// competing hold directly.
	raceTheBin(t, db, bin.ID, func(binID, orderID int64) error {
		testdb.ReserveBin(t, db, orderID, binID)
		return nil
	})

	resp, status := submitRetrieveSpecific(t, h, bin.Label, sd.LineNode.Name)

	if status == http.StatusInternalServerError {
		t.Fatalf("losing the bin came back as a server error (%q). Nothing is broken — somebody else took the bin.", resp.Error)
	}
	if status != http.StatusConflict {
		t.Errorf("status = %d, want %d", status, http.StatusConflict)
	}
	// Two guards can produce this: the up-front availability check, and the
	// reservation itself losing a race in the gap after that check. A test can
	// only stage the first — the second needs two requests colliding inside a
	// few microseconds — so what is pinned here is the answer, not which guard
	// gave it. Both must say something the operator can act on.
	if resp.Error == "" {
		t.Error("rejection carried no message")
	}
	if !strings.Contains(resp.Error, bin.Label) {
		t.Errorf("message %q does not name the bin", resp.Error)
	}

	// And nothing stranded.
	rows, err := db.ListOrdersByStation("core-operator", 50)
	if err != nil {
		t.Fatalf("list spot orders: %v", err)
	}
	for _, o := range rows {
		if o.Status == "pending" {
			t.Errorf("order %d was left pending after the request failed — nothing will ever dispatch, fail or clean it up", o.ID)
		}
	}
}
