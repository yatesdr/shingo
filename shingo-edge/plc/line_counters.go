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

// lineCounterGap is how far a station counter's last poll may trail its
// process's production point's last counted read, as the run before this boot
// left them, before the counter's first read of the boot re-seeds instead of
// counting. While a build that reads stations runs, the row is written at
// least once a minute (lineCounterTouch, on a failed read too), so it trails
// its point by under a minute. Past the gap a build that does not read
// stations counted the point (an Edge rolled back, then forward) and charged
// the bound positions through the production walk; the station's difference
// would charge them again. A downtime, with nothing polled, leaves no gap: its
// clicks are counted, as the point counts its own.
const lineCounterGap = 2 * lineCounterTouch

// lineCounterTouch is how often an unchanged or failed read rewrites
// last_poll_at, so a station idle or unreadable while the line runs never
// trails its point into the next boot's gap.
const lineCounterTouch = time.Minute

// lineCounterKey keys a station counter in the non-numeric streak map, apart
// from the reporting points' ids.
type lineCounterKey int64

func (m *Manager) pollLineCounterSafe(lc counters.LineCounter) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("plc pollLoop: panic polling station counter %d (%s/%s): %v\n%s",
				lc.ID, lc.PLCName, lc.TagName, r, debug.Stack())
		}
	}()
	m.pollLineCounter(lc)
}

// pollLineCounter reads one station counter. A first read seeds, as does the
// first read of a boot that follows a rollback (bootVerdict); otherwise the
// difference is written (only while the row still reads this tag) and emitted
// with the stroke's identity. Within a run a gap never re-seeds: a station
// read that failed for an hour while the line ran was not charged by anything,
// and its first good read counts the hour.
func (m *Manager) pollLineCounter(lc counters.LineCounter) {
	verdict := m.bootVerdict(lc)
	// A station's reads are raised as the reporting points' are (no point:
	// id 0), for the health strip's "unreadable"; a PLC the manager does not
	// hold or is not connected to is one.
	if !m.IsConnected(lc.PLCName) {
		m.emitter.EmitCounterReadError(0, lc.PLCName, lc.TagName, m.notConnected(lc.PLCName))
		m.touchLineCounter(lc)
		return
	}
	val, err := m.ReadTag(lc.PLCName, lc.TagName)
	if err != nil {
		m.DebugLog.Log("station counter read error: %s/%s lc=%d: %v", lc.PLCName, lc.TagName, lc.ID, err)
		m.emitter.EmitCounterReadError(0, lc.PLCName, lc.TagName, err.Error())
		m.touchLineCounter(lc)
		return
	}
	n, ok := toInt64(val)
	if !ok {
		if _, seen := m.nonNumeric.LoadOrStore(lineCounterKey(lc.ID), struct{}{}); !seen {
			log.Printf("station counter %d (%s/%s): read is not a number (%T %v) — nothing is counted until it reads a number",
				lc.ID, lc.PLCName, lc.TagName, val, val)
		}
		m.emitter.EmitCounterReadError(0, lc.PLCName, lc.TagName, fmt.Sprintf("read is not a number: %T %v", val, val))
		m.touchLineCounter(lc)
		return
	}
	m.nonNumeric.Delete(lineCounterKey(lc.ID))
	m.emitter.EmitCounterRead(0, lc.PLCName, lc.TagName, n)
	now := clock.Now()

	seed := verdict
	if lc.LastPollAt == nil {
		seed = "first read"
	}
	if seed != "" {
		if wrote, err := m.db.UpdateLineCounterRead(lc.ID, lc.PLCName, lc.TagName, n, now); err != nil || !wrote {
			log.Printf("seed station counter %d (%s/%s) at %d: written=%t %v", lc.ID, lc.PLCName, lc.TagName, n, wrote, err)
			return
		}
		m.lineBoot.Store(lc.ID, "")
		log.Printf("station counter %d (%s/%s): %s, %d — counting from here, nothing charged", lc.ID, lc.PLCName, lc.TagName, seed, n)
		return
	}

	delta, anomaly := CalculateDelta(lc.LastCount, n, m.counterJumpThreshold)
	if delta == 0 && anomaly == "" {
		m.touchLineCounter(lc)
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

// bootVerdict is the counter's rollback verdict for this boot, taken at its
// first listing, before this boot has written its point or its row: its last
// poll trailing its point's last counted read by more than lineCounterGap
// names the gap; "" counts on. A first read that seeds consumes it.
func (m *Manager) bootVerdict(lc counters.LineCounter) string {
	if v, ok := m.lineBoot.Load(lc.ID); ok {
		return v.(string)
	}
	verdict := ""
	if lc.LastPollAt != nil && lc.PointPollAt != nil && lc.PointPollAt.Sub(*lc.LastPollAt) > lineCounterGap {
		verdict = fmt.Sprintf("last polled %s before its process's counter was last counted, before this boot (a rollback)",
			lc.PointPollAt.Sub(*lc.LastPollAt).Round(time.Second))
	}
	m.lineBoot.Store(lc.ID, verdict)
	return verdict
}

// touchLineCounter rewrites a polled row's last_poll_at, at most once a
// minute, with the count it holds: an unchanged read, or one that failed while
// this build ran. A row never read stays unread, so its first good read seeds.
func (m *Manager) touchLineCounter(lc counters.LineCounter) {
	now := clock.Now()
	if lc.LastPollAt == nil || now.Sub(*lc.LastPollAt) < lineCounterTouch {
		return
	}
	if _, err := m.db.UpdateLineCounterRead(lc.ID, lc.PLCName, lc.TagName, lc.LastCount, now); err != nil {
		log.Printf("touch station counter %d: %v", lc.ID, err)
	}
}
