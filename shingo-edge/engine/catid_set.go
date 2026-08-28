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

// parseCATIDList splits a manual expected_catid pin (comma-separated) into a set,
// trimming whitespace and dropping empties. A single value yields a one-element
// set; a two-part pin ("40017111,40017112") yields both.
func parseCATIDList(pin string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, part := range strings.Split(pin, ",") {
		if v := strings.TrimSpace(part); v != "" {
			set[v] = struct{}{}
		}
	}
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
		for _, part := range strings.Split(ce.CATID, ",") {
			if v := strings.TrimSpace(part); v != "" {
				set[v] = struct{}{}
			}
		}
	}
	return set
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
	styles, err := e.db.ListStylesByProcess(processID)
	if err != nil {
		return nil
	}
	var out []styleCATIDMatch
	for i := range styles {
		if catidSetHas(e.styleCATIDSet(&styles[i]), catid) {
			out = append(out, styleCATIDMatch{ID: styles[i].ID, Name: styles[i].Name})
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
func (e *Engine) activeStyleCATIDSet(processID int64) (styleID int64, styleName string, set map[string]struct{}, ok bool) {
	proc, err := e.db.GetProcess(processID)
	if err != nil || proc == nil || proc.ActiveStyleID == nil {
		return 0, "", nil, false
	}
	style, err := e.db.GetStyle(*proc.ActiveStyleID)
	if err != nil || style == nil {
		return 0, "", nil, false
	}
	return style.ID, style.Name, e.styleCATIDSet(style), true
}
