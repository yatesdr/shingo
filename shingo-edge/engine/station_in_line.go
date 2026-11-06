// station_in_line.go — what a station counter's card measures: the parts in
// the line since the last program change, the deepest run-out, and the
// clicks per production stroke. Read on the process page and in the binding's
// log; nothing that charges a cart reads any of it.

package engine

import (
	"fmt"
	"time"

	"shingoedge/store/counters"
)

// identityChangedAt is when the CATID monitor last confirmed the process's
// part changing to one that names a part; zero when its confirmed value is
// the first read after its start, names no part, or there is none. The
// poll's anchor asks (plc line_anchor.go). No lock: the snapshot.
func (e *Engine) identityChangedAt(processID int64) time.Time {
	cm := e.tickMonitor.Load()
	if cm == nil {
		return time.Time{}
	}
	snap, ok := cm.snapshot(processID)
	if !ok || snap.baseline || !isIdentity(snap.value) {
		return time.Time{}
	}
	return snap.confirmedAt
}

// StationCounterCard is a station counter as its card reads it: the row, its
// deepest run-out the deeper of the stored one and this boot's; the parts in
// the line (nil: not anchored); the median clicks per production stroke over
// the intervals read so far (nil before the first); and the production
// strokes since its last click, Stopped past the deepest run-out (the strip's
// measure, stationStopped).
type StationCounterCard struct {
	counters.LineCounter
	InLine            *int64   `json:"in_line"`
	Ratio             *float64 `json:"ratio"`
	RatioIntervals    int      `json:"ratio_intervals"`
	StrokesSinceClick int64    `json:"strokes_since_click"`
	Stopped           bool     `json:"stopped"`
}

// ListStationCounters returns a process's station counters as their cards
// read them ([] for none). The parts in the line are computed here, on read.
func (e *Engine) ListStationCounters(processID int64) ([]StationCounterCard, error) {
	list, err := e.db.ListLineCounters(processID)
	if err != nil {
		return nil, err
	}
	out := make([]StationCounterCard, 0, len(list))
	if len(list) == 0 {
		return out, nil
	}
	point := e.pointCount(processID)
	for _, lc := range list {
		s := e.stationHealth.snapshot(lc.ID, processID)
		d := deepestOf(lc, s)
		c := StationCounterCard{LineCounter: lc, RatioIntervals: len(s.ratios), StrokesSinceClick: s.prodSinceClick,
			Stopped: stationStopped(s.prodSinceClick, d)}
		c.DeepestRunout = &d
		if n, ok := inLine(lc, point); ok {
			c.InLine = &n
		}
		if m, ok := median(s.ratios); ok {
			c.Ratio = &m
		}
		out = append(out, c)
	}
	return out, nil
}

// pointCount is the process's production point's last count: since the
// counter's last reset, what it has counted. 0 when it has none.
func (e *Engine) pointCount(processID int64) int64 {
	proc, err := e.db.GetProcess(processID)
	if err != nil || proc == nil || proc.CounterPLCName == "" || proc.CounterTagName == "" {
		return 0
	}
	rp, err := e.db.GetReportingPointByTag(proc.CounterPLCName, proc.CounterTagName)
	if err != nil || rp == nil {
		return 0
	}
	return rp.LastCount
}

// inLine is the parts between a station counter and the production counter:
// its clicks since its anchor less the production counter's counts since the
// reset that set it (the counter counts from 0 again at the reset, so its
// last count is that). Not ok while unanchored, or when the counts say the
// anchor no longer holds (the station's own counter went back since).
func inLine(lc counters.LineCounter, pointCount int64) (int64, bool) {
	if lc.AnchorCount == nil {
		return 0, false
	}
	n := lc.LastCount - *lc.AnchorCount - pointCount
	return n, n >= 0
}

// inLineWords names the parts in the line for the binding's log and confirm.
func inLineWords(n int64, ok bool) string {
	if !ok {
		return "the parts in the line (not anchored yet: the next program change measures them) net against this cart's run-out under the borrowing rule"
	}
	return fmt.Sprintf("≈%d in the line; they net against this cart's run-out under the borrowing rule", n)
}

// stationInLine is a station counter's parts in the line, read for the
// binding's log.
func (e *Engine) stationInLine(counterID int64) (int64, bool) {
	lc, err := e.db.GetLineCounter(counterID)
	if err != nil || lc == nil {
		return 0, false
	}
	return inLine(*lc, e.pointCount(lc.ProcessID))
}

// deepestOf is a station counter's deepest run-out: the stored one or this
// boot's, whichever is deeper.
func deepestOf(lc counters.LineCounter, s stationTrack) int64 {
	if lc.DeepestRunout != nil && *lc.DeepestRunout > s.deepest {
		return *lc.DeepestRunout
	}
	return s.deepest
}

// raiseDeepest stores a deeper run-out than this boot had seen (the store's
// write is a no-op when the stored one is deeper still).
func (e *Engine) raiseDeepest(counterID, deepest int64) {
	if err := e.db.RaiseLineCounterDeepest(counterID, deepest); err != nil {
		e.logFn("station counter %d: store the deepest run-out (%d): %v", counterID, deepest, err)
	}
}
