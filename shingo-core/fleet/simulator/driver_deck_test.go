//go:build sim

package simulator

import (
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingocore/config"
	"shingocore/fleet"
)

// The deck model and the vehicle pin exist so the cancel-return path can be
// driven on the simulator: an order cancelled after its pickup leaves the bin
// on the robot, Core reads the jack, and orders the bin home on a return pinned
// to THAT robot. These pin each piece of that chain on the driver alone.

// deckCfg is a small finite fleet with flat timing: CREATED for 1.5s, then 5s
// per block, no faults — so the tests can step to a state rather than count
// ticks.
func deckCfg(fleetSize int) config.SimConfig {
	return config.SimConfig{TransitTime: 5 * time.Second, JitterPct: 0, FailRate: 0, FleetSize: fleetSize}
}

// stepUntil ticks the driver one simulated second at a time until cond holds,
// failing after max ticks.
func stepUntil(t *testing.T, d *Driver, m *clock.Manual, max int, what string, cond func() bool) {
	t.Helper()
	for range max {
		if cond() {
			return
		}
		m.Advance(time.Second)
		d.step(m.Now())
	}
	if !cond() {
		t.Fatalf("after %d ticks: %s never happened", max, what)
	}
}

// robotOf returns the fleet row for a robot.
func robotOf(t *testing.T, d *Driver, id string) FleetRobot {
	t.Helper()
	for _, r := range d.Fleet() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("robot %s not in fleet %+v", id, d.Fleet())
	return FleetRobot{}
}

// statusOf returns the vendor-shaped status row for a robot.
func statusOf(t *testing.T, s *SimulatorBackend, id string) fleet.RobotStatus {
	t.Helper()
	rows, err := s.GetRobotsStatus()
	if err != nil {
		t.Fatalf("GetRobotsStatus: %v", err)
	}
	for _, r := range rows {
		if r.VehicleID == id {
			return r
		}
	}
	t.Fatalf("robot %s not in status %+v", id, rows)
	return fleet.RobotStatus{}
}

// pinnedUnload creates the cancel-return's own order shape: pinned to one
// robot, a single JackUnload block, complete.
func pinnedUnload(t *testing.T, s *SimulatorBackend, ext, vehicle, at string) string {
	t.Helper()
	res, err := s.CreateOrder(fleet.CreateOrderRequest{
		ExternalID: ext,
		Vehicle:    vehicle,
		Blocks:     []fleet.OrderBlock{{BlockID: ext + "_unload", Location: at, BinTask: "JackUnload"}},
		Complete:   true,
	})
	if err != nil {
		t.Fatalf("CreateOrder(%s): %v", ext, err)
	}
	return res.VendorOrderID
}

// The plain lifecycle: the JackLoad block loads the deck and puts the robot at
// the pickup; the final JackUnload — reported as FINISHED, not CompleteBlock —
// empties it and puts the robot at the drop. Status follows: JackState 1 then 3.
func TestDeck_LoadsOnPickupAndEmptiesOnFinalUnload(t *testing.T) {
	d, s, m, em := newTestDriver(t, deckCfg(2), 1)
	s.driver = d

	vid := mkTransport(t, s, "o1") // JackLoad@A, JackUnload@B
	stepUntil(t, d, m, 30, "pickup block completed", func() bool { return len(em.blocks) == 1 })
	d.publishFleet()

	robot := d.progress[vid].robotID
	if r := robotOf(t, d, robot); !r.Loaded || r.At != "A" || !r.Busy {
		t.Fatalf("after the pickup %s should be busy, loaded, at A; got %+v", robot, r)
	}
	st := statusOf(t, s, robot)
	if st.JackState != 1 || !st.JackIsFull || !st.IsLoaded || st.LiftHeight <= 0.005 {
		t.Fatalf("a loaded deck must report jack_state 1 / full / loaded / raised; got %+v", st)
	}

	stepUntil(t, d, m, 30, "order finished", func() bool { return s.GetOrder(vid).State == "FINISHED" })
	d.publishFleet()
	if r := robotOf(t, d, robot); r.Loaded || r.At != "B" || r.Busy {
		t.Fatalf("after the final unload %s should be free, empty, at B; got %+v", robot, r)
	}
	st = statusOf(t, s, robot)
	if st.JackState != 3 || st.JackIsFull || st.IsLoaded || st.LiftHeight != 0 {
		t.Fatalf("an empty deck must report jack_state 3 and a lowered jack; got %+v", st)
	}
}

