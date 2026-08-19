//go:build docker

package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// boot_mission_census_docker_test.go — §R.98 stage A4.
//
// Re-registering an order into the tracker is Core saying "I am still commanding
// this mission". Nothing ever checked the other half of that sentence. Against
// the in-process simulator a Core restart empties the fleet, and every mission
// reloaded at boot is one Core will drive, append to and wait on forever against
// nobody — which is what a whole measured window turned out to be. The tracker's
// own line said "loaded 3 active vendor orders into tracker" and was true.

type capturingLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *capturingLog) logf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *capturingLog) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func bootEngine(t *testing.T, db *store.DB, flt fleet.Backend, lg *capturingLog) *Engine {
	t.Helper()
	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-core"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"
	eng := New(Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     flt,
		MsgClient: nil,
		LogFunc:   lg.logf,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })
	return eng
}

// A mission the backend does hold is loaded quietly — the census must be silent
// at zero, or nobody will read it when it is not.
func TestBoot_MissionTheFleetHoldsLoadsQuietly(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	sim := simulator.New()

	res, err := sim.CreateOrder(fleet.CreateOrderRequest{
		ExternalID: "held",
		Blocks:     []fleet.OrderBlock{{BlockID: "b1", Location: "A", BinTask: "JackLoad"}},
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	held := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusInTransit
	})
	if err := db.UpdateOrderVendor(held.ID, res.VendorOrderID, "RUNNING", "AMR-01"); err != nil {
		t.Fatalf("UpdateOrderVendor: %v", err)
	}

	lg := &capturingLog{}
	bootEngine(t, db, sim, lg)

	out := lg.joined()
	if !strings.Contains(out, "loaded 1 active vendor orders into tracker") {
		t.Fatalf("the ordinary load line must still appear; got:\n%s", out)
	}
	if strings.Contains(out, "does not hold it") {
		t.Fatalf("a mission the fleet holds must not be reported missing; got:\n%s", out)
	}
}

// And the restart shape: Core's row says the mission is live, the fleet has never
// heard of it. That has to be said out loud at boot, and it has to say so without
// touching the order — nothing terminates on a mission Core cannot see (§R.98,
// refused 4/4).
func TestBoot_MissionTheFleetLostScreams(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	sim := simulator.New() // the empty fleet a restarted Core wakes up beside

	ord := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.Status = protocol.StatusInTransit
	})
	if err := db.UpdateOrderVendor(ord.ID, "sg-77-forgotten", "RUNNING", "AMR-01"); err != nil {
		t.Fatalf("UpdateOrderVendor: %v", err)
	}

	lg := &capturingLog{}
	bootEngine(t, db, sim, lg)

	out := lg.joined()
	if !strings.Contains(out, "sg-77-forgotten") || !strings.Contains(out, "does not hold it") {
		t.Fatalf("boot must name the mission Core is commanding alone; got:\n%s", out)
	}
	if !strings.Contains(out, "1 mission(s) are Core's alone") {
		t.Fatalf("boot must count the missions the fleet lost; got:\n%s", out)
	}

	// Reported, not acted on.
	fresh, err := db.GetOrder(ord.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if fresh.Status != protocol.StatusInTransit {
		t.Fatalf("the census must not move the order; status = %s", fresh.Status)
	}
}
