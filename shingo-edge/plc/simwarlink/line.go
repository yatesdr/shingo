package simwarlink

// This file carries no build tag: the line model is pure (no clock, no I/O, no
// lock), so a run with a clock of its own (a docker-tagged scenario) can drive
// the same line semantics the sim-tagged FakeClient ticks.

// Line is one process's line in the fake PLC. A load enters at the station
// (the station counter clicks) and leaves as a counted part depth loads later
// (the production counter ticks by per), so the counter trails the station by
// exactly depth clicks. A program change stops loading and lands once the line
// has drained; a forced one lands at once and leaves the in-flight loads to
// count out under the new identity. A pause stops loading until it is
// resumed; the line counts out meanwhile.
//
// A Line is not safe for concurrent use; FakeClient guards each one with its
// own lock.
type Line struct {
	depth    int
	per      int64
	identity int64
	counter  int64
	station  int64           // station clicks: every load the line has taken
	inFlight int             // loads past the station, not yet counted
	loads    map[int64]int64 // loads by the identity they were loaded under
	changeTo *int64          // the identity a drain change lands; nil when none waits
	paused   bool            // loading paused (Pause): nothing loads, the line counts out
	// slot is the MES struct slot the line counts on, 0-based: identity and
	// counter are that slot's CATID and counter. held is every other slot as
	// the count left it (slots.go).
	slot int
	held [MESSlots]Slot
}

// LineState is a line's ledger at one instant: the loads taken from the carts
// (in total and by the identity each was loaded under), the loads in flight,
// and the identity and counter the PLC publishes.
type LineState struct {
	Loads      int64           `json:"loads"`
	ByIdentity map[int64]int64 `json:"loads_by_identity"`
	InFlight   int             `json:"in_flight"`
	Identity   int64           `json:"identity"`
	Counter    int64           `json:"counter"`
	// Slot is the MES struct slot the line counts on (1-based) and Slots the
	// struct's pairs as the PLC publishes them; Identity and Counter are
	// Slots[Slot-1]'s.
	Slot  int            `json:"slot"`
	Slots [MESSlots]Slot `json:"slots"`
	// Paused is true while the line's loading is paused (Line.Pause).
	Paused bool `json:"paused"`
}

// NewLine returns an empty line of the given depth whose counter ticks by per
// for each load it counts. A depth below zero is zero: every load is counted on
// the tick it is taken, the counter of a process with no line.
func NewLine(depth int, per int64) *Line {
	return &Line{depth: max(depth, 0), per: per, loads: make(map[int64]int64)}
}

// Tick is one machine cycle. A drain change whose line is empty lands and is
// the whole tick (the reset stays visible for a tick rather than being
// overwritten by the next count). While a change waits or loading is paused,
// nothing loads and one in-flight load counts out; otherwise one load enters
// and, once the line is full, the oldest counts out.
func (l *Line) Tick() {
	if l.land() {
		return
	}
	draining := l.changeTo != nil || l.paused
	if !draining {
		l.station++
		l.loads[l.identity]++
		l.inFlight++
	}
	if l.inFlight > 0 && (draining || l.inFlight > l.depth) {
		l.inFlight--
		l.counter += l.per
	}
}

// Stall is a tick on which the machine does not cycle: nothing loads and
// nothing counts. A drain change whose line is already empty still lands: the
// change is the PLC's, not a machine cycle, and a stopped cell must not hold
// the new identity back.
func (l *Line) Stall() { l.land() }

// SetIdentity moves the identity (the CATID of the slot the line counts on) and
// nothing else (no drain, no reset): a run setting the part the line starts
// on. Later loads are booked to it.
func (l *Line) SetIdentity(v int64) { l.identity = v }

// Change is a program change: loading stops, the in-flight loads count out one
// a tick, and on the tick after the last of them the identity becomes to and
// the counter reads 0; loading then resumes under to. A second change before
// it lands replaces the target.
func (l *Line) Change(to int64) { l.changeTo = &to }

// Force is a program change without the drain: the identity becomes to and the
// counter reads 0 now, the loads in flight stay and count out under to, and
// loading goes on. It cancels a drain change still waiting.
func (l *Line) Force(to int64) {
	l.identity, l.counter, l.changeTo = to, 0, nil
}

// Pause stops loading: from the next tick nothing loads and the loads in
// flight count out one a tick, so the counter ticks until the line is empty
// and then stands. A drain change still lands once the line is empty, and
// loading stays paused after it.
func (l *Line) Pause() { l.paused = true }

// Resume ends a pause: loading goes on from the next tick, and the counter
// waits for the line to refill, as after a drain change.
func (l *Line) Resume() { l.paused = false }

// State returns the line's ledger; the map is a copy.
func (l *Line) State() LineState {
	by := make(map[int64]int64, len(l.loads))
	for id, n := range l.loads {
		by[id] = n
	}
	return LineState{Loads: l.station, ByIdentity: by, InFlight: l.inFlight, Identity: l.identity, Counter: l.counter,
		Slot: l.CountingSlot(), Slots: l.Slots(), Paused: l.paused}
}

// land lands a drain change once the line is empty and reports whether it did.
func (l *Line) land() bool {
	if l.changeTo == nil || l.inFlight > 0 {
		return false
	}
	l.identity, l.counter, l.changeTo = *l.changeTo, 0, nil
	return true
}
