//go:build docker

package engine

import (
	"sort"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// rearmingFleet is the simulator plus fleet.ReleasedOrderRearmer, recording
// which orders boot re-armed.
type rearmingFleet struct {
	*simulator.SimulatorBackend
	mu     sync.Mutex
	rearms []string
}

func (f *rearmingFleet) RearmReleased(vendorOrderID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rearms = append(f.rearms, vendorOrderID)
	return nil
}

// TestBoot_RearmsReleasedOpenOrders: order 112 across a restart (S3). Boot
// re-arms only the open orders the station released past a wait — in transit
// with a wait behind them — so the tracker does not read the robot still at
// the old wait as parked at the next one. A leg driving to its first wait, and
// a staged one, are tracked as before.
func TestBoot_RearmsReleasedOpenOrders(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	for _, c := range []struct {
		vid       string
		status    protocol.Status
		waitIndex int
	}{
		{"V-RELEASED", protocol.StatusInTransit, 1},
		{"V-FIRST-WAIT", protocol.StatusInTransit, 0},
		{"V-STAGED", protocol.StatusStaged, 1},
	} {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.StationID, o.Status = "u-"+c.vid, "line-1", c.status
		})
		testutil.MustNoErr(t, db.UpdateOrderVendor(o.ID, c.vid, "WAITING", ""), "vendor")
		if c.waitIndex != 0 {
			testutil.MustNoErr(t, db.UpdateOrderWaitIndex(o.ID, c.waitIndex), "wait_index")
		}
	}
	flt := &rearmingFleet{SimulatorBackend: simulator.New()}
	newTestEngine(t, db, flt)
	flt.mu.Lock()
	got := append([]string(nil), flt.rearms...)
	flt.mu.Unlock()
	sort.Strings(got)
	if strings.Join(got, ",") != "V-RELEASED" {
		t.Errorf("boot re-armed %v, want exactly [V-RELEASED]", got)
	}
}