// THE INCIDENT SHAPE, end to end on the driver: a cancel after the pickup frees
// the robot but leaves the bin on its deck and the robot where it lifted it. A
// fresh unpinned order does not take that robot; the pinned return does, and
// its single unload empties the deck.
func TestDeck_CancelAfterPickupLeavesTheBinOnTheRobot(t *testing.T) {
	d, s, m, em := newTestDriver(t, deckCfg(2), 1)
	s.driver = d

	vid := mkTransport(t, s, "carry")
	stepUntil(t, d, m, 30, "pickup block completed", func() bool { return len(em.blocks) == 1 })
	robot := d.progress[vid].robotID

	if err := s.CancelOrder(vid); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	m.Advance(time.Second)
	d.step(m.Now())

	r := robotOf(t, d, robot)
	if r.Busy || !r.Loaded || r.At != "A" {
		t.Fatalf("after the cancel %s should be free, still loaded, still at A; got %+v", robot, r)
	}
	st := statusOf(t, s, robot)
	if !st.Available || st.Busy || st.JackState != 1 {
		t.Fatalf("Core must read a free robot with a bin up (the cancel-return trigger); got %+v", st)
	}

	// An unpinned order must not lift onto a full jack.
	other := mkTransport(t, s, "other")
	stepUntil(t, d, m, 30, "unpinned order took a robot", func() bool {
		p := d.progress[other]
		return p != nil && p.robotID != ""
	})
	if got := d.progress[other].robotID; got == robot {
		t.Fatalf("unpinned order was handed %s, whose deck still carries the cancelled bin", robot)
	}

	// The return: pinned to the carrying robot, one JackUnload.
	ret := pinnedUnload(t, s, "return", robot, "STORE")
	stepUntil(t, d, m, 30, "return finished", func() bool { return s.GetOrder(ret).State == "FINISHED" })
	if !contains(em.assigned, ret+":"+robot) {
		t.Fatalf("the return must run on %s; assignments %v", robot, em.assigned)
	}
	if r := robotOf(t, d, robot); r.Loaded || r.At != "STORE" {
		t.Fatalf("the return's unload should empty %s at STORE; got %+v", robot, r)
	}
}

// A pinned order waits for its robot while it is busy, even with other robots
// free, and then runs on it.
func TestDeck_PinnedOrderWaitsForItsRobot(t *testing.T) {
	d, s, m, em := newTestDriver(t, deckCfg(3), 1)
	s.driver = d

	first := mkTransport(t, s, "first")
	stepUntil(t, d, m, 30, "first order running", func() bool { return s.GetOrder(first).State == "RUNNING" })
	robot := d.progress[first].robotID

	pinned := pinnedUnload(t, s, "pinned", robot, "STORE")
	// Long enough that an unpinned order would have left CREATED on a free
	// robot, short of the first order finishing.
	runTicks(d, m, 3)
	if got := s.GetOrder(pinned).State; got != "CREATED" {
		t.Fatalf("pinned order must wait while %s is busy, even with free robots; state %s", robot, got)
	}
	if d.queuedCount != 1 {
		t.Fatalf("the waiting pinned order counts as queued on a finite fleet; queued=%d", d.queuedCount)
	}

	stepUntil(t, d, m, 60, "pinned order finished", func() bool { return s.GetOrder(pinned).State == "FINISHED" })
	if !contains(em.assigned, pinned+":"+robot) {
		t.Fatalf("pinned order must run on %s; assignments %v", robot, em.assigned)
	}
	if d.robotsInUse != 0 || d.queuedCount != 0 {
		t.Fatalf("accounting must settle: inUse=%d queued=%d", d.robotsInUse, d.queuedCount)
	}
}

