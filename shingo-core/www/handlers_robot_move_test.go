//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

type robotMoveResponse struct {
	VendorOrderID string `json:"vendor_order_id"`
	Destination   string `json:"destination"`
	Error         string `json:"error"`
}

func moveRobotTo(t *testing.T, h *Handlers, node string) (*robotMoveResponse, int) {
	t.Helper()
	rec := postJSON(t, h.apiRobotMoveTo, "/api/robots/move",
		map[string]any{"delivery_node": node, "priority": 1})
	var resp robotMoveResponse
	if rec.Body.Len() > 0 {
		_ = json.NewDecoder(rec.Body).Decode(&resp)
	}
	return &resp, rec.Code
}

// TestRobotMove_MakesNoOrder is the whole point of the change: sending a robot
// somewhere is a fleet command, so it must leave nothing behind in the orders
// table.
//
// It used to write a row. That row was stamped dispatched and then never
// closed — the release path it was documented to wait for needs an owning
// station, and no edge owns Core's own. So every one of them sat open until
// the stuck-order sweep auto-cancelled it and filed a recovery action, which
// is why the plants' only historical row is a cancelled one. A row nothing
// reads, that cannot be closed, and that fakes an entry in the recovery audit
// is worse than no row.
func TestRobotMove_MakesNoOrder(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	resp, status := moveRobotTo(t, h, sd.LineNode.Name)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; err=%q", status, resp.Error)
	}
	if resp.Destination != sd.LineNode.Name {
		t.Errorf("destination = %q, want %q", resp.Destination, sd.LineNode.Name)
	}
	if resp.VendorOrderID == "" {
		t.Error("no vendor order id came back — it is the only handle this command has")
	}

	// The row that must not exist. core-spot is the station the deleted door
	// stamped, so anything here is the old behaviour coming back.
	rows, err := db.ListOrdersByStation("core-spot", 50)
	if err != nil {
		t.Fatalf("list spot orders: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d order row(s) written for a robot move — moving a robot is not an order", len(rows))
	}

	// And the fleet really was told to go, so "no order" did not become
	// "nothing happened".
	if sim.OrderCount() != 1 {
		t.Fatalf("fleet order count = %d, want 1", sim.OrderCount())
	}
	ov := sim.GetOrder(resp.VendorOrderID)
	if ov == nil {
		t.Fatalf("the fleet has no order under %q", resp.VendorOrderID)
	}
	if !ov.Complete {
		t.Error("fleet order was created incomplete. Complete=false means " +
			"'more blocks are coming via ReleaseOrder', and none ever are — it " +
			"leaves an order open at the vendor forever.")
	}
}

// TestRobotMove_RefusesAnOccupiedDestination keeps the occupancy pre-flight
// that shipped with the old door.
//
// Nothing downstream would catch this. The block carries no binTask, so the
// position gate waves it through — a robot would drive to a spot that already
// holds a bin, arrive, and stop, and the operator would find out by watching.
func TestRobotMove_RefusesAnOccupiedDestination(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-MOVEROBOT-OCCUPIED")

	resp, status := moveRobotTo(t, h, sd.LineNode.Name)

	if status == http.StatusOK {
		t.Fatalf("sending a robot to an occupied node returned 200 — it will arrive and stop. body=%+v", resp)
	}
	if status != http.StatusConflict {
		t.Errorf("status = %d, want %d: the spot being taken is a conflict with plant state, not a malformed request", status, http.StatusConflict)
	}
	if resp.Error == "" {
		t.Error("rejection carried no message")
	}
	if !strings.Contains(resp.Error, sd.LineNode.Name) {
		t.Errorf("message %q does not name the node the operator has to clear", resp.Error)
	}

	// Refused before the fleet call, so no robot was moved.
	if sim.OrderCount() != 0 {
		t.Errorf("fleet order count = %d, want 0 — the refusal came too late", sim.OrderCount())
	}
}

// TestRobotMove_RequiresAKnownNode pins the two input refusals.
func TestRobotMove_RequiresAKnownNode(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	testdb.SetupStandardData(t, db)

	if _, status := moveRobotTo(t, h, ""); status != http.StatusBadRequest {
		t.Errorf("empty destination: status = %d, want %d", status, http.StatusBadRequest)
	}
	if _, status := moveRobotTo(t, h, "NO-SUCH-NODE"); status != http.StatusBadRequest {
		t.Errorf("unknown destination: status = %d, want %d", status, http.StatusBadRequest)
	}
	if sim.OrderCount() != 0 {
		t.Errorf("fleet order count = %d, want 0", sim.OrderCount())
	}
}
