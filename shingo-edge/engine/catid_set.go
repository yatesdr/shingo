// catid_set.go — a style's part-identity SET.
//
// On a two-position press each side's produce claim runs its own part, so a style
// can legitimately carry more than one CATID (left/right). The guard, auto-arm,
// and post-cutover verification all reason over the SET of a style's valid CATIDs,
// and the single live CATID_01 must be a MEMBER of the active style's set.
//
// The set is DERIVED from the style's produce claims — each claim's payload → the
// synced payload-catalog CATID — unless the style carries a manual pin
// (expected_catid, a comma-separated list), which then IS the set verbatim. Empty
// set = inert (the guard/auto-arm/verify all no-op), exactly like a blank
// expected_catid did before.

package engine

import (
	"sort"
	"strings"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// addCATIDList splits a comma-joined CATID list into set, trimming whitespace
// and dropping empties. It is the one splitter for both spellings of a list —
// a manual expected_catid pin and a synced catalog value — so the two cannot
// drift apart.
func addCATIDList(set map[string]struct{}, list string) {
	for _, part := range strings.Split(list, ",") {
		if v := strings.TrimSpace(part); v != "" {
			set[v] = struct{}{}
		}
	}
}

// parseCATIDList splits a manual expected_catid pin (comma-separated) into a set.
// A single value yields a one-element set; a two-part pin ("40017111,40017112")
// yields both.
func parseCATIDList(pin string) map[string]struct{} {
	set := map[string]struct{}{}
	addCATIDList(set, pin)
	return set
}

// styleCATIDSet returns the style's part-identity set: the manual pin when
// expected_catid is non-empty (the comma-separated list IS the set), else the set
// derived from the style's produce claims' payload CATIDs.
func (e *Engine) styleCATIDSet(style *processes.Style) map[string]struct{} {
	if style == nil {
		return nil
	}
	if strings.TrimSpace(style.ExpectedCATID) != "" {
		return parseCATIDList(style.ExpectedCATID)
	}
	return e.derivedCATIDSet(style.ID)
}

// derivedCATIDSet unions the CATIDs of a style's PRODUCE claims, each resolved
// from its payload via the synced payload catalog. The catalog value is itself
// a comma-joined list (a multi-part kit payload carries every part it holds —
// Core sends the distinct set since the multi-part catalog sync), so each
// value splits into the set. A claim whose payload has no catalog CATID
// contributes nothing (so a partially-configured style yields the subset that
// is known — never a guess).
//
// ONE style, 1 + produce-claims queries. The single-style callers (the
// post-cutover verify tick, the A5 relief guard, the changeover guard) hold
// one style and are right to read one; anything that asks for every style in
// a process goes through processStyleCATIDSets instead.
func (e *Engine) derivedCATIDSet(styleID int64) map[string]struct{} {
	set := map[string]struct{}{}
	claims, err := e.db.ListStyleNodeClaims(styleID)
	if err != nil {
		return set
	}
	for _, c := range claims {
		if c.Role != protocol.ClaimRoleProduce || c.PayloadCode == "" {
			continue
		}
		ce, err := e.db.GetPayloadCatalogByCode(c.PayloadCode)
		if err != nil || ce == nil || ce.CATID == "" {
			continue
		}
		addCATIDList(set, ce.CATID)
	}
	return set
}

// catidSetFromIdentity applies the same two rules as styleCATIDSet to a row of
// the process-wide read: a non-blank manual pin IS the set, verbatim;
// otherwise the union of the produce claims' catalog values, each split on
// commas. Empty in, empty out.
func catidSetFromIdentity(row processes.StylePartIdentity) map[string]struct{} {
	if strings.TrimSpace(row.ExpectedCATID) != "" {
		return parseCATIDList(row.ExpectedCATID)
	}
	return derivedCATIDSetFromIdentity(row)
}

// derivedCATIDSetFromIdentity is the batch form of derivedCATIDSet: the union
// of the row's produce claims' catalog values, ignoring any manual pin. The
// pin-retirement pass needs the derived set on its own to compare the pin
// against.
func derivedCATIDSetFromIdentity(row processes.StylePartIdentity) map[string]struct{} {
	set := map[string]struct{}{}
	for _, v := range row.CatalogCATIDs {
		addCATIDList(set, v)
	}
	return set
}

// styleCATIDEntry is one live style of a process with its part-identity set.
type styleCATIDEntry struct {
	ID   int64
	Name string
	Set  map[string]struct{}
}

// processStyleCATIDSets returns every LIVE style of the process with its
// part-identity set, in name order, from ONE query. This is the process-wide
// form of styleCATIDSet: the per-style derivation was one claim list plus one
// catalog lookup per produce claim for every style in the process, ~450
// queries per PLC part change at 90 styles × 4 claims, and the store runs on
// one connection. A read failure yields no styles, which every caller treats
// as "maps to nothing" — the same inert answer derivedCATIDSet gave.
func (e *Engine) processStyleCATIDSets(processID int64) []styleCATIDEntry {
	rows, err := processes.ListStylePartIdentitiesByProcess(e.db.DB, processID)
	if err != nil {
		return nil
	}
	out := make([]styleCATIDEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, styleCATIDEntry{ID: r.StyleID, Name: r.Name, Set: catidSetFromIdentity(r)})
	}
	return out
}

