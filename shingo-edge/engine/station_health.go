// station_health.go — the station counters' health lines on the process's
// strip: stopped while the line produces, not one click per part, unreadable
// (the production counter's tag too), and a bound position that became an A/B
// pair. Tracked in memory from the counting events; only the deepest run-out
// is also stored, when it grows (station_in_line.go).

package engine

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"shingo/protocol/clock"
	"shingoedge/store/counters"
	"shingoedge/store/processes"
)

const (
	// stationStopFloor is the production strokes a station may go without a
	// click before "stopped" can show at all, until a deeper run-out has been
	// seen: the first run-out is not mistaken for a stop.
	stationStopFloor = 20
	// stationRatioWindow is how many production intervals with a click the
	// clicks-per-part median reads, so a start-up lead lands in one of them.
	stationRatioWindow = 20

	stationKindStopped    = "station_stopped"
	stationKindRatio      = "station_ratio"
	counterKindUnreadable = "counter_unreadable"
	stationKindPaired     = "station_paired"
)

// stationTrack is one station counter's running account, this boot's.
type stationTrack struct {
	processID int64
	// clicks since the last production stroke, and the production strokes
	// since the last click.
	clicksSinceProd int64
	prodSinceClick  int64
	// deepest is the most production strokes ever counted between two
	// clicks: the deepest run-out (a cart's last parts in the line).
	deepest int64
	// ratios: clicks per production stroke over the last intervals that had a
	// click; a run-out's strokes with none say nothing about the tag.
	ratios []float64
}

// readState is a tag's last good read and last failure.
type readState struct {
	okAt, errAt time.Time
	err         string
}

// stationHealthTracker is the engine's in-memory account; the zero value works.
type stationHealthTracker struct {
	mu       sync.Mutex
	stations map[int64]*stationTrack // counter id → track
	reads    map[string]*readState   // "plc/tag" → read state
	// produced is each process's production strokes since boot: the account
	// of a counter that has not clicked since (snapshot).
	produced map[int64]int64
}

func (h *stationHealthTracker) onProductionStroke(processID, delta int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.produced == nil {
		h.produced = map[int64]int64{}
	}
	h.produced[processID] += delta
	for _, s := range h.stations {
		if s.processID != processID {
			continue
		}
		s.prodSinceClick += delta
		if s.clicksSinceProd > 0 {
			s.ratios = append(s.ratios, float64(s.clicksSinceProd)/float64(delta))
			if len(s.ratios) > stationRatioWindow {
				s.ratios = s.ratios[len(s.ratios)-stationRatioWindow:]
			}
		}
		s.clicksSinceProd = 0
	}
}

// startTrack begins a counter's account at zero: a counter made or moved to
// another tag while the line runs counts its strokes from then, not from boot.
func (h *stationHealthTracker) startTrack(counterID, processID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stations == nil {
		h.stations = map[int64]*stationTrack{}
	}
	h.stations[counterID] = &stationTrack{processID: processID}
}

// onStationClick counts a station counter's clicks; it reports the deepest
// run-out when this click made it deeper (0 otherwise), for the store.
func (h *stationHealthTracker) onStationClick(counterID, processID, delta int64) (deeper int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stations == nil {
		h.stations = map[int64]*stationTrack{}
	}
	s := h.stations[counterID]
	if s == nil {
		s = &stationTrack{processID: processID}
		h.stations[counterID] = s
	}
	if s.prodSinceClick > s.deepest {
		s.deepest = s.prodSinceClick
		deeper = s.deepest
	}
	s.prodSinceClick = 0
	s.clicksSinceProd += delta
	return deeper
}

func (h *stationHealthTracker) onRead(plcName, tagName string, failure string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reads == nil {
		h.reads = map[string]*readState{}
	}
	r := h.reads[plcName+"/"+tagName]
	if r == nil {
		r = &readState{}
		h.reads[plcName+"/"+tagName] = r
	}
	now := clock.Now()
	if failure == "" {
		r.okAt = now
		return
	}
	r.errAt, r.err = now, failure
}

