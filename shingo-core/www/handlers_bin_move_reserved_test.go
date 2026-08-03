//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// TestBinMove_RefusesABinAnotherOrderHasReserved closes the gap between the two
// bin-move doors' availability checks.
//
// A bin is held in two stages: a soft reservation taken at planning, and a hard
// claim taken immediately before dispatch. That is deliberate — it is what lets
// two orders race for a bin without either one bricking it. But it means that
// for the whole window between the two, an in-flight order's bin has
// claimed_by still NULL.
//
// The operator door read only claimed_by. So a bin already spoken for looked
// free: the request sailed past the check, created its order row, and died at
// the reservation with a 500 saying "failed to reserve bin" — leaving that
// order row behind, in pending, with nothing to dispatch, fail or clean it up.
// The engineer's door has always checked both, and skips such a bin while
// scanning.
//
// The operator now gets the same 409 they get for an outright claimed bin,
// which is the honest answer: somebody else has it, try another.
func TestBinMove_RefusesABinAnotherOrderHasReserved(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RESERVED")

	// Another order holds a soft reservation on it — no hard claim yet, which is
	// the normal state of every in-flight order's bin.
	holder := testdb.CreateOrder(t, db)
	testdb.ReserveBin(t, db, holder.ID, bin.ID)

	before, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("reload bin: %v", err)
	}
	if before.ClaimedBy != nil {
		t.Fatal("fixture claimed the bin; this test is about the window BEFORE the claim, so it would prove nothing")
	}
	if !before.HasPendingReservation {
		t.Fatal("fixture did not leave a pending reservation; nothing to test")
	}

	resp, status := submitRetrieveSpecificTo(t, h, bin.Label, sd.LineNode.Name)

	if status == http.StatusInternalServerError {
		t.Fatalf("reserved bin produced a 500 (%q) — that is the reservation failing after the order row was already written, which is the bug: the row is orphaned in pending", resp.Error)
	}
	if status != http.StatusConflict {
		t.Errorf("status = %d, want %d — another order holds this bin, which is the same situation as an outright claim", status, http.StatusConflict)
	}
	if resp.Error == "" {
		t.Error("rejection carried no message")
	}

	// No order row left behind.
	rows, err := db.ListOrdersByStation("core-spot", 50)
	if err != nil {
		t.Fatalf("list core-spot orders: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d core-spot order row(s) written despite the rejection — they sit in pending forever", len(rows))
	}
}
