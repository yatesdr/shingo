//go:build docker

package www

import (
	"encoding/json"
	"net/http"
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

	// The row that must not exist. core-operator is where the manual order
	// screen's rows land, so anything here is the old behaviour coming back.
	rows, err := db.ListOrdersByStation("core-operator", 50)
	if err != nil {
		t.Fatalf("list operator orders: %v", err)
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

// A BIN AT THE DESTINATION IS NOT AN OBSTACLE TO A MOVE.
//
// This test used to assert the opposite, and its own rationale is what gives it
// away: it kept "the occupancy pre-flight that shipped with the old door" on the
// grounds that otherwise "a robot would drive to a spot that already holds a
// bin, arrive, and stop, and the operator would find out by watching."
//
// Arriving and stopping is what being sent somewhere MEANS. This endpoint picks
// up nothing, places nothing and writes no order row -- the form says so in as
// many words -- so a bin already at the node is scenery, not a conflict. The
// gate was a dropoff-capacity check inherited from the order door, asking
// whether a BIN COULD BE PLACED there, applied to a command that places none.
//
// It also refused the case the endpoint is most useful for: sending a robot to
// a node precisely BECAUSE something is there and somebody wants eyes on it.
func TestRobotMove_AnOccupiedDestinationIsNotAnObstacle(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-MOVEROBOT-OCCUPIED")

	resp, status := moveRobotTo(t, h, sd.LineNode.Name)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: a bin at the destination cannot refuse a "+
			"command that places no bin. body=%+v", status, resp)
	}
	if resp.Error != "" {
		t.Errorf("carried an error %q on a successful move", resp.Error)
	}
	if resp.Destination != sd.LineNode.Name {
		t.Errorf("destination = %q, want %q", resp.Destination, sd.LineNode.Name)
	}
	// The robot really was dispatched -- the point is that it goes, not merely
	// that the handler stopped objecting.
	if sim.OrderCount() != 1 {
		t.Errorf("fleet order count = %d, want 1 — the move was accepted but no "+
			"robot was sent", sim.OrderCount())
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
