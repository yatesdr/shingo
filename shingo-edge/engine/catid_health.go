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
	catidKindWrongPart        = "wrong_part"
	catidKindChangeoverWaits  = "changeover_waiting"
	catidKindForcedLegsOpen   = "forced_legs_open"
	catidKindUnmapped         = "unmapped_catid"
	catidKindShared           = "shared_catid"
	catidKindNoCATIDs         = "style_no_catids"
	catidKindIdentity         = "identity"
	catidKindPostCutover      = "post_cutover_failed"
	catidKindAutoDecisions    = "automatic_decisions"
	catidKindHeld             = "held"
	catidHealthUnmappedWindow = 14 * 24 * time.Hour
	catidHealthDecisionWindow = 24 * time.Hour
	// catidIdentityStale is how old the last good identity read may be before
	// the identity reads as unreadable.
	catidIdentityStale = 10 * time.Second
)

// automaticTriggers are the triggered_by values of a switch or a cutover no
// one pressed CUTOVER for: the strip's "automatic decisions".
var automaticTriggers = []string{
	triggeredAutoCATID, triggeredAutoCATID + triggeredForced, triggeredAutoCATIDFlip,
	triggeredAdminSwitch, triggeredAdminSwitch + triggeredForced,
}

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
	co, _ := e.db.GetActiveProcessChangeover(processID)
	var items []CatidHealthItem
	if tag := identityTagOf(proc); tag != "" {
		var facts identityFacts
		if e.catidMon != nil {
			facts = e.catidMon.identityFacts(processID)
		}
		items = append(items, identityItems(proc, tag, facts, now)...)
		active, target := activeAndTarget(proc, sets, co)
		items = append(items, e.partItems(proc, facts, active, target, co)...)
		items = append(items, noCATIDItems(active, target)...)
		items = append(items, e.unmappedItems(processID, sets, now)...)
	}
	holds, _ := e.db.ListOpenCatidHolds(processID)
	items = append(items, heldItems(holds, now)...)
	items = markUnmappedHeld(items, holds)
	items = append(items, sharedItems(sets)...)
	items = append(items, e.forcedItems(processID)...)
	if flag, ok := e.PostCutoverFlag(processID); ok {
		items = append(items, CatidHealthItem{Kind: catidKindPostCutover, Severity: CatidHealthRed,
			Headline: "Post-cutover check failed", Detail: flag.Message,
			Data: map[string]any{"changeover_id": flag.ChangeoverID, "live_catid": flag.LiveCATID}})
	}
	items = append(items, e.decisionItems(processID, now)...)
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

// activeAndTarget picks the active style's and the open changeover's target's
// entries out of the process-wide sets (nil when absent).
func activeAndTarget(proc *processes.Process, sets []styleCATIDEntry, co *processes.Changeover) (active, target *styleCATIDEntry) {
	for i := range sets {
		if proc.ActiveStyleID != nil && sets[i].ID == *proc.ActiveStyleID {
			active = &sets[i]
		}
		if co != nil && sets[i].ID == co.ToStyleID {
			target = &sets[i]
		}
	}
	return active, target
}

