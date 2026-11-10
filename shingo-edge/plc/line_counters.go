package plc

// line_counters.go — the poll's station-counter arm. A station counter clicks
// once per assembly where the part leaves the cart; its delta charges the
// positions bound to it (the engine's walk), in place of the production
// counter. It writes no snapshot, no hourly row and nothing to Core: the
// production counter is the cell's record. Read in the same pass as the
// reporting points, after them.
//
// A counter may read two tags (a station with one fixture per hand, one hand
// running at a time) and counts their sum. Each tag is read as a one-tag
// counter's is: it seeds on its own first read, and its reset or jump is its
// own (CalculateDelta); the counter emits the sum of the two tags' counted
// differences, once.

import (
	"fmt"
	"log"
	"runtime/debug"
	"strings"
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

// lineCounterTouch is how often an unchanged or failed read rewrites a tag's
// last poll, so a station idle or unreadable while the line runs never trails
// its point into the next boot's gap.
const lineCounterTouch = time.Minute

// lineCounterKey keys one of a station counter's tags in the non-numeric
// streak map and the boot verdicts, apart from the reporting points' ids.
type lineCounterKey struct {
	id     int64
	second bool
}

// lineTag is one of a station counter's tags as the pass listed it: its name
// and its own last reading (polled nil: never read, its first read seeds).
type lineTag struct {
	second bool
	name   string
	last   int64
	polled *time.Time
}

// lineTags is the counter's tags, the first first.
func lineTags(lc counters.LineCounter) []lineTag {
	out := []lineTag{{name: lc.TagName, last: lc.LastCount, polled: lc.LastPollAt}}
	if lc.TagName2 != "" {
		out = append(out, lineTag{second: true, name: lc.TagName2, last: lc.LastCount2, polled: lc.LastPollAt2})
	}
	return out
}

func (m *Manager) pollLineCounterSafe(lc counters.LineCounter) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("plc pollLoop: panic polling station counter %d (%s/%s): %v\n%s",
				lc.ID, lc.PLCName, strings.Join(lc.Tags(), " + "), r, debug.Stack())
		}
	}()
	m.pollLineCounter(lc)
}

// pollLineCounter reads one station counter, each of its tags in turn
// (pollLineTag), and emits the sum of what they counted, with the stroke's
// identity, when it is any. Within a run a gap never re-seeds: a station read
// that failed for an hour while the line ran was not charged by anything, and
// its first good read counts the hour.
func (m *Manager) pollLineCounter(lc counters.LineCounter) {
	tags := lineTags(lc)
	// The verdicts are taken first, at the counter's first listing, before
	// this boot has written its point or its row.
	verdicts := make([]string, len(tags))
	for i, t := range tags {
		verdicts[i] = m.bootVerdict(lc, t)
	}
	// A station's reads are raised as the reporting points' are (no point:
	// id 0), for the health strip's "unreadable"; a PLC the manager does not
	// hold or is not connected to is one, for each tag.
	if !m.IsConnected(lc.PLCName) {
		why := m.notConnected(lc.PLCName)
		for _, t := range tags {
			m.emitter.EmitCounterReadError(0, lc.PLCName, t.name, why)
			m.touchLineTag(lc, t)
		}
		return
	}
	var delta int64
	var anomalies []string
	for i, t := range tags {
		d, anomaly := m.pollLineTag(lc, t, verdicts[i])
		delta += d
		if anomaly != "" {
			anomalies = append(anomalies, anomaly)
		}
	}
	if delta > 0 {
		id := m.emitter.AttributeTick(lc.ProcessID, lc.ActiveStyleID)
		m.emitter.EmitLineCounterDelta(lc.ID, lc.ProcessID, delta, strings.Join(anomalies, ", "), id)
	}
}