// snapshot copies one counter's track. A counter with none has not clicked
// since boot (nor been made or moved since): every production stroke of its
// process since boot is a stroke since its last click.
func (h *stationHealthTracker) snapshot(counterID, processID int64) stationTrack {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.stations[counterID]
	if !ok {
		return stationTrack{processID: processID, prodSinceClick: h.produced[processID]}
	}
	c := *s
	c.ratios = append([]float64(nil), s.ratios...)
	return c
}

func (h *stationHealthTracker) unreadable(plcName, tagName string) (string, time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.reads[plcName+"/"+tagName]
	if !ok || r.errAt.IsZero() || !r.errAt.After(r.okAt) {
		return "", time.Time{}, false
	}
	return r.err, r.errAt, true
}

// median of the ratios (none: 0, false).
func median(v []float64) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2], true
	}
	return (s[n/2-1] + s[n/2]) / 2, true
}

// stationHealthItems: the process's counters' health lines.
func (e *Engine) stationHealthItems(processID int64) []CatidHealthItem {
	proc, err := e.db.GetProcess(processID)
	if err != nil || proc == nil {
		return nil
	}
	var items []CatidHealthItem
	if proc.CounterPLCName != "" && proc.CounterTagName != "" {
		if why, at, bad := e.stationHealth.unreadable(proc.CounterPLCName, proc.CounterTagName); bad {
			items = append(items, CatidHealthItem{Kind: counterKindUnreadable, Severity: CatidHealthRed,
				Headline: fmt.Sprintf("Production counter %s unreadable", proc.CounterTagName), Detail: why, Since: &at,
				Data: map[string]any{"plc_name": proc.CounterPLCName, "tag_name": proc.CounterTagName}})
		}
	}
	lcs, err := e.db.ListLineCounters(processID)
	if err != nil || len(lcs) == 0 {
		return items
	}
	nodes, _ := e.db.ListProcessNodesByProcess(processID)
	_, changeoverOpen := e.openChangeover(processID)
	for _, lc := range lcs {
		name := lc.Label
		if name == "" {
			name = lc.TagName
		}
		if why, at, bad := e.stationHealth.unreadable(lc.PLCName, lc.TagName); bad {
			items = append(items, CatidHealthItem{Kind: counterKindUnreadable, Severity: CatidHealthRed,
				Headline: fmt.Sprintf("Station counter %s unreadable", name), Detail: why, Since: &at,
				Data: map[string]any{"counter_id": lc.ID, "plc_name": lc.PLCName, "tag_name": lc.TagName}})
		}
		activeBound, paired := e.boundPositions(proc, nodes, lc.ID)
		for _, p := range paired {
			items = append(items, CatidHealthItem{Kind: stationKindPaired, Severity: CatidHealthAmber,
				Headline: fmt.Sprintf("%s became an A/B pair under %s — counted at the cell's counter while %s runs", p.node, p.style, p.style),
				Detail:   fmt.Sprintf("bound to %s; pairs are counted by the production counter", name),
				Data:     map[string]any{"counter_id": lc.ID, "node": p.node, "style": p.style}})
		}
		s := e.stationHealth.snapshot(lc.ID, processID)
		deepest := s.deepest
		if !changeoverOpen && activeBound && stationStopped(s.prodSinceClick, deepest) {
			items = append(items, CatidHealthItem{Kind: stationKindStopped, Severity: CatidHealthAmber,
				Headline: fmt.Sprintf("Station counter %s stopped while the line produces", name),
				Detail:   fmt.Sprintf("%d strokes since its last click; the deepest run-out seen is %d", s.prodSinceClick, deepest),
				Data:     map[string]any{"counter_id": lc.ID, "strokes": s.prodSinceClick, "deepest": deepest}})
		}
		// Quiet while a changeover is open (the station and the cell count
		// different parts' strokes) or a run-out is: more than one stroke
		// since the last click, one being a pass that read the cell's stroke
		// before the station's click.
		if !changeoverOpen && s.prodSinceClick <= 1 && len(s.ratios) >= stationRatioWindow {
			if m, _ := median(s.ratios); m < 0.75 || m > 1.5 {
				items = append(items, CatidHealthItem{Kind: stationKindRatio, Severity: CatidHealthAmber,
					Headline: fmt.Sprintf("Station counter %s: not one click per part", name),
					Detail:   fmt.Sprintf("%.1f clicks per part over the last %d strokes — a tag that counts something else (a weld gun?)", m, len(s.ratios)),
					Data:     map[string]any{"counter_id": lc.ID, "median": m}})
			}
		}
	}
	return items
}

