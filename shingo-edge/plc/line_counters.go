package plc

// line_counters.go — the poll's station-counter arm. A station counter clicks
// once per assembly where the part leaves the cart; its delta charges the
// positions bound to it (the engine's walk), in place of the production
// counter. It writes no snapshot, no hourly row and nothing to Core: the
// production counter is the cell's record. Read in the same pass as the
// reporting points, after them.

import (
	"fmt"
	"log"
	"runtime/debug"
	"time"

	"shingo/protocol/clock"
	"shingoedge/store/counters"
)

// lineCounterGap is how far a station counter's last read may trail its
// process's production point before its next read re-seeds instead of
// counting: past it the station was not being read (an Edge rolled back to a
// build without station counters, then forward), and the difference is the
// whole rollback's clicks, which would land on the bound carts as one jump.
const lineCounterGap = 15 * time.Minute

// lineCounterTouch is how often an unchanged read rewrites last_poll_at, so a
// station that is idle while the line runs never trails by the gap.
const lineCounterTouch = time.Minute

// lineCounterKey keys a station counter in the non-numeric streak map, apart
// from the reporting points' ids.
type lineCounterKey int64

// pollAllLineCounters reads every station counter of a live process whose
// production counter is wired and enabled.
func (m *Manager) pollAllLineCounters() {
	lcs, err := m.db.ListPolledLineCounters()
	if err != nil {
		log.Printf("list station counters: %v", err)
		return
	}
	for _, lc := range lcs {
		m.pollLineCounterSafe(lc)
	}
}

func (m *Manager) pollLineCounterSafe(lc counters.LineCounter) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("plc pollLoop: panic polling station counter %d (%s/%s): %v\n%s",
				lc.ID, lc.PLCName, lc.TagName, r, debug.Stack())
		}
	}()
	m.pollLineCounter(lc)
}

// pollLineCounter reads one station counter. A first read seeds; a read after
// the rollback gap re-seeds; otherwise the difference is written (only while
// the row still reads this tag) and emitted with the stroke's identity.
func (m *Manager) pollLineCounter(lc counters.LineCounter) {
	// A station's reads are raised as the reporting points' are (no point:
	// id 0), for the health strip's "unreadable"; a PLC the manager does not
	// hold or is not connected to is one.
	if !m.IsConnected(lc.PLCName) {
		m.emitter.EmitCounterReadError(0, lc.PLCName, lc.TagName, "PLC "+lc.PLCName+" is not connected")
		return
	}
	val, err := m.ReadTag(lc.PLCName, lc.TagName)
	if err != nil {
		m.DebugLog.Log("station counter read error: %s/%s lc=%d: %v", lc.PLCName, lc.TagName, lc.ID, err)
		m.emitter.EmitCounterReadError(0, lc.PLCName, lc.TagName, err.Error())
		return
	}
	n, ok := toInt64(val)
	if !ok {
		if _, seen := m.nonNumeric.LoadOrStore(lineCounterKey(lc.ID), struct{}{}); !seen {
			log.Printf("station counter %d (%s/%s): read is not a number (%T %v) — nothing is counted until it reads a number",
				lc.ID, lc.PLCName, lc.TagName, val, val)
		}
		m.emitter.EmitCounterReadError(0, lc.PLCName, lc.TagName, fmt.Sprintf("read is not a number: %T %v", val, val))
		return
	}
	m.nonNumeric.Delete(lineCounterKey(lc.ID))
	m.emitter.EmitCounterRead(0, lc.PLCName, lc.TagName, n)
	now := clock.Now()

	seed := ""
	switch {
	case lc.LastPollAt == nil:
		seed = "first read"
	case lc.PointPollAt != nil && lc.PointPollAt.Sub(*lc.LastPollAt) > lineCounterGap:
		seed = fmt.Sprintf("last read %s before its process's counter (a rollback gap)",
			lc.PointPollAt.Sub(*lc.LastPollAt).Round(time.Minute))
	}
	if seed != "" {
		if wrote, err := m.db.UpdateLineCounterRead(lc.ID, lc.PLCName, lc.TagName, n, now); err != nil || !wrote {
			log.Printf("seed station counter %d (%s/%s) at %d: written=%t %v", lc.ID, lc.PLCName, lc.TagName, n, wrote, err)
			return
		}
		log.Printf("station counter %d (%s/%s): %s, %d — counting from here, nothing charged", lc.ID, lc.PLCName, lc.TagName, seed, n)
		return
	}

	delta, anomaly := CalculateDelta(lc.LastCount, n, m.counterJumpThreshold)
	if delta == 0 && anomaly == "" {
		if now.Sub(*lc.LastPollAt) >= lineCounterTouch {
			if _, err := m.db.UpdateLineCounterRead(lc.ID, lc.PLCName, lc.TagName, n, now); err != nil {
				log.Printf("touch station counter %d: %v", lc.ID, err)
			}
		}
		return
	}
	m.DebugLog.Log("station counter delta: lc=%d %s/%s last=%d new=%d delta=%d anomaly=%s",
		lc.ID, lc.PLCName, lc.TagName, lc.LastCount, n, delta, anomaly)
	// Written only while the row still reads this tag: no write, no emit.
	if wrote, err := m.db.UpdateLineCounterRead(lc.ID, lc.PLCName, lc.TagName, n, now); err != nil || !wrote {
		log.Printf("update station counter %d (%s/%s): written=%t %v — not emitted", lc.ID, lc.PLCName, lc.TagName, wrote, err)
		return
	}
	if delta > 0 {
		id := m.emitter.AttributeTick(lc.ProcessID, lc.ActiveStyleID)
		m.emitter.EmitLineCounterDelta(lc.ID, lc.ProcessID, delta, anomaly, id)
	}
}
