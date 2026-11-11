package simulator

import (
	"fmt"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol/lockcheck"
	"shingocore/fleet"
	"shingocore/internal/lockorder"
)

// table_turn.go — fleet.TableTurner for the simulator, in every build.
//
// A simulated robot's table has an angle, and a turn sent to it settles after
// a delay, reading task_status 2 (fleet.TaskStatusRunning) until it does, as a
// SEER robot reads while it runs a #3057 (measured 2026-11-10, about 8 s for
// 90°). Two failures are switchable: a turn the fleet refuses, and a turn the
// fleet takes while the table never moves. The table does not zero itself when
// the robot drives on, as a plant robot's does (AutoSpinToZeroInUnload): a sim
// turn is always absolute, so a turned table left behind changes no later
// answer.

// TableTurnCall is one TurnTable the simulator took, for a test to assert.
type TableTurnCall struct {
	Vehicle string
	Rad     float64
	At      time.Time
}

// simTable is the simulated tables. Behind its own lock, a leaf: it is read by
// the status readers and written by TurnTable, and calls nothing that locks.
type simTable struct {
	mu     lockcheck.Mutex[lockorder.SimTable]
	angle  map[string]float64   // the table's settled angle, per robot
	target map[string]float64   // the angle a turn in flight is going to
	doneAt map[string]time.Time // when that turn settles
	jackUp map[string]bool      // a robot whose jack is up (a set-down turn's precondition)
	calls  []TableTurnCall
	delay  time.Duration
	fails  bool // TurnTable refuses
	stuck  bool // TurnTable is taken and the table never moves
}

func (t *simTable) init() {
	t.angle = map[string]float64{}
	t.target = map[string]float64{}
	t.doneAt = map[string]time.Time{}
	t.jackUp = map[string]bool{}
}

var _ fleet.TableTurner = (*SimulatorBackend)(nil)

// OrderVehicle is the robot the simulator holds the order on: the one it was
// driven with (DriveStateWithRobot), else the one it was pinned to, else "".
func (s *SimulatorBackend) OrderVehicle(vendorOrderID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[vendorOrderID]
	if !ok {
		return "", fmt.Errorf("simulator: no order %s", vendorOrderID)
	}
	if o.assigned != "" {
		return o.assigned, nil
	}
	return o.vehicle, nil
}

// TurnTable turns a simulated robot's table to rad. It settles after the
// configured delay (SetTableTurnDelay); with none it is there at once.
func (s *SimulatorBackend) TurnTable(vehicleID string, rad float64) error {
	t := &s.table
	now := s.clk.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, TableTurnCall{Vehicle: vehicleID, Rad: rad, At: now})
	if t.fails {
		return fmt.Errorf("simulator: injected table turn failure for %s", vehicleID)
	}
	if t.stuck {
		return nil
	}
	t.settleLocked(vehicleID, now)
	t.target[vehicleID] = rad
	t.doneAt[vehicleID] = now.Add(t.delay)
	if t.delay <= 0 {
		t.angle[vehicleID] = rad
		delete(t.target, vehicleID)
		delete(t.doneAt, vehicleID)
	}
	return nil
}

// settleLocked lands a turn whose time has come. Caller holds t.mu.
func (t *simTable) settleLocked(vehicleID string, now time.Time) {
	if at, ok := t.doneAt[vehicleID]; ok && !now.Before(at) {
		t.angle[vehicleID] = t.target[vehicleID]
		delete(t.target, vehicleID)
		delete(t.doneAt, vehicleID)
	}
}

// readLocked is one robot's table: its angle, and whether a turn still runs.
func (t *simTable) readLocked(vehicleID string, now time.Time) (angle float64, turning bool) {
	t.settleLocked(vehicleID, now)
	_, turning = t.doneAt[vehicleID]
	return t.angle[vehicleID], turning
}

// TableStatus is one simulated robot as the table turn reads it: connected,
// idle unless a turn runs, its table angle, and its jack down (empty at rest)
// unless SetRobotJackUp raised it. A test hands this to the dispatcher as its
// robot status reader in a build where the simulator lists no fleet.
func (s *SimulatorBackend) TableStatus(vehicleID string) fleet.RobotStatus {
	t := &s.table
	t.mu.Lock()
	defer t.mu.Unlock()
	angle, turning := t.readLocked(vehicleID, s.clk.Now())
	r := fleet.RobotStatus{
		VehicleID:  vehicleID,
		Connected:  true,
		Available:  true,
		TaskStatus: 4,
		TableAngle: angle,
		JackState:  3,
	}
	if turning {
		r.TaskStatus = fleet.TaskStatusRunning
	}
	if t.jackUp[vehicleID] {
		r.JackState, r.JackIsFull, r.IsLoaded = 1, true, true
	}
	return r
}

// SetTableTurnDelay sets how long a simulated turn takes to settle.
func (s *SimulatorBackend) SetTableTurnDelay(d time.Duration) {
	s.table.mu.Lock()
	s.table.delay = d
	s.table.mu.Unlock()
}

// SetTableTurnFails makes TurnTable refuse while on.
func (s *SimulatorBackend) SetTableTurnFails(on bool) {
	s.table.mu.Lock()
	s.table.fails = on
	s.table.mu.Unlock()
}

// SetTableNeverTurns makes TurnTable take the command and the table never move
// while on: a robot that acks and does not turn.
func (s *SimulatorBackend) SetTableNeverTurns(on bool) {
	s.table.mu.Lock()
	s.table.stuck = on
	s.table.mu.Unlock()
}

// SetRobotJackUp raises or lowers a simulated robot's jack as TableStatus
// reports it.
func (s *SimulatorBackend) SetRobotJackUp(vehicleID string, up bool) {
	s.table.mu.Lock()
	s.table.jackUp[vehicleID] = up
	s.table.mu.Unlock()
}

// TableTurnCalls is every TurnTable the simulator took, in order.
func (s *SimulatorBackend) TableTurnCalls() []TableTurnCall {
	s.table.mu.Lock()
	defer s.table.mu.Unlock()
	return append([]TableTurnCall(nil), s.table.calls...)
}
