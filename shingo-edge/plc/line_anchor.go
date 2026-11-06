package plc

// line_anchor.go — the parts in the line, anchored at a program change.
//
// Where a cell drains before it changes program, the production counter
// resets at the change with the line empty, so a station counter's count at
// that reset is where the line's loads start again: the parts in the line are
// the station's clicks since then less the counter's counts since (computed on
// read, engine.ListStationCounters). The anchor is written only for a reset
// that came with a confirmed CATID change; any other reset (a hand reset, a
// PLC restore, one read late) may leave parts in the line, and unanchors.
// Nothing that charges a cart reads it.

import (
	"log"
	"runtime/debug"
	"time"

	"shingo/protocol/clock"
	"shingoedge/store/counters"
)

// anchorWindow is how near the reset, either side, the monitor's confirm of a
// CATID change must fall. The CATID and the counter change in one PLC scan,
// and the monitor confirms after its debounce, so the confirm usually lands a
// few seconds after the poll read the reset.
const anchorWindow = 10 * time.Second

// pointReset is a production point's read that went backwards (CalculateDelta's
// "reset"): its process, the count read and when.
type pointReset struct {
	processID int64
	count     int64
	at        time.Time
}

// pendingAnchor is a fresh reset waiting for the monitor's confirm, with each
// of the process's read station counters at the count the reset's pass listed
// (the previous pass's read, never that pass's own).
type pendingAnchor struct {
	at     time.Time
	counts []counters.LineCounter
}

// SetIdentityChangeSource installs what the poll asks for when a process's
// confirmed CATID last changed to a part (zero: not since the monitor's first
// read). Set once at the composition root; read by the poll goroutine.
func (m *Manager) SetIdentityChangeSource(fn func(processID int64) time.Time) {
	m.identityChange.Store(&fn)
}

func (m *Manager) identityChangedAt(processID int64) time.Time {
	if fn := m.identityChange.Load(); fn != nil {
		return (*fn)(processID)
	}
	return time.Time{}
}

func (m *Manager) anchorStationsSafe(lcs []counters.LineCounter, resets []pointReset) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("plc pollLoop: panic anchoring station counters: %v\n%s", r, debug.Stack())
		}
	}()
	m.anchorStations(lcs, resets)
}

// anchorStations is the station pass's first step, after the points. A reset
// unanchors its process's station counters (one statement); read fresh (the
// counter at 0 or 1) it then waits, this pass or a later one, for the
// monitor's confirm of a CATID change within anchorWindow of it, and anchors
// each counter at its listed count (one statement each). Read late (past 1:
// the Edge was not polling across the change), or with no confirm by the end
// of the window, it stays unanchored. A pass with no reset and none waiting
// writes nothing. A restart drops a wait: the counters stay unanchored until
// the next program change.
func (m *Manager) anchorStations(lcs []counters.LineCounter, resets []pointReset) {
	for _, r := range resets {
		delete(m.pendingAnchors, r.processID)
		if err := m.db.ClearLineCounterAnchors(r.processID); err != nil {
			log.Printf("process %d's counter reset: unanchor its station counters: %v", r.processID, err)
		}
		if r.count > 1 {
			log.Printf("process %d's counter reset read late (at %d): its station counters stay unanchored", r.processID, r.count)
			continue
		}
		p := pendingAnchor{at: r.at}
		for _, lc := range lcs {
			if lc.ProcessID == r.processID && lc.LastPollAt != nil {
				p.counts = append(p.counts, lc)
			}
		}
		if len(p.counts) == 0 {
			continue
		}
		if m.pendingAnchors == nil {
			m.pendingAnchors = map[int64]pendingAnchor{}
		}
		m.pendingAnchors[r.processID] = p
	}
	now := clock.Now()
	for pid, p := range m.pendingAnchors {
		switch changed := m.identityChangedAt(pid); {
		case !changed.IsZero() && changed.Sub(p.at).Abs() <= anchorWindow:
			for _, lc := range p.counts {
				if err := m.db.AnchorLineCounter(lc.ID, lc.PLCName, lc.TagName, lc.LastCount, p.at); err != nil {
					log.Printf("anchor station counter %d: %v", lc.ID, err)
				}
			}
			log.Printf("process %d: a program change at %s (CATID confirmed %s): %d station counter(s) anchored",
				pid, p.at.Format(time.RFC3339), changed.Format(time.RFC3339), len(p.counts))
			delete(m.pendingAnchors, pid)
		case now.Sub(p.at) > anchorWindow:
			log.Printf("process %d: a counter reset at %s with no CATID change: its station counters stay unanchored",
				pid, p.at.Format(time.RFC3339))
			delete(m.pendingAnchors, pid)
		}
	}
}
