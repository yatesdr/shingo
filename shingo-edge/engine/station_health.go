// station_health.go — the station counters' health lines on the process's
// strip: stopped while the line produces, not one click per part, unreadable
// (the production counter's tag too), and a bound position that became an A/B
// pair. Tracked in memory from the counting events; nothing is stored.

package engine

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"shingo/protocol/clock"
	"shingoedge/store/processes"
)

const (
	// stationStopFloor is the production strokes a station may go without a
	// click before "stopped" can show at all, until a deeper run-out has been
	// seen: the first run-out is not mistaken for a stop.
	stationStopFloor = 20
	// stationRatioWindow is how many production intervals the clicks-per-part
	// median reads, so a start-up lead lands in one of them.
	stationRatioWindow = 20

	stationKindStopped    = "station_stopped"
	stationKindRatio      = "station_ratio"
	counterKindUnreadable = "counter_unreadable"
	stationKindPaired     = "station_paired"
)

// stationTrack is one station counter's running account.
type stationTrack struct {
	processID int64
	// clicks since the last production stroke, and the production strokes
	// since the last click.
	clicksSinceProd int64
	prodSinceClick  int64
	// deepest is the most production strokes ever counted between two
	// clicks: the deepest run-out (a cart's last parts in the line).
	deepest int64
	// ratios: clicks per production stroke over the last intervals.
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
}

func (h *stationHealthTracker) onProductionStroke(processID, delta int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.stations {
		if s.processID != processID {
			continue
		}
		s.prodSinceClick += delta
		s.ratios = append(s.ratios, float64(s.clicksSinceProd)/float64(delta))
		if len(s.ratios) > stationRatioWindow {
			s.ratios = s.ratios[len(s.ratios)-stationRatioWindow:]
		}
		s.clicksSinceProd = 0
	}
}

func (h *stationHealthTracker) onStationClick(counterID, processID, delta int64) {
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
	}
	s.prodSinceClick = 0
	s.clicksSinceProd += delta
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

// snapshot copies one counter's track and one tag's read state.
func (h *stationHealthTracker) snapshot(counterID int64) (stationTrack, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.stations[counterID]
	if !ok {
		return stationTrack{}, false
	}
	c := *s
	c.ratios = append([]float64(nil), s.ratios...)
	return c, true
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
				Headline: fmt.Sprintf("%s became an A/B pair — counted at the cell's counter", p),
				Detail:   fmt.Sprintf("bound to %s; pairs are counted by the production counter", name),
				Data:     map[string]any{"counter_id": lc.ID, "node": p}})
		}
		s, ok := e.stationHealth.snapshot(lc.ID)
		if !ok {
			continue
		}
		limit := s.deepest
		if limit < stationStopFloor {
			limit = stationStopFloor
		}
		if !changeoverOpen && activeBound && s.prodSinceClick > limit {
			items = append(items, CatidHealthItem{Kind: stationKindStopped, Severity: CatidHealthAmber,
				Headline: fmt.Sprintf("Station counter %s stopped while the line produces", name),
				Detail:   fmt.Sprintf("%d strokes since its last click; the deepest run-out seen is %d", s.prodSinceClick, s.deepest),
				Data:     map[string]any{"counter_id": lc.ID, "strokes": s.prodSinceClick, "deepest": s.deepest}})
		}
		if len(s.ratios) >= stationRatioWindow {
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

// boundPositions reports whether a position counted at the counter is
// claimed by the active style, and names the bound positions whose claim (any
// live style) became an A/B pair.
func (e *Engine) boundPositions(proc *processes.Process, nodes []processes.Node, counterID int64) (activeBound bool, paired []string) {
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
				paired = append(paired, n.Name)
				break
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
