// catid_health.go — the part-identity health strip.
//
// Every standing condition of a process's part identity, as items computed
// from CURRENT state on each call: an item shows while its condition is true
// and clears itself, so nothing here is driven by an event (an admin flip with
// an unchanged CATID raises no event, and its wrong part must still show). The
// homepage reads the per-process summary; the processes page lists the items.
// EventCATIDHealthChanged is only a hint to refetch.

package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"shingo/protocol/clock"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// The strip's severities, worst first.
const (
	CatidHealthRed   = "red"
	CatidHealthAmber = "amber"
	CatidHealthInfo  = "info"
)

// The strip's item kinds.
const (
	catidKindWrongPart       = "wrong_part"
	catidKindChangeoverWaits = "changeover_waiting"
	catidKindUnmapped        = "unmapped_catid"
	catidKindShared          = "shared_catid"
	catidKindNoCATIDs        = "style_no_catids"
	catidKindIdentity        = "identity"
	catidKindPostCutover     = "post_cutover_failed"
	catidKindHeld            = "held"
	// catidKindOneWayPair is not a part-identity condition: it is the page's
	// warning on a legacy A/B pair, shown in the same strip.
	catidKindOneWayPair       = "one_way_pair"
	catidHealthUnmappedWindow = 14 * 24 * time.Hour
)

