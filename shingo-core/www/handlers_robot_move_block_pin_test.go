//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// TestRobotMove_ItsBlockNamesNoBinTaskAndReachesNoSubscriber pins the Robots
// page's send-to as one of the three orderless doors: its one block names the
// node and no binTask, and with no order row behind its vendor order the
// fleet's FINISHED for that block reaches no block subscriber.
func TestRobotMove_ItsBlockNamesNoBinTaskAndReachesNoSubscriber(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	resp, status := moveRobotTo(t, h, sd.LineNode.Name)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; err=%q", status, resp.Error)
	}
	blocks := sim.BlocksForOrder(resp.VendorOrderID)
	if len(blocks) != 1 {
		t.Fatalf("the move sent %d blocks, want one", len(blocks))
	}
	b := blocks[0]
	if b.BlockID != resp.VendorOrderID+"-b1" || b.Location != sd.LineNode.Name || b.BinTask != "" {
		t.Errorf("the move's block is %+v, want %s-b1 at %s with no binTask", b, resp.VendorOrderID, sd.LineNode.Name)
	}
	if sim.CompleteBlock(resp.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0) {
		t.Error("the move's FINISHED block reached a subscriber; an orderless door has no order to read it against")
	}
}
