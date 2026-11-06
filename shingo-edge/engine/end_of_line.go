// end_of_line.go — the end-of-line sentence. A process that counts its
// carts at the cell's production counter charges a cart when the part is made,
// not when it leaves the cart, so the parts between the two read on the cart
// until they are counted. The sentence says so, with N measured from the
// changeovers whose landing preceded the counter's reset (the run-out), or
// typed by a person where nothing is measured. N decides nothing.

package engine

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/counters"
	"shingoedge/store/processes"
)

// endOfLineWords is the sentence's first half, said whatever N is.
const endOfLineWords = "Counted at the end of the line: carts are charged when the part is made."

// endOfLineFresh is how long the station tile reads a process's sentence from
// the last time it was computed before computing it again: the board builds on
// every count, the record is read on the setup page, the history, and at most
// this often for the tile.
const endOfLineFresh = 10 * time.Minute

// RunOut is a completed changeover's measured run-out: the parts of the style
// it left that the counter counted after its landing, before its next reset.
type RunOut struct {
	ChangeoverID int64     `json:"changeover_id"`
	Parts        int64     `json:"parts"`
	CompletedAt  time.Time `json:"completed_at"`
}

// EndOfLine is what a process's setup says of its end-of-line counting.
// Applies: it counts on a production counter and a position its active style
// consumes at is counted there, not at a station counter (the sentence is
// shown only then). Measured: the run-outs, oldest first, wherever the process
// has a production counter (the changeover history shows them whatever
// counts the positions). HasN: Sentence carries an N. Slots: the process's
// MES slots as the slot watch last read them, which the setup page's Counting
// block shows beside the sentence ([] when it has none).
type EndOfLine struct {
	Applies  bool                `json:"applies"`
	Measured []RunOut            `json:"measured"`
	Typed    processes.LineDepth `json:"typed"`
	Sentence string              `json:"sentence"`
	HasN     bool                `json:"has_n"`
	Slots    []MESSlot           `json:"slots"`
}

// endOfLineMemo is each process's last computed sentence for the station
// tile ("" when it carries no N), and when.
type endOfLineMemo struct {
	mu   sync.Mutex
	seen map[int64]endOfLineSeen
}

type endOfLineSeen struct {
	note string
	at   time.Time
}

// EndOfLine reads a process's end-of-line facts and sentence: on the setup
// page and the changeover history, on read. The run-outs cost the record's
// oldest row, the process's resets, and one sum per measurable changeover
// (counters.RunOuts). It refreshes the tile's memo.
func (e *Engine) EndOfLine(processID int64) (*EndOfLine, error) {
	proc, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	out := &EndOfLine{Measured: []RunOut{}, Slots: e.mesSlots(proc)}
	if out.Typed, err = e.db.GetLineDepth(processID); err != nil {
		return nil, err
	}
	if out.Applies, err = e.countsAtTheEnd(proc); err != nil {
		return nil, err
	}
	if proc.CounterPLCName != "" && proc.CounterTagName != "" {
		if out.Measured, err = e.runOuts(processID); err != nil {
			return nil, err
		}
	}
	// Dates in the plant's zone, read quietly (ReportingLocation logs an unset
	// zone, and this runs on every page read).
	loc := time.UTC
	if l, err := time.LoadLocation(e.cfg.Timezone); err == nil && e.cfg.Timezone != "" {
		loc = l
	}
	out.Sentence, out.HasN = endOfLineSentence(out.Measured, out.Typed, loc)
	note := ""
	if out.Applies && out.HasN {
		note = out.Sentence
	}
	e.endOfLine.mu.Lock()
	if e.endOfLine.seen == nil {
		e.endOfLine.seen = map[int64]endOfLineSeen{}
	}
	e.endOfLine.seen[processID] = endOfLineSeen{note: note, at: time.Now()}
	e.endOfLine.mu.Unlock()
	return out, nil
}

// endOfLineNote is the station tile's sentence for a process: the last one
// computed within endOfLineFresh, else computed now; "" when it carries no N
// or the process counts nothing at the end of its line.
func (e *Engine) endOfLineNote(processID int64) string {
	e.endOfLine.mu.Lock()
	s, ok := e.endOfLine.seen[processID]
	e.endOfLine.mu.Unlock()
	if ok && time.Since(s.at) < endOfLineFresh {
		return s.note
	}
	if _, err := e.EndOfLine(processID); err != nil {
		e.logFn("end of the line for process %d: %v", processID, err)
		return ""
	}
	e.endOfLine.mu.Lock()
	defer e.endOfLine.mu.Unlock()
	return e.endOfLine.seen[processID].note
}