// The infinite fleet honours a pin too: robot names are durable once minted,
// and a pin to a robot that does not exist waits rather than minting it.
func TestDeck_InfiniteFleetHonoursPin(t *testing.T) {
	d, s, m, em := newTestDriver(t, deckCfg(0), 1)
	s.driver = d

	vid := mkTransport(t, s, "o1")
	stepUntil(t, d, m, 30, "order finished", func() bool { return s.GetOrder(vid).State == "FINISHED" })
	robot := robotsAssigned(em)[0]

	ghost := pinnedUnload(t, s, "ghost", "AMR-99", "STORE")
	pinned := pinnedUnload(t, s, "pinned", robot, "STORE")
	stepUntil(t, d, m, 30, "pinned order finished", func() bool { return s.GetOrder(pinned).State == "FINISHED" })
	if !contains(em.assigned, pinned+":"+robot) {
		t.Fatalf("pinned order must run on %s; assignments %v", robot, em.assigned)
	}
	if got := s.GetOrder(ghost).State; got != "CREATED" {
		t.Fatalf("a pin to a robot not in the fleet must wait, not mint it; state %s", got)
	}
	if d.queuedCount != 0 {
		t.Fatalf("the infinite fleet keeps its queue metrics at zero; queued=%d", d.queuedCount)
	}
}

// cancel-fails: the refused cancel errors, the order keeps RUNNING and its
// robot stays busy. Turning it off restores the ordinary cancel.
func TestDeck_CancelFailsKeepsTheOrderRunning(t *testing.T) {
	d, s, m, _ := newTestDriver(t, deckCfg(2), 1)
	s.driver = d

	vid := mkTransport(t, s, "o1")
	stepUntil(t, d, m, 30, "order running", func() bool { return s.GetOrder(vid).State == "RUNNING" })
	robot := d.progress[vid].robotID

	if err := s.SetCancelFails(true); err != nil {
		t.Fatalf("SetCancelFails: %v", err)
	}
	if err := s.CancelOrder(vid); err == nil {
		t.Fatal("CancelOrder must refuse while cancel-fails is on")
	}
	m.Advance(time.Second)
	d.step(m.Now())
	if got := s.GetOrder(vid).State; got != "RUNNING" {
		t.Fatalf("a refused cancel leaves the order RUNNING; got %s", got)
	}
	if st := statusOf(t, s, robot); !st.Busy || st.Available {
		t.Fatalf("a refused cancel leaves %s busy; got %+v", robot, st)
	}

	if err := s.SetCancelFails(false); err != nil {
		t.Fatalf("SetCancelFails: %v", err)
	}
	if err := s.CancelOrder(vid); err != nil {
		t.Fatalf("CancelOrder after the toggle is off: %v", err)
	}
	if got := s.GetOrder(vid).State; got != "STOPPED" {
		t.Fatalf("want STOPPED, got %s", got)
	}
}

// A faulted robot reports IsError and takes no new order, pinned or not; a
// forced deck reads back as forced. Unknown robots are refused, not invented.
func TestDeck_FaultAndForcedDeck(t *testing.T) {
	d, s, m, em := newTestDriver(t, deckCfg(2), 1)
	s.driver = d

	if err := s.SetRobotFault("AMR-01", true); err != nil {
		t.Fatalf("SetRobotFault: %v", err)
	}
	if err := s.SetRobotFault("AMR-77", true); err == nil {
		t.Fatal("faulting a robot not in the fleet must be refused")
	}
	pinned := pinnedUnload(t, s, "pinned", "AMR-01", "STORE")
	free := mkTransport(t, s, "free")
	stepUntil(t, d, m, 30, "unpinned order running", func() bool { return s.GetOrder(free).State == "RUNNING" })
	if got := d.progress[free].robotID; got != "AMR-02" {
		t.Fatalf("unpinned order must skip the faulted AMR-01; got %s", got)
	}
	if got := s.GetOrder(pinned).State; got != "CREATED" {
		t.Fatalf("an order pinned to a faulted robot waits; state %s", got)
	}
	if st := statusOf(t, s, "AMR-01"); !st.IsError {
		t.Fatalf("a faulted robot reports IsError; got %+v", st)
	}

	if err := s.SetRobotFault("AMR-01", false); err != nil {
		t.Fatalf("SetRobotFault: %v", err)
	}
	stepUntil(t, d, m, 30, "pinned order finished", func() bool { return s.GetOrder(pinned).State == "FINISHED" })
	if !contains(em.assigned, pinned+":AMR-01") {
		t.Fatalf("pinned order must run on AMR-01 once the fault clears; %v", em.assigned)
	}

	if err := s.SetRobotDeck("AMR-02", true); err != nil {
		t.Fatalf("SetRobotDeck: %v", err)
	}
	if r := robotOf(t, d, "AMR-02"); !r.Loaded {
		t.Fatalf("a forced deck reads back loaded at once, not a tick later; got %+v", r)
	}
}
