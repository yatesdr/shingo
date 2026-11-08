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
	"database/sql"
	"errors"
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
	// catidKindAssumed: the PLC's part taken as the unpinned style's
	// (decision 56), its pin asked for.
	catidKindAssumed = "assumed_catid"
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
	// Action is the one verb that resolves the item, when it has one: the
	// strip renders it as a button that calls the verb's endpoint.
	Action *CatidHealthAction `json:"action,omitempty"`
}

// CatidHealthAction is an item's verb. Kind start_changeover starts a
// changeover to ToStyleID through the start verb (StartProcessChangeover).
type CatidHealthAction struct {
	Kind        string `json:"kind"`
	ToStyleID   int64  `json:"to_style_id"`
	ToStyleName string `json:"to_style_name"`
}

// catidActionStartChangeover is the wrong-part item's verb.
const catidActionStartChangeover = "start_changeover"

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
	holds, _ := e.db.ListOpenCatidHolds(processID)
	var items []CatidHealthItem
	if tag := identityTagOf(proc); tag != "" {
		var facts catidFacts
		if e.catidMon != nil {
			facts = e.catidMon.factsOf(processID, now)
		}
		items = append(items, identityItems(proc, tag, facts, now)...)
		if e.catidMon != nil {
			items = append(items, slotItems(proc, e.catidMon.slotFactsOf(processID),
				func() time.Time { return e.lastCountedAt(processID) })...)
		}
		// The wrong-part item reads what the attribution reads: the PLC's
		// part taken as the unpinned style's where decision 56 assumes it.
		// The items that say what is configured (no CATIDs, unmapped and its
		// pin verbs, the suggestion of decision 17) read the pins as they are.
		assumed := withAssumed(sets, facts.value)
		parts := e.partItems(proc, facts, assumed, holds)
		items = append(items, parts...)
		items = append(items, assumedItems(assumed, facts.value, parts)...)
		items = append(items, noCATIDItems(sets)...)
		items = append(items, e.unmappedItems(processID, sets, now)...)
	}
	items = append(items, e.oneWayPairItems(processID)...)
	items = append(items, e.stationHealthItems(processID)...)
	items = append(items, e.heldItems(processID, holds, now)...)
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
// nobody's. The wrong-part item offers the changeover to the one style that
// runs the part when changeoverOffer names it, and says what to do; nothing
// moves until a person starts it.
func (e *Engine) partItems(proc *processes.Process, f catidFacts, sets []styleCATIDEntry, holds []store.CatidHold) []CatidHealthItem {
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
	if co, waiting := e.plannedChangeoverTo(proc.ID, v, sets); co != nil {
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
	switch to := e.changeoverOffer(proc, f, sets); {
	case to != nil && to.Assumed:
		// Decision 56: the part taken as the unpinned style's; the offer
		// stands, and the item asks for the pin, yellow.
		item.Severity = CatidHealthAmber
		item.Headline = assumedHeadline(v, to.Name)
		item.Action = &CatidHealthAction{Kind: catidActionStartChangeover, ToStyleID: to.ID, ToStyleName: to.Name}
	case to != nil && !to.Pinned:
		// No pin, no offer: the one-tap needs a person's pin to start from. The
		// headline says which style to pin.
		item.Headline = fmt.Sprintf("The PLC is on %s; the style that runs it, %s, is not pinned — pin it to offer the changeover.",
			v, to.Name)
	case to != nil:
		item.Headline = fmt.Sprintf("The PLC is on %s (%s); %s is active. Start a changeover to %s%s.", v, to.Name,
			active.Name, to.Name, e.heldAtPositions(proc.ID, v, holds))
		item.Action = &CatidHealthAction{Kind: catidActionStartChangeover, ToStyleID: to.ID, ToStyleName: to.Name}
	}
	return []CatidHealthItem{item}
}

// assumedHeadline is the strip's words for a part taken as the unpinned
// style's (decision 56).
func assumedHeadline(v, style string) string {
	return fmt.Sprintf("The PLC is on %s, taken as %s — %s's CATID is not pinned; pin it.", v, style, style)
}

// assumedItems: the PLC's part taken as the unpinned style's (decision 56),
// when the wrong-part item does not already say so — the style running it, or
// the changeover open to it. Yellow, the pin asked for; nothing moves.
func assumedItems(sets []styleCATIDEntry, v string, parts []CatidHealthItem) []CatidHealthItem {
	for _, it := range parts {
		if it.Kind == catidKindWrongPart && it.Action != nil {
			return nil
		}
	}
	for _, s := range sets {
		if s.Assumed {
			return []CatidHealthItem{{Kind: catidKindAssumed, Severity: CatidHealthAmber, Headline: assumedHeadline(v, s.Name),
				Detail: "the process runs two styles and only the other is pinned; pin " + v + " on " + s.Name + " to confirm it",
				Data:   map[string]any{"catid": v, "style_id": s.ID}}}
		}
	}
	return nil
}

// changeoverOffer is the style the wrong-part item offers a changeover to, nil
// when it offers none: the PLC's part is a change, not a baseline, confirmed
// after the process's latest landing (a part standing from before it is the
// run-out of the style the landing left), a stroke has been counted on it off
// another style's plan, that style is the only live one whose set holds it,
// and no changeover is open (the start would refuse it). A read that fails
// offers nothing. The item offers the one-tap when the style's set is a
// person's pin (to.Pinned), or the part is taken as its (to.Assumed, decision
// 56); else its headline names the style to pin.
func (e *Engine) changeoverOffer(proc *processes.Process, f catidFacts, sets []styleCATIDEntry) *styleCATIDEntry {
	if e.catidMon == nil || proc.ActiveStyleID == nil || f.baseline || !countedSince(f, plc.TickOffPlan) ||
		!f.confirmedAt.After(e.catidMon.landedAt(proc.ID)) {
		return nil
	}
	to := switchTarget(sets, *proc.ActiveStyleID, f.value)
	if to == nil {
		return nil
	}
	if co, err := e.db.GetActiveProcessChangeover(proc.ID); co != nil || (err != nil && !errors.Is(err, sql.ErrNoRows)) {
		return nil
	}
	return to
}

// heldAtPositions is the offer's clause on what waits for it: " — N strokes
// held at A and B", the open holds of CATID v at the positions whose part
// differs (an off-plan stroke's share there; N the most strokes one holds);
// "" when none is open.
func (e *Engine) heldAtPositions(processID int64, v string, holds []store.CatidHold) string {
	var at []int64
	strokes := int64(0)
	for _, h := range holds {
		if h.CATID != v || h.ProcessNodeID == 0 {
			continue
		}
		at = append(at, h.ProcessNodeID)
		strokes = max(strokes, h.Strokes)
	}
	if len(at) == 0 {
		return ""
	}
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return ""
	}
	names := map[int64]string{}
	for _, n := range nodes {
		names[n.ID] = n.Name
	}
	var where []string
	for _, id := range at {
		if names[id] != "" {
			where = append(where, names[id])
		}
	}
	sort.Strings(where)
	noun := "strokes"
	if strokes == 1 {
		noun = "stroke"
	}
	return fmt.Sprintf(" — %d %s held at %s", strokes, noun, joinAnd(where))
}