// pollLineTag reads one of a counter's tags and reports what it counted. A
// first read seeds, as does the first read of a boot that follows a rollback
// (verdict); otherwise the difference is written, only while the row still
// reads this tag, and counted. A reset counts its new reading, the clicks
// since the tag restarted (CalculateDelta), never the drop. No write, no
// count.
func (m *Manager) pollLineTag(lc counters.LineCounter, t lineTag, verdict string) (int64, string) {
	key := lineCounterKey{id: lc.ID, second: t.second}
	val, err := m.ReadTag(lc.PLCName, t.name)
	if err != nil {
		m.DebugLog.Log("station counter read error: %s/%s lc=%d: %v", lc.PLCName, t.name, lc.ID, err)
		m.emitter.EmitCounterReadError(0, lc.PLCName, t.name, err.Error())
		m.touchLineTag(lc, t)
		return 0, ""
	}
	n, ok := toInt64(val)
	if !ok {
		if _, seen := m.nonNumeric.LoadOrStore(key, struct{}{}); !seen {
			log.Printf("station counter %d (%s/%s): read is not a number (%T %v) — nothing is counted until it reads a number",
				lc.ID, lc.PLCName, t.name, val, val)
		}
		m.emitter.EmitCounterReadError(0, lc.PLCName, t.name, fmt.Sprintf("read is not a number: %T %v", val, val))
		m.touchLineTag(lc, t)
		return 0, ""
	}
	m.nonNumeric.Delete(key)
	m.emitter.EmitCounterRead(0, lc.PLCName, t.name, n)
	now := clock.Now()

	seed := verdict
	if t.polled == nil {
		seed = "first read"
	}
	if seed != "" {
		if wrote, err := m.db.UpdateLineCounterRead(lc.ID, lc.PLCName, t.name, t.second, n, now); err != nil || !wrote {
			log.Printf("seed station counter %d (%s/%s) at %d: written=%t %v", lc.ID, lc.PLCName, t.name, n, wrote, err)
			return 0, ""
		}
		m.lineBoot.Store(key, "")
		log.Printf("station counter %d (%s/%s): %s, %d — counting from here, nothing charged", lc.ID, lc.PLCName, t.name, seed, n)
		return 0, ""
	}

	delta, anomaly := CalculateDelta(t.last, n, m.counterJumpThreshold)
	if delta == 0 && anomaly == "" {
		m.touchLineTag(lc, t)
		return 0, ""
	}
	m.DebugLog.Log("station counter delta: lc=%d %s/%s last=%d new=%d delta=%d anomaly=%s",
		lc.ID, lc.PLCName, t.name, t.last, n, delta, anomaly)
	// Written only while the row still reads this tag: no write, no count.
	if wrote, err := m.db.UpdateLineCounterRead(lc.ID, lc.PLCName, t.name, t.second, n, now); err != nil || !wrote {
		log.Printf("update station counter %d (%s/%s): written=%t %v — not counted", lc.ID, lc.PLCName, t.name, wrote, err)
		return 0, ""
	}
	return delta, anomaly
}

// bootVerdict is a tag's rollback verdict for this boot, taken at its
// counter's first listing, before this boot has written its point or its row:
// the counter's last poll (the later of its two tags', so a build that read
// only the first tag counts the second's clicks it charged nothing for)
// trailing its point's last counted read by more than lineCounterGap names
// the gap; "" counts on. The tag's first read that seeds consumes it.
func (m *Manager) bootVerdict(lc counters.LineCounter, t lineTag) string {
	key := lineCounterKey{id: lc.ID, second: t.second}
	if v, ok := m.lineBoot.Load(key); ok {
		return v.(string)
	}
	last := lc.LastPollAt
	if lc.LastPollAt2 != nil && (last == nil || lc.LastPollAt2.After(*last)) {
		last = lc.LastPollAt2
	}
	verdict := ""
	if last != nil && lc.PointPollAt != nil && lc.PointPollAt.Sub(*last) > lineCounterGap {
		verdict = fmt.Sprintf("last polled %s before its process's counter was last counted, before this boot (a rollback)",
			lc.PointPollAt.Sub(*last).Round(time.Second))
	}
	m.lineBoot.Store(key, verdict)
	return verdict
}

// touchLineTag rewrites a polled tag's last poll, at most once a minute, with
// the count it holds: an unchanged read, or one that failed while this build
// ran. A tag never read stays unread, so its first good read seeds.
func (m *Manager) touchLineTag(lc counters.LineCounter, t lineTag) {
	now := clock.Now()
	if t.polled == nil || now.Sub(*t.polled) < lineCounterTouch {
		return
	}
	if _, err := m.db.UpdateLineCounterRead(lc.ID, lc.PLCName, t.name, t.second, t.last, now); err != nil {
		log.Printf("touch station counter %d (%s/%s): %v", lc.ID, lc.PLCName, t.name, err)
	}
}