// CatidHealthItem is one standing condition of a process's part identity.
type CatidHealthItem struct {
	Kind     string         `json:"kind"`
	Severity string         `json:"severity"`
	Headline string         `json:"headline"`
	Detail   string         `json:"detail"`
	Since    *time.Time     `json:"since,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}

// CatidHealthSummary is one process's line on the homepage: its worst item.
type CatidHealthSummary struct {
	ProcessID int64  `json:"process_id"`
	Name      string `json:"name"`
	Worst     string `json:"worst"`
	Count     int    `json:"count"`
	Headline  string `json:"headline"`
}

// CATIDHealthChangedEvent hints that a process's health items may have moved.
type CATIDHealthChangedEvent struct {
	ProcessID int64 `json:"process_id"`
}

// catidHealthChanged emits the refresh hint for a process.
func (e *Engine) catidHealthChanged(processID int64) {
	if e.Events != nil {
		e.Events.Emit(Event{Type: EventCATIDHealthChanged, Payload: CATIDHealthChangedEvent{ProcessID: processID}})
	}
}

// CatidHealth returns the process's standing items, worst first.
func (e *Engine) CatidHealth(processID int64) ([]CatidHealthItem, error) {
	proc, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	now := clock.Now()
	sets := e.processStyleCATIDSets(processID)
	var items []CatidHealthItem
	if tag := identityTagOf(proc); tag != "" {
		var facts catidFacts
		if e.catidMon != nil {
			facts = e.catidMon.factsOf(processID, now)
		}
		items = append(items, identityItems(proc, tag, facts, now)...)
		if e.catidMon != nil {
			items = append(items, slotItems(proc, e.catidMon.slotFactsOf(processID))...)
		}
		items = append(items, e.partItems(proc, facts, sets)...)
		items = append(items, noCATIDItems(sets)...)
		items = append(items, e.unmappedItems(processID, sets, now)...)
	}
	items = append(items, e.oneWayPairItems(processID)...)
	items = append(items, e.stationHealthItems(processID)...)
	holds, _ := e.db.ListOpenCatidHolds(processID)
	items = append(items, heldItems(holds, now)...)
	items = markUnmappedHeld(items, holds)
	items = append(items, sharedItems(sets)...)
	if flag, ok := e.PostCutoverFlag(processID); ok {
		items = append(items, CatidHealthItem{Kind: catidKindPostCutover, Severity: CatidHealthRed,
			Headline: "Post-cutover check failed", Detail: flag.Message,
			Data: map[string]any{"changeover_id": flag.ChangeoverID, "live_catid": flag.LiveCATID}})
	}
	sort.SliceStable(items, func(i, j int) bool { return severityRank(items[i].Severity) < severityRank(items[j].Severity) })
	return items, nil
}

// CatidHealthSummary returns one line per process with at least one item.
func (e *Engine) CatidHealthSummary() ([]CatidHealthSummary, error) {
	procs, err := e.db.ListProcesses()
	if err != nil {
		return nil, err
	}
	out := []CatidHealthSummary{}
	for _, p := range procs {
		items, err := e.CatidHealth(p.ID)
		if err != nil || len(items) == 0 {
			continue
		}
		out = append(out, CatidHealthSummary{ProcessID: p.ID, Name: p.Name, Worst: items[0].Severity,
			Count: len(items), Headline: items[0].Headline})
	}
	return out, nil
}

func severityRank(s string) int {
	switch s {
	case CatidHealthRed:
		return 0
	case CatidHealthAmber:
		return 1
	default:
		return 2
	}
}

// identityItems: the identity tag read is failing — no good read within the
// age the attribution trusts (tickIdentityFresh), after which a stroke is
// attributed by today's rule (TestTick_StaleIdentityFallsBackToToday) — or the
// monitor holds no state for a process that has one (a process given its
// counter since the last start of a monitor that is not running).
func identityItems(proc *processes.Process, tag string, f catidFacts, now time.Time) []CatidHealthItem {
	data := map[string]any{"tag": proc.CounterPLCName + "." + tag}
	if !f.monitored {
		return []CatidHealthItem{{Kind: catidKindIdentity, Severity: CatidHealthAmber,
			Headline: "Part identity not monitored",
			Detail:   "nothing reads " + tag + " for this process; it is read from the next start", Data: data}}
	}
	if !f.lastOK.IsZero() && now.Sub(f.lastOK) <= tickIdentityFresh {
		return nil
	}
	detail := "no good read of " + tag + " yet"
	if f.lastErr != "" {
		detail = tag + ": " + f.lastErr
	}
	item := CatidHealthItem{Kind: catidKindIdentity, Severity: CatidHealthAmber,
		Headline: "Part identity unreadable", Detail: detail, Data: data}
	if !f.lastOK.IsZero() {
		t := f.lastOK
		item.Since = &t
	}
	return []CatidHealthItem{item}
}

// partItems: the PLC's part is not the active style's — the open changeover's
// target's (the program changed ahead of the cutover: plannedChangeoverTo), or
// nobody's.
func (e *Engine) partItems(proc *processes.Process, f catidFacts, sets []styleCATIDEntry) []CatidHealthItem {
	v := f.value
	if !isIdentity(v) {
		return nil
	}
	var active styleCATIDEntry
	for _, s := range sets {
		if s.Active {
			active = s
		}
	}
	if catidSetHas(active.Set, v) {
		return nil
	}
	if co, waiting := e.plannedChangeoverTo(proc.ID, v); co != nil {
		detail := "the gate is green; it cuts over on the declaration"
		if len(waiting) > 0 {
			detail = "waiting on " + strings.Join(waiting, "; ")
		}
		return []CatidHealthItem{{Kind: catidKindChangeoverWaits, Severity: CatidHealthAmber,
			Headline: fmt.Sprintf("Changeover to %s waiting; the PLC is on its part %s", co.ToStyleName, v),
			Detail:   detail, Data: map[string]any{"changeover_id": co.ID, "catid": v, "waiting_on": append([]string{}, waiting...)}}}
	}
	if len(active.Set) == 0 {
		return nil
	}
	n, _ := e.db.SumCounterDeltaSince(proc.ID, v, f.confirmedAt.UnixMilli())
	since := "since restart"
	countedAs := e.countedAs(proc, v, sets)
	item := CatidHealthItem{Kind: catidKindWrongPart, Severity: CatidHealthAmber,
		Headline: fmt.Sprintf("Wrong part: the PLC is on %s; %s runs %s", v, active.Name, formatCATIDSet(active.Set)),
		Data:     map[string]any{"catid": v, "parts": n, "counting_as": countedAs}}
	if !f.baseline && !f.confirmedAt.IsZero() {
		t := f.confirmedAt
		item.Since = &t
		since = "since " + t.Format("2006-01-02 15:04")
	}
	if n > 0 {
		item.Severity = CatidHealthRed
	}
	item.Detail = fmt.Sprintf("%s, %d parts", since, n)
	if countedAs != "" {
		item.Detail += " counted as " + countedAs
	}
	return []CatidHealthItem{item}
}

// countedAs names the style a stroke on v is counted under, as the poll
// attributes it now (attributeCATID over the monitor's snapshot): the style
// that runs v off-plan, the running style when the attribution falls back to
// today's; "" when the stroke is held, counted under no style.
func (e *Engine) countedAs(proc *processes.Process, v string, sets []styleCATIDEntry) string {
	var observed map[string]struct{}
	var runningOut, point int64
	if e.catidMon != nil {
		if snap, ok := e.catidMon.snapshot(proc.ID); ok {
			observed, runningOut = snap.observed, snap.runningOut()
		}
	}
	if proc.ActiveStyleID != nil {
		point = *proc.ActiveStyleID
	}
	id := attributeCATID(v, point, sets, observed, runningOut)
	if id.Kind == plc.TickHeld {
		return ""
	}
	for _, s := range sets {
		if s.ID == id.StyleID {
			return s.Name
		}
	}
	return ""
}

// noCATIDItems: the active style or the open target has no CATIDs, so the
// monitor cannot tell its part.
func noCATIDItems(sets []styleCATIDEntry) []CatidHealthItem {
	var items []CatidHealthItem
	for _, s := range sets {
		if !(s.Active || s.Target) || len(s.Set) > 0 {
			continue
		}
		role, severity := "running", CatidHealthAmber
		if !s.Active {
			role, severity = "incoming", CatidHealthInfo
		}
		detail := "its parts have no CATID in the catalog; pin one"
		if s.ProduceClaims == 0 {
			detail = "the catalog cannot supply this one; confirm one from a changeover"
		}
		items = append(items, CatidHealthItem{Kind: catidKindNoCATIDs, Severity: severity,
			Headline: fmt.Sprintf("The %s style %s has no CATIDs", role, s.Name), Detail: detail,
			Data: map[string]any{"style_id": s.ID}})
	}
	return items
}

// unmappedItems: a value the PLC sent in the last 14 days that no live style's
// set holds, with the styles it could be pinned to, and the suggestion when a
// completed changeover's evidence names one (suggestCATIDPin).
func (e *Engine) unmappedItems(processID int64, sets []styleCATIDEntry, now time.Time) []CatidHealthItem {
	obs, err := e.db.ListPLCCATIDObservationsForProcess(processID, now.Add(-catidHealthUnmappedWindow))
	if err != nil {
		return nil
	}
	recent, _ := e.db.ListRecentProcessChangeovers(processID, catidSuggestionLookback)
	styles := make([]map[string]any, 0, len(sets))
	for _, s := range sets {
		styles = append(styles, map[string]any{"id": s.ID, "name": s.Name})
	}
	var items []CatidHealthItem
	for _, o := range obs {
		if !isIdentity(o.CATID) || anySetHas(sets, o.CATID) {
			continue
		}
		item := CatidHealthItem{Kind: catidKindUnmapped, Severity: CatidHealthAmber,
			Headline: fmt.Sprintf("CATID %s maps to no style", o.CATID),
			Detail:   fmt.Sprintf("seen %d times, last %s", o.Observations, o.LastSeen),
			Data:     map[string]any{"catid": o.CATID, "last_seen": o.LastSeen, "observations": o.Observations, "styles": styles}}
		if co, ok := suggestionFor(recent, sets, o.CATID); ok {
			item.Detail += fmt.Sprintf("; changeover #%d to %s ended on %s", co.ID, co.ToStyleName, o.CATID)
			item.Data["suggestion"] = map[string]any{"style_id": co.ToStyleID, "style_name": co.ToStyleName, "changeover_id": co.ID}
		}
		items = append(items, item)
	}
	return items
}

// catidSuggestionLookback is how many of the process's latest changeovers a
// suggestion is looked for in.
const catidSuggestionLookback = 20

// suggestionFor finds the newest completed changeover whose evidence suggests
// v for its to-style.
func suggestionFor(recent []processes.Changeover, sets []styleCATIDEntry, v string) (processes.Changeover, bool) {
	setOf := func(id *int64) map[string]struct{} {
		for _, s := range sets {
			if id != nil && s.ID == *id {
				return s.Set
			}
		}
		return nil
	}
	for _, co := range recent {
		if co.State != "completed" || co.EndCATID != v {
			continue
		}
		to := co.ToStyleID
		if _, ok := suggestCATIDPin(co, setOf(&to), setOf(co.FromStyleID)); ok {
			return co, true
		}
	}
	return processes.Changeover{}, false
}

// oneWayPairItems: each claim naming an A/B partner that does not name it
// back — no pair, so a stroke with neither side lit is charged nowhere.
func (e *Engine) oneWayPairItems(processID int64) []CatidHealthItem {
	pairs, err := e.db.ListOneWayPairs(processID)
	if err != nil {
		return nil
	}
	items := make([]CatidHealthItem, 0, len(pairs))
	for _, p := range pairs {
		items = append(items, CatidHealthItem{Kind: catidKindOneWayPair, Severity: CatidHealthAmber,
			Headline: fmt.Sprintf("%s names %s as its A/B partner; %s does not name it back", p.Node, p.Partner, p.Partner),
			Detail:   fmt.Sprintf("style %s: not a pair, so it has no fallthrough — set both sides to name each other", p.StyleName),
			Data:     map[string]any{"style_id": p.StyleID, "node": p.Node, "partner": p.Partner}})
	}
	return items
}

// heldItems: each open hold, red, with its quantity and its age. Charge and
// Write off act on its id.
func heldItems(holds []store.CatidHold, now time.Time) []CatidHealthItem {
	items := make([]CatidHealthItem, 0, len(holds))
	for _, h := range holds {
		where := "the whole draw"
		if h.ProcessNodeID != 0 {
			where = fmt.Sprintf("node %d's share of %s", h.ProcessNodeID, h.PayloadCode)
		}
		t := h.FirstTickAt
		items = append(items, CatidHealthItem{Kind: catidKindHeld, Severity: CatidHealthRed,
			Headline: fmt.Sprintf("%d held on CATID %s", h.Qty, h.CATID),
			Detail: fmt.Sprintf("%s, %d strokes over %s (%s)", where, h.Strokes,
				now.Sub(t).Round(time.Minute), h.Reason),
			Since: &t,
			Data: map[string]any{"hold_id": h.ID, "catid": h.CATID, "qty": h.Qty, "strokes": h.Strokes,
				"process_node_id": h.ProcessNodeID, "payload_code": h.PayloadCode, "reason": h.Reason}})
	}
	return items
}

// markUnmappedHeld turns an unmapped CATID red while counts are held on it.
func markUnmappedHeld(items []CatidHealthItem, holds []store.CatidHold) []CatidHealthItem {
	held := map[string]bool{}
	for _, h := range holds {
		held[h.CATID] = true
	}
	for i := range items {
		if items[i].Kind == catidKindUnmapped && held[fmt.Sprint(items[i].Data["catid"])] {
			items[i].Severity = CatidHealthRed
		}
	}
	return items
}

func anySetHas(sets []styleCATIDEntry, v string) bool {
	for _, s := range sets {
		if catidSetHas(s.Set, v) {
			return true
		}
	}
	return false
}

// sharedItems: a CATID two live styles both hold. The auto-cutover between
// them never fires on it.
func sharedItems(sets []styleCATIDEntry) []CatidHealthItem {
	by := map[string][]string{}
	for _, s := range sets {
		for v := range s.Set {
			by[v] = append(by[v], s.Name)
		}
	}
	vals := make([]string, 0, len(by))
	for v, names := range by {
		if len(names) > 1 {
			vals = append(vals, v)
		}
	}
	sort.Strings(vals)
	items := make([]CatidHealthItem, 0, len(vals))
	for _, v := range vals {
		items = append(items, CatidHealthItem{Kind: catidKindShared, Severity: CatidHealthAmber,
			Headline: fmt.Sprintf("CATID %s is in %s", v, strings.Join(by[v], " and ")),
			Detail:   "the automatic cutover between them never fires on it",
			Data:     map[string]any{"catid": v, "styles": by[v]}})
	}
	return items
}