// joinAnd is a list as a sentence names it: "A", "A and B", "A, B and C".
func joinAnd(xs []string) string {
	if len(xs) <= 1 {
		return strings.Join(xs, "")
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
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

// heldItems: each open row of the ledger. A held or off-plan row is red, with
// its quantity and its age; a "no cart" row (decision 55) is a count, not an
// alarm — info, in the words a person reads it by (noCartItem). Charge and
// Write off act on its id either way. The node names are read once, only when
// a "no cart" row is open.
func (e *Engine) heldItems(processID int64, holds []store.CatidHold, now time.Time) []CatidHealthItem {
	items := make([]CatidHealthItem, 0, len(holds))
	var names map[int64]string
	for _, h := range holds {
		if h.Reason == holdReasonNoCart {
			if names == nil {
				names = e.nodeNames(processID)
			}
			items = append(items, noCartItem(h, names[h.ProcessNodeID], ReportingLocation(e.cfg.Timezone)))
			continue
		}
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

// noCartItem is a "no cart" row as the strip says it: "N strokes of P at NODE
// with no P cart — last cart off: bin B at D, HH:MM", or "— no cart released
// there yet" when none was, the times in the plant's zone. Severity info: a
// line that ran off-system for a shift is a row, not an alarm.
func noCartItem(h store.CatidHold, node string, loc *time.Location) CatidHealthItem {
	if node == "" {
		node = fmt.Sprintf("node %d", h.ProcessNodeID)
	}
	noun := "strokes"
	if h.Qty == 1 {
		noun = "stroke"
	}
	last := "no cart released there yet"
	if h.LastBinID != 0 && h.LastLeftAt != nil {
		last = fmt.Sprintf("last cart off: bin %d at %d, %s", h.LastBinID, h.LastDeclared, h.LastLeftAt.In(loc).Format("15:04"))
	}
	catid := "no CATID"
	if h.CATID != "" {
		catid = "CATID " + h.CATID
	}
	t := h.FirstTickAt
	return CatidHealthItem{Kind: catidKindHeld, Severity: CatidHealthInfo,
		Headline: fmt.Sprintf("%d %s of %s at %s with no %s cart — %s", h.Qty, noun, h.PayloadCode, node, h.PayloadCode, last),
		Detail: fmt.Sprintf("logged from %s to %s, %d strokes (%s); Charge it to a cart of %s, or write it off",
			h.FirstTickAt.In(loc).Format("2006-01-02 15:04"), h.LastTickAt.In(loc).Format("15:04"), h.Strokes, catid, h.PayloadCode),
		Since: &t,
		Data: map[string]any{"hold_id": h.ID, "catid": h.CATID, "qty": h.Qty, "strokes": h.Strokes,
			"process_node_id": h.ProcessNodeID, "payload_code": h.PayloadCode, "reason": h.Reason,
			"last_bin_id": h.LastBinID, "last_declared": h.LastDeclared}}
}

// nodeNames is a process's positions by id, {} on a failed read.
func (e *Engine) nodeNames(processID int64) map[int64]string {
	out := map[int64]string{}
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return out
	}
	for _, n := range nodes {
		out[n.ID] = n.Name
	}
	return out
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