// countsAtTheEnd reports whether the process counts on a production counter
// at a position its active style consumes at and no station counter counts.
func (e *Engine) countsAtTheEnd(proc *processes.Process) (bool, error) {
	if proc.CounterPLCName == "" || proc.CounterTagName == "" || proc.ActiveStyleID == nil {
		return false, nil
	}
	nodes, err := e.db.ListProcessNodesByProcess(proc.ID)
	if err != nil {
		return false, err
	}
	claims, err := e.db.ListStyleNodeClaims(*proc.ActiveStyleID)
	if err != nil {
		return false, err
	}
	consume := map[string]bool{}
	for _, c := range claims {
		if c.Role == protocol.ClaimRoleConsume && !c.IsLoaderNode() {
			consume[c.CoreNodeName] = true
		}
	}
	for _, n := range nodes {
		if n.LineCounterID == nil && consume[n.CoreNodeName] {
			return true, nil
		}
	}
	return false, nil
}

// runOuts measures the process's completed changeovers' run-outs, by the part
// identity set of the style each left (a retired one read on its own).
func (e *Engine) runOuts(processID int64) ([]RunOut, error) {
	cos, err := e.db.ListProcessChangeovers(processID)
	if err != nil {
		return nil, err
	}
	sets := map[int64]map[string]struct{}{}
	for _, st := range e.processStyleCATIDSets(processID) {
		sets[st.ID] = st.Set
	}
	var landings []counters.Landing
	completed := map[int64]time.Time{}
	for _, c := range cos {
		if c.State != domain.ChangeoverCompleted || c.CompletedAt == nil || c.FromStyleID == nil || *c.FromStyleID == c.ToStyleID {
			continue
		}
		set, ok := sets[*c.FromStyleID]
		if !ok {
			if st, err := e.db.GetStyle(*c.FromStyleID); err == nil {
				set = e.styleCATIDSet(st)
			}
			sets[*c.FromStyleID] = set
		}
		var ids []string
		for v := range set {
			if isIdentity(v) {
				ids = append(ids, v)
			}
		}
		sort.Strings(ids)
		landings = append(landings, counters.Landing{ID: c.ID, AtMs: c.CompletedAt.UnixMilli(), CATIDs: ids})
		completed[c.ID] = *c.CompletedAt
	}
	parts, err := e.db.RunOuts(processID, landings)
	if err != nil {
		return nil, err
	}
	out := []RunOut{}
	for id, n := range parts {
		out = append(out, RunOut{ChangeoverID: id, Parts: n, CompletedAt: completed[id]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CompletedAt.Before(out[j].CompletedAt) })
	return out, nil
}

// endOfLineSentence is the sentence (pure): N measured, as the run-outs'
// range with each changeover and its date; else N typed, with who and when;
// else saying neither has one. hasN reports whether it carries an N.
func endOfLineSentence(measured []RunOut, typed processes.LineDepth, loc *time.Location) (sentence string, hasN bool) {
	day := func(t time.Time) string { return t.In(loc).Format("2006-01-02") }
	if len(measured) > 0 {
		lo, hi := measured[0].Parts, measured[0].Parts
		refs := make([]string, 0, len(measured))
		for _, r := range measured {
			lo, hi = min(lo, r.Parts), max(hi, r.Parts)
			refs = append(refs, fmt.Sprintf("#%d on %s", r.ChangeoverID, day(r.CompletedAt)))
		}
		n := fmt.Sprint(lo)
		if hi != lo {
			n = fmt.Sprintf("%d–%d", lo, hi)
		}
		return fmt.Sprintf("%s About %s parts sit between the cart and the counter (measured at changeovers %s).",
			endOfLineWords, n, strings.Join(refs, ", ")), true
	}
	if typed.N != nil {
		who := ""
		if typed.By != "" {
			who = " by " + typed.By
		}
		on := ""
		if typed.At != nil {
			on = " on " + day(*typed.At)
		}
		return fmt.Sprintf("%s About %d parts sit between the cart and the counter (typed%s%s).", endOfLineWords, *typed.N, who, on), true
	}
	return endOfLineWords + " How many parts sit between the cart and the counter is not measured yet (no changeover " +
		"has landed before the counter's reset) and none is typed.", false
}