// StationCounterCard is a station counter as its card reads it: the row; the
// median clicks per production stroke over the intervals read so far (nil
// before the first); and the production strokes since its last click, Stopped
// past the deepest run-out this boot has seen (the strip's measure,
// stationStopped).
type StationCounterCard struct {
	counters.LineCounter
	Ratio             *float64 `json:"ratio"`
	RatioIntervals    int      `json:"ratio_intervals"`
	StrokesSinceClick int64    `json:"strokes_since_click"`
	Stopped           bool     `json:"stopped"`
}

// ListStationCounters returns a process's station counters as their cards
// read them ([] for none).
func (e *Engine) ListStationCounters(processID int64) ([]StationCounterCard, error) {
	list, err := e.db.ListLineCounters(processID)
	if err != nil {
		return nil, err
	}
	out := make([]StationCounterCard, 0, len(list))
	for _, lc := range list {
		s := e.stationHealth.snapshot(lc.ID, processID)
		c := StationCounterCard{LineCounter: lc, RatioIntervals: len(s.ratios), StrokesSinceClick: s.prodSinceClick,
			Stopped: stationStopped(s.prodSinceClick, s.deepest)}
		if m, ok := median(s.ratios); ok {
			c.Ratio = &m
		}
		out = append(out, c)
	}
	return out, nil
}

// stationStopped: more production strokes since the station's last click than
// the deepest run-out seen, and than stationStopFloor.
func stationStopped(strokes, deepest int64) bool {
	return strokes > max(deepest, stationStopFloor)
}

// pairedBinding is a position bound to a station counter whose claim under a
// live style names an A/B partner: while that style runs, the position is
// counted at the cell's counter (countedAt).
type pairedBinding struct{ node, style string }

// boundPositions reports whether a position counted at the counter is claimed
// by the active style unpaired (the station counts it now), and names each
// bound position and live style whose claim there became an A/B pair. Every
// style is read at every position: one style's pair says nothing about the
// active style's claim.
func (e *Engine) boundPositions(proc *processes.Process, nodes []processes.Node, counterID int64) (activeBound bool, paired []pairedBinding) {
	styles, _ := e.db.ListStylesByProcess(proc.ID)
	for _, n := range nodes {
		if n.LineCounterID == nil || *n.LineCounterID != counterID {
			continue
		}
		for _, s := range styles {
			c, err := e.db.GetStyleNodeClaimByNode(s.ID, n.CoreNodeName)
			if err != nil || c == nil {
				continue
			}
			if c.PairedCoreNode != "" && !c.IsPressIndex() {
				paired = append(paired, pairedBinding{node: n.Name, style: s.Name})
				continue
			}
			if proc.ActiveStyleID != nil && s.ID == *proc.ActiveStyleID {
				activeBound = true
			}
		}
	}
	return activeBound, paired
}

// openChangeover reports the process's open changeover id.
func (e *Engine) openChangeover(processID int64) (int64, bool) {
	co, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil || co == nil {
		return 0, false
	}
	return co.ID, true
}
