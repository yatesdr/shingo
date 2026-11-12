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
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
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
// A single value yields a one-element set; a two-part pin ("70000011,70000012")
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
// of the row's produce claims' catalog values, used when the style has no
// manual pin. (A pin-retirement pass that would compare a pin against it was
// planned and never built.)
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
	// ProduceClaims is the style's produce claims with a payload; zero is a
	// consume-only style, whose set the catalog cannot supply.
	ProduceClaims int
	// Active and Target: the process's active style, its open target.
	Active, Target bool
	// Pinned: the set is the style's expected_catid, which a person
	// confirmed (N2's verbs), not the catalog's derivation.
	Pinned bool
	// Assumed: the set holds a value no live style's set did, taken as this
	// style's because the process runs exactly two live styles and the other
	// one is pinned (withAssumed, decision 56); the strip asks for the pin.
	Assumed bool
}

// withAssumed is the process's sets with a confirmed value v that no live
// style's set holds taken as the unpinned style's part (decision 56), when
// the process runs exactly two live styles and exactly one of them is pinned:
// the attribution, the declaration and the force, and the one-tap then read v
// as that style's, its entry marked Assumed. Anything else — three or more
// live styles, none or both pinned, a value a set already holds or that names
// no part — is the sets as they are (the pin suggestion, decision 17).
// Nothing is pinned. Pure, over sets already in hand: +0 statements.
func withAssumed(sets []styleCATIDEntry, v string) []styleCATIDEntry {
	if !isIdentity(v) || len(sets) != 2 || sets[0].Pinned == sets[1].Pinned || anySetHas(sets, v) {
		return sets
	}
	out := append([]styleCATIDEntry(nil), sets...)
	i := 0
	if out[0].Pinned {
		i = 1
	}
	set := make(map[string]struct{}, len(out[i].Set)+1)
	for k := range out[i].Set {
		set[k] = struct{}{}
	}
	set[v] = struct{}{}
	out[i].Set, out[i].Assumed = set, true
	return out
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
		out = append(out, styleCATIDEntry{ID: r.StyleID, Name: r.Name, Set: catidSetFromIdentity(r), ProduceClaims: r.ProduceClaims,
			Active: r.Active, Target: r.Target, Pinned: strings.TrimSpace(r.ExpectedCATID) != ""})
	}
	return out
}

// cutoverCATIDSets returns the CATID sets the auto-cutover decides over for
// the PLC's value v: out, the active style's, and in, the open changeover's
// target's, from the one process-wide read, v taken as the unpinned style's
// where decision 56 assumes it (withAssumed). An active style missing from
// the live styles (retired but still running) is read on its own, as
// activeStyleCATIDSet does.
func (e *Engine) cutoverCATIDSets(proc *processes.Process, targetStyleID int64, v string) (out, in map[string]struct{}) {
	foundActive := proc.ActiveStyleID == nil
	for _, st := range withAssumed(e.processStyleCATIDSets(proc.ID), v) {
		if proc.ActiveStyleID != nil && st.ID == *proc.ActiveStyleID {
			out, foundActive = st.Set, true
		}
		if st.ID == targetStyleID {
			in = st.Set
		}
	}
	if !foundActive {
		if style, err := e.db.GetStyle(*proc.ActiveStyleID); err == nil && style != nil {
			out = e.styleCATIDSet(style)
		}
	}
	return out, in
}

// plannedChangeoverTo returns the open changeover when its target style's
// CATID set holds v — the program changed ahead of a planned cutover — with
// the gate's blockers as they stand: the A5 notice and the strip's waiting
// item. (nil, nil) when no changeover is open or its target does not run v.
func (e *Engine) plannedChangeoverTo(processID int64, v string, sets []styleCATIDEntry) (*processes.Changeover, []string) {
	co, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil || co == nil {
		return nil, nil
	}
	if !targetHolds(e, co.ToStyleID, v, sets) {
		return nil, nil
	}
	_, blockers, err := e.canCompleteChangeover(co.ID)
	if err != nil {
		return co, []string{"the gate could not be read"}
	}
	return co, domain.BlockersToReasons(blockers)
}

// targetHolds reports whether the changeover's target runs v: its entry in
// sets (the caller's, decision 56's assumption applied), else its own set read
// (a target outside the live styles, or no sets given).
func targetHolds(e *Engine, toStyleID int64, v string, sets []styleCATIDEntry) bool {
	for _, st := range sets {
		if st.ID == toStyleID {
			return catidSetHas(st.Set, v)
		}
	}
	style, err := e.db.GetStyle(toStyleID)
	return err == nil && style != nil && catidSetHas(e.styleCATIDSet(style), v)
}

