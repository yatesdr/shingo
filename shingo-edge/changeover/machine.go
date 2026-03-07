package changeover

import (
	"fmt"
	"log"
	"sync"

	"shingoedge/store"
)

// Machine manages the changeover state machine for a production line.
type Machine struct {
	mu           sync.Mutex
	db           *store.DB
	emitter      EventEmitter
	lineID       int64
	lineName     string
	fromJobStyle string
	toJobStyle   string
	state        string
	active       bool

	DebugLog func(string, ...any)
}

func (m *Machine) dbg(format string, args ...any) {
	if fn := m.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// NewMachine creates a changeover state machine for a specific production line.
func NewMachine(db *store.DB, emitter EventEmitter, lineID int64, lineName string) *Machine {
	return &Machine{
		db:       db,
		emitter:  emitter,
		lineID:   lineID,
		lineName: lineName,
		state:    StateRunning,
	}
}

// Restore rebuilds the machine state from the last changeover log entry.
// If the last logged state is not "running", the changeover is resumed.
func (m *Machine) Restore() {
	entry, err := m.db.GetLatestChangeoverState(m.lineID)
	if err != nil || entry == nil {
		return // no history, start fresh
	}
	if entry.State == StateRunning {
		return // last changeover completed normally
	}
	m.mu.Lock()
	m.fromJobStyle = entry.FromJobStyle
	m.toJobStyle = entry.ToJobStyle
	m.state = entry.State
	m.active = true
	m.mu.Unlock()
	m.dbg("restore: line=%d state=%s %s->%s", m.lineID, entry.State, entry.FromJobStyle, entry.ToJobStyle)
	log.Printf("changeover: restored in-progress changeover for line %d (state=%s, %s→%s)", m.lineID, entry.State, entry.FromJobStyle, entry.ToJobStyle)
}

// LineID returns the production line ID for this machine.
func (m *Machine) LineID() int64 {
	return m.lineID
}

// LineName returns the production line name for this machine.
func (m *Machine) LineName() string {
	return m.lineName
}

// IsActive returns whether a changeover is in progress.
func (m *Machine) IsActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// CurrentState returns the current changeover state.
func (m *Machine) CurrentState() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Info returns the current changeover info.
func (m *Machine) Info() (fromJobStyle, toJobStyle, state string, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fromJobStyle, m.toJobStyle, m.state, m.active
}

// Start begins a changeover from one job style to another.
func (m *Machine) Start(fromJobStyle, toJobStyle, operator string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.active {
		return fmt.Errorf("changeover already in progress from %s to %s", m.fromJobStyle, m.toJobStyle)
	}

	m.fromJobStyle = fromJobStyle
	m.toJobStyle = toJobStyle
	m.state = StateStopping
	m.active = true

	m.dbg("start: line=%d %s->%s", m.lineID, fromJobStyle, toJobStyle)
	m.logTransition(StateRunning, StateStopping, "changeover initiated", operator)
	m.emitter.EmitChangeoverStarted(m.lineID, fromJobStyle, toJobStyle)
	m.emitter.EmitChangeoverStateChanged(m.lineID, fromJobStyle, toJobStyle, StateRunning, StateStopping)

	return nil
}

// Advance moves to the next state in the changeover sequence.
func (m *Machine) Advance(operator string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.active {
		return fmt.Errorf("no changeover in progress")
	}

	next, ok := NextState(m.state)
	if !ok {
		return fmt.Errorf("no next state from %s", m.state)
	}

	oldState := m.state
	m.state = next
	m.dbg("advance: line=%d %s->%s", m.lineID, oldState, next)

	m.logTransition(oldState, next, "", operator)
	m.emitter.EmitChangeoverStateChanged(m.lineID, m.fromJobStyle, m.toJobStyle, oldState, next)

	// If we've returned to Running, the changeover is complete
	if next == StateRunning {
		m.active = false
		m.emitter.EmitChangeoverCompleted(m.lineID, m.fromJobStyle, m.toJobStyle)
	}

	return nil
}

// Cancel aborts the current changeover and resets to running.
func (m *Machine) Cancel(operator string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.active {
		return fmt.Errorf("no changeover in progress")
	}

	oldState := m.state
	from, to := m.fromJobStyle, m.toJobStyle
	m.dbg("cancel: line=%d from_state=%s", m.lineID, oldState)
	m.state = StateRunning
	m.active = false
	m.fromJobStyle = ""
	m.toJobStyle = ""

	m.logTransition(oldState, StateRunning, "changeover cancelled", operator)
	m.emitter.EmitChangeoverStateChanged(m.lineID, from, to, oldState, StateRunning)
	m.emitter.EmitChangeoverCancelled(m.lineID, from, to, operator)

	return nil
}

func (m *Machine) logTransition(oldState, newState, detail, operator string) {
	if _, err := m.db.InsertChangeoverLog(m.fromJobStyle, m.toJobStyle, newState, detail, operator, m.lineID); err != nil {
		log.Printf("insert changeover log: %v", err)
	}
}