// identityItems: the identity tag read is failing, or the monitor holds no
// state for a process that has one (a process given its counter since the
// last start of a monitor that is not running).
func identityItems(proc *processes.Process, tag string, f identityFacts, now time.Time) []CatidHealthItem {
	data := map[string]any{"tag": proc.CounterPLCName + "." + tag}
	if !f.monitored {
		return []CatidHealthItem{{Kind: catidKindIdentity, Severity: CatidHealthAmber,
			Headline: "Part identity not monitored",
			Detail:   "nothing reads " + tag + " for this process; it is read from the next start", Data: data}}
	}
	if !f.lastOK.IsZero() && now.Sub(f.lastOK) <= catidIdentityStale {
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
// target's (the program changed ahead of the cutover), or nobody's.
func (e *Engine) partItems(proc *processes.Process, f identityFacts, active, target *styleCATIDEntry, co *processes.Changeover) []CatidHealthItem {
	v := f.value
	if !isIdentity(v) {
		return nil
	}
	var out, in map[string]struct{}
	if active != nil {
		out = active.Set
	}
	if target != nil {
		in = target.Set
	}
	if catidSetHas(out, v) {
		return nil
	}
	if co != nil && catidSetHas(in, v) {
		_, blockers, _ := e.canCompleteChangeover(co.ID)
		waiting := make([]string, 0, len(blockers))
		for _, b := range blockers {
			waiting = append(waiting, b.Reason)
		}
		detail := "the gate is green; it cuts over on the declaration"
		if len(waiting) > 0 {
			detail = "waiting on " + strings.Join(waiting, "; ")
		}
		return []CatidHealthItem{{Kind: catidKindChangeoverWaits, Severity: CatidHealthAmber,
			Headline: fmt.Sprintf("Changeover to %s waiting; the PLC is on its part %s", co.ToStyleName, v),
			Detail:   detail, Data: map[string]any{"changeover_id": co.ID, "catid": v, "waiting_on": waiting}}}
	}
	if len(out) == 0 {
		return nil
	}
	activeName := ""
	if active != nil {
		activeName = active.Name
	}
	n, _ := e.db.SumCounterDeltaSince(proc.ID, v, f.confirmedAt.UnixMilli())
	since := "since restart"
	item := CatidHealthItem{Kind: catidKindWrongPart, Severity: CatidHealthAmber,
		Headline: fmt.Sprintf("Wrong part: the PLC is on %s; %s runs %s", v, activeName, formatCATIDSet(out)),
		Data:     map[string]any{"catid": v, "parts": n, "counting_as": activeName}}
	if !f.confirmedBaseline && !f.confirmedAt.IsZero() {
		t := f.confirmedAt
		item.Since = &t
		since = "since " + t.Format("2006-01-02 15:04")
	}
	if n > 0 {
		item.Severity = CatidHealthRed
	}
	item.Detail = fmt.Sprintf("%s, %d parts counting as %s", since, n, activeName)
	return []CatidHealthItem{item}
}

// noCATIDItems: the active style or the open target has no CATIDs, so the
// monitor cannot tell its part.
func noCATIDItems(active, target *styleCATIDEntry) []CatidHealthItem {
	var items []CatidHealthItem
	for _, s := range []struct {
		e        *styleCATIDEntry
		role     string
		severity string
	}{{active, "running", CatidHealthAmber}, {target, "incoming", CatidHealthInfo}} {
		if s.e == nil || len(s.e.Set) > 0 {
			continue
		}
		detail := "its parts have no CATID in the catalog; pin one"
		if s.e.ProduceClaims == 0 {
			detail = "the catalog cannot supply this one; confirm one from a changeover"
		}
		items = append(items, CatidHealthItem{Kind: catidKindNoCATIDs, Severity: s.severity,
			Headline: fmt.Sprintf("The %s style %s has no CATIDs", s.role, s.e.Name), Detail: detail,
			Data: map[string]any{"style_id": s.e.ID}})
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

// forcedItems: the latest changeover completed past its gate still has legs
// open.
func (e *Engine) forcedItems(processID int64) []CatidHealthItem {
	co, err := e.db.LatestForcedChangeover(processID)
	if err != nil || co == nil {
		return nil
	}
	ok, blockers, err := e.canCompleteChangeover(co.ID)
	if err != nil || ok {
		return nil
	}
	open := make([]string, 0, len(blockers))
	for _, b := range blockers {
		open = append(open, b.Reason)
	}
	item := CatidHealthItem{Kind: catidKindForcedLegsOpen, Severity: CatidHealthAmber,
		Headline: fmt.Sprintf("Changeover to %s completed past its gate; %d still open", co.ToStyleName, len(open)),
		Detail:   strings.Join(open, "; "), Data: map[string]any{"changeover_id": co.ID, "open": open}}
	if co.CompletedAt != nil {
		t := *co.CompletedAt
		item.Since = &t
	}
	return []CatidHealthItem{item}
}

// decisionItems: the switches and cutovers no one pressed CUTOVER for in the
// last 24 hours.
func (e *Engine) decisionItems(processID int64, now time.Time) []CatidHealthItem {
	rows, err := e.db.ListChangeoversTriggeredSince(processID, automaticTriggers, now.Add(-catidHealthDecisionWindow))
	if err != nil || len(rows) == 0 {
		return nil
	}
	list := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		list = append(list, map[string]any{"changeover_id": r.ID, "from": r.FromStyleName, "to": r.ToStyleName,
			"triggered_by": r.TriggeredBy, "completed_at": r.CompletedAt})
	}
	latest := rows[0]
	return []CatidHealthItem{{Kind: catidKindAutoDecisions, Severity: CatidHealthInfo,
		Headline: fmt.Sprintf("%d automatic decision(s) in 24 h", len(rows)),
		Detail:   fmt.Sprintf("latest: %s to %s (%s)", latest.FromStyleName, latest.ToStyleName, latest.TriggeredBy),
		Data:     map[string]any{"decisions": list}}}
}