// suggestCATIDPin is the pin suggestion from a completed changeover's evidence
// (pure): the value the PLC was on when its post-cutover watch closed is
// suggested for the changeover's to-style when it names a part, is not the
// value the PLC was already on when the changeover started, is not already in
// the to-style's set, and is not the from-style's (when that set has any).
// Nothing is ever suggested from "the style that was active": a run with no
// changeover leaves no evidence. Returns the value and whether to suggest it.
func suggestCATIDPin(co processes.Changeover, toSet, fromSet map[string]struct{}) (string, bool) {
	v := co.EndCATID
	if !isIdentity(v) || v == co.StartCATID || catidSetHas(toSet, v) {
		return "", false
	}
	if len(fromSet) > 0 && catidSetHas(fromSet, v) {
		return "", false
	}
	return v, true
}

// ErrCATIDTaken is a pin refused because another live style's set already
// holds the value: one CATID names one style.
var ErrCATIDTaken = errors.New("another style already runs that CATID")

// CheckStyleCATIDPin validates a typed expected_catid for a style of the
// process: every value new to the style's stored pin must name a part, and
// none may be in another live style's set. A value already in the pin passes:
// a rename re-sends the pin unchanged, and a pin written before one CATID
// named one style would otherwise block every save of either style. A blank
// pin (derive from the catalog) passes. styleID 0 is a style about to be
// created.
func (e *Engine) CheckStyleCATIDPin(processID, styleID int64, pin string) error {
	var stored map[string]struct{}
	if styleID != 0 {
		if style, err := e.db.GetStyle(styleID); err == nil && style != nil {
			stored = parseCATIDList(style.ExpectedCATID)
		}
	}
	for v := range parseCATIDList(pin) {
		if catidSetHas(stored, v) {
			continue
		}
		if !isIdentity(v) {
			return fmt.Errorf("CATID %q names no part (0 and negative values are what an unused slot reads)", v)
		}
		if other, ok := otherStyleHolding(e.processStyleCATIDSets(processID), styleID, v); ok {
			return fmt.Errorf("%w: CATID %s is %s's", ErrCATIDTaken, v, other)
		}
	}
	return nil
}

// AddStyleCATID writes v into a style's pin: the pin becomes the style's set
// as it stands (its pin, or the catalog-derived set) plus v — a bare {v} would
// evict a catalog set, since a pin IS the set. Refused when v is not one value
// that names a part, or by the typed pin's check (CheckStyleCATIDPin: another
// live style holds it). The strip's one-click and the post-cutover flag's
// Confirm write through it; by and evidence go to the log. A write pokes the
// CATID monitor: the pin may make a value it already ruled inert cut over or
// switch.
func (e *Engine) AddStyleCATID(processID, styleID int64, v, by, evidence string) error {
	v = strings.TrimSpace(v)
	if !isIdentity(v) {
		return fmt.Errorf("CATID %q names no part", v)
	}
	if err := e.CheckStyleCATIDPin(processID, styleID, v); err != nil {
		return err
	}
	style, err := e.db.GetStyle(styleID)
	if err != nil || style == nil || style.ProcessID != processID {
		return fmt.Errorf("style %d is not a style of process %d", styleID, processID)
	}
	set := e.styleCATIDSet(style)
	if catidSetHas(set, v) {
		return nil
	}
	merged := map[string]struct{}{v: {}}
	for s := range set {
		merged[s] = struct{}{}
	}
	pin := strings.ReplaceAll(formatCATIDSet(merged), " ", "")
	if err := e.db.SetStyleExpectedCATID(styleID, pin); err != nil {
		return err
	}
	log.Printf("catid pin: style %s (%d) on process %d now %s — added %s by %s: %s", style.Name, styleID, processID, pin, v, by, evidence)
	e.pokeCutover(processID)
	return nil
}

// StyleCATIDPinSaved is the style save telling the CATID monitor a typed pin
// changed. The cutover and the switch decide over the styles' sets, and a
// value already decided is decided again only on a poke
// (TestPoke_APinRedecidesADecidedFalseValue is the one-click's twin).
func (e *Engine) StyleCATIDPinSaved(processID int64) {
	e.pokeCutover(processID)
}

// otherStyleHolding names a live style other than styleID whose set holds v.
func otherStyleHolding(sets []styleCATIDEntry, styleID int64, v string) (string, bool) {
	for _, s := range sets {
		if s.ID != styleID && catidSetHas(s.Set, v) {
			return s.Name, true
		}
	}
	return "", false
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
// target (the prompt pre-fills it; the wrong-part item offers a changeover to it
// when its set is a pin); more than one = ambiguous (nothing guesses — auto falls
// back to the prompt, decideWithNoChangeover); zero = maps to no configured style. This is why the
// uniqueness assumption is checked in code, not assumed.
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