// catidSetHas reports membership.
func catidSetHas(set map[string]struct{}, catid string) bool {
	_, ok := set[catid]
	return ok
}

// formatCATIDSet renders a set as a sorted, comma-separated string for operator
// messages. Empty set → "".
func formatCATIDSet(set map[string]struct{}) string {
	if len(set) == 0 {
		return ""
	}
	vals := make([]string, 0, len(set))
	for v := range set {
		vals = append(vals, v)
	}
	sort.Strings(vals)
	return strings.Join(vals, ", ")
}

// styleCATIDMatch names a style whose part-identity set contains a given CATID.
type styleCATIDMatch struct {
	ID   int64
	Name string
}

// stylesForCATID returns EVERY style in the process whose part-identity set
// contains catid. The multiplicity is the point: exactly one = an unambiguous
// target (auto-arm may arm to it); more than one = ambiguous (auto-arm must NOT
// guess — it falls back to the prompt); zero = maps to no configured style. This
// is why the uniqueness assumption is checked in code, not assumed.
func (e *Engine) stylesForCATID(processID int64, catid string) []styleCATIDMatch {
	if catid == "" {
		return nil
	}
	var out []styleCATIDMatch
	for _, st := range e.processStyleCATIDSets(processID) {
		if catidSetHas(st.Set, catid) {
			out = append(out, styleCATIDMatch{ID: st.ID, Name: st.Name})
		}
	}
	return out
}

// matchNames projects match names for logging/messages.
func matchNames(matches []styleCATIDMatch) []string {
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = m.Name
	}
	return names
}

// activeStyleCATIDSet returns the process's active style id, name, and
// part-identity set. ok=false when there is no active style or the lookup fails.
//
// Two queries: the process, then the process-wide read the active style is
// picked out of. The per-style path it replaces was 2 + produce-claims, and
// this runs on the same part-change edge as stylesForCATID.
func (e *Engine) activeStyleCATIDSet(processID int64) (styleID int64, styleName string, set map[string]struct{}, ok bool) {
	proc, err := e.db.GetProcess(processID)
	if err != nil || proc == nil || proc.ActiveStyleID == nil {
		return 0, "", nil, false
	}
	for _, st := range e.processStyleCATIDSets(processID) {
		if st.ID == *proc.ActiveStyleID {
			return st.ID, st.Name, st.Set, true
		}
	}
	// Not among the process's LIVE styles: a retired style that
	// active_style_id still points at. This path used to go through GetStyle,
	// which is unfiltered on purpose (a caller holding an id is asking "what
	// is this"), so keep answering for the retired-but-running style rather
	// than going inert on a press that is still stamping its part.
	style, err := e.db.GetStyle(*proc.ActiveStyleID)
	if err != nil || style == nil {
		return 0, "", nil, false
	}
	return style.ID, style.Name, e.styleCATIDSet(style), true
}
