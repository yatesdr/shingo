package engine

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PLC part-identity (CATID) monitor — the A5 guard's data source.
//
// Subscribes to each counter-bound process's CATID_01 tag (derived from
// counter_tag_name's parent struct via deriveIdentityTag) and tracks the live,
// debounced part-identity value per process. Two jobs:
//
//   - It is the source the request-path guard (guardCatidMismatch) reads: the
//     guard blocks outgoing-style relief when the live CATID diverges from the
//     active style's expected_catid. The monitor owns the debounce so the guard
//     never blocks on flicker.
//   - On a confirmed value it raises EventCATIDMismatch when the live part does
//     not match the active style — the ground-truth "wrong part on the press"
//     alert (Hopkinsville 2026-07-23). (Commit 3 also emits a prompt-arm event
//     on a value CHANGE; see raiseCATIDChangePrompt.)
//
// Unlike the cutover monitor, this runs for EVERY process with a counter
// binding — NOT only auto-cutover-enabled ones. The part-identity guard is an
// independent safety feature; coupling it to auto-cutover would leave it inert
// on lines that have not opted into PLC-driven completion. Reuses the cutover
// monitor's poll interval and debounce so it reads/debounces "the same way".
//
// The whole monitor is inert when no process yields a CATID tag (plant does not
// publish the MES struct) — reads fail, the guard stays fail-open, no alert.

// catidState tracks the value-change debounce for one process.
type catidState struct {
	plcName     string
	tagName     string
	processName string
	// lastConfirmed is the debounced part-identity value the guard reads.
	lastConfirmed string
	seenValue     bool // false until the first successful read
	// pending / pendingSince hold a candidate new value while it settles for
	// cutoverDebounce before being confirmed. nil outside a change window.
	pending      *string
	pendingSince *time.Time
}

type catidMonitor struct {
	eng    *Engine
	states map[int64]*catidState // processID → state
	mu     sync.Mutex
}

// startCatidMonitor primes per-process state from the database (every
// counter-bound process whose CATID tag can be derived), enables WarLink
// publishing on each CATID tag, stores the monitor on the engine so the guard
// can query it, and spawns the polling goroutine. Safe to call with no eligible
// processes — the goroutine just polls an empty map.
func (e *Engine) startCatidMonitor() {
	if e.plcMgr == nil {
		return
	}
	cm := &catidMonitor{eng: e, states: map[int64]*catidState{}}
	cm.prime()
	e.catidMon = cm
	go cm.run()
}

// prime enumerates counter-bound processes, derives the CATID tag, enables
// WarLink publishing, and stores per-process state. Processes with no counter
// binding or whose plant doesn't match the MES struct convention
// (deriveIdentityTag returns empty) are skipped — the guard simply stays inert
// for them.
func (cm *catidMonitor) prime() {
	procs, err := cm.eng.db.ListProcesses()
	if err != nil {
		log.Printf("plc-catid: list processes: %v", err)
		return
	}
	for _, p := range procs {
		if p.CounterPLCName == "" || p.CounterTagName == "" {
			continue
		}
		derived := deriveIdentityTag(p.CounterTagName)
		if derived == "" {
			log.Printf("plc-catid: process %d (%s) counter tag %q has no parent struct — skipping (plant does not match MES_*.<leaf> convention)", p.ID, p.Name, p.CounterTagName)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := cm.eng.plcMgr.EnableTagPublishing(ctx, p.CounterPLCName, derived); err != nil {
			log.Printf("plc-catid: enable publish for %s.%s: %v", p.CounterPLCName, derived, err)
			cancel()
			continue
		}
		cancel()
		log.Printf("plc-catid: monitoring process %d (%s) on %s.%s", p.ID, p.Name, p.CounterPLCName, derived)
		cm.states[p.ID] = &catidState{plcName: p.CounterPLCName, tagName: derived, processName: p.Name}
	}
}

func (cm *catidMonitor) run() {
	ticker := time.NewTicker(cutoverPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-cm.eng.stopChan:
			return
		case <-ticker.C:
			cm.tick(time.Now())
		}
	}
}

func (cm *catidMonitor) tick(now time.Time) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for processID, st := range cm.states {
		cm.evaluateProcess(processID, st, now)
	}
}

// evaluateProcess reads the current CATID value from the WarLink cache and runs
// the value-change debounce. When applyCatidEdge confirms a value it delegates
// to onConfirmedCATID. Called with cm.mu held.
func (cm *catidMonitor) evaluateProcess(processID int64, st *catidState, now time.Time) {
	raw, err := cm.eng.plcMgr.ReadTag(st.plcName, st.tagName)
	var cur string
	ok := err == nil
	if ok {
		cur, ok = catidToString(raw)
		if !ok {
			log.Printf("plc-catid: unexpected value type %T for %s.%s — skipping", raw, st.plcName, st.tagName)
		}
	}
	confirmed, isChange := applyCatidEdge(st, cur, ok, now)
	if confirmed {
		cm.onConfirmedCATID(processID, st, isChange)
	}
}

// applyCatidEdge runs the value-change debounce state machine. Returns
// (confirmed, isChange): confirmed is true when st.lastConfirmed was just
// (re)established; isChange is true only when that confirmation replaced a
// different previously-confirmed value (i.e. the part actually changed), and
// false for the first-read baseline.
//
// Inputs:
//
//	cur — current CATID value (only consulted when ok=true)
//	ok  — false signals an unreadable value (PLC disconnect, tag missing,
//	      unexpected type). Unlike the cutover monitor, a read failure does
//	      NOT erase the confirmed baseline: the last known part identity is
//	      still our best knowledge and the guard should keep using it. It only
//	      cancels an in-flight change window so a blip can't confirm a value.
//
// Pure mutation on st; no I/O. Extracted for testability — tests script value
// sequences (baseline, change, flicker within the window) against it directly.
func applyCatidEdge(st *catidState, cur string, ok bool, now time.Time) (confirmed, isChange bool) {
	if !ok {
		st.pending = nil
		st.pendingSince = nil
		return false, false
	}
	if !st.seenValue {
		st.lastConfirmed = cur
		st.seenValue = true
		st.pending = nil
		st.pendingSince = nil
		return true, false // baseline, not a change
	}
	if cur == st.lastConfirmed {
		// No divergence (or the candidate rebounded to the confirmed value).
		st.pending = nil
		st.pendingSince = nil
		return false, false
	}
	// cur differs from the confirmed value → candidate change.
	if st.pending == nil || *st.pending != cur {
		c := cur
		t := now
		st.pending = &c
		st.pendingSince = &t
		return false, false
	}
	// Same candidate still present — confirm once it has settled.
	if now.Sub(*st.pendingSince) >= cutoverDebounce {
		st.lastConfirmed = cur
		st.pending = nil
		st.pendingSince = nil
		return true, true
	}
	return false, false
}

// onConfirmedCATID handles a newly-confirmed part-identity value for a process.
// Commit 2: raise EventCATIDMismatch when the live part does not match the
// active style. (Commit 3 extends this to also prompt a changeover on a change.)
// Called with cm.mu held; keep DB/event work light.
func (cm *catidMonitor) onConfirmedCATID(processID int64, st *catidState, isChange bool) {
	styleID, styleName, expected, ok := cm.eng.activeStyleCATID(processID)
	// A5: alert only when the active style HAS an expected value (configured)
	// and the live part diverges from it. Empty expected = inert (never alert).
	if ok && expected != "" && st.lastConfirmed != expected {
		log.Printf("CATID mismatch: press %s (process %d) live CATID %q != active style %q expected %q — outgoing-style relief is blocked",
			st.processName, processID, st.lastConfirmed, styleName, expected)
		cm.eng.Events.Emit(Event{Type: EventCATIDMismatch, Payload: CATIDMismatchEvent{
			ProcessID:     processID,
			ProcessName:   st.processName,
			PLCName:       st.plcName,
			StyleID:       styleID,
			StyleName:     styleName,
			LiveCATID:     st.lastConfirmed,
			ExpectedCATID: expected,
		}})
	}
	_ = isChange // Commit 3 uses this for the B1 prompt-arm.
}

// liveCATID returns the debounced part-identity value for a process, or
// ("", false) when nothing has been observed yet (startup, unreadable tag,
// process not monitored). The guard reads this; ok=false ⇒ guard stays inert.
func (cm *catidMonitor) liveCATID(processID int64) (string, bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	st, ok := cm.states[processID]
	if !ok || !st.seenValue {
		return "", false
	}
	return st.lastConfirmed, true
}

// activeStyleCATID resolves a process's active style id, name, and
// expected_catid. ok=false when the process has no active style set or the
// lookups fail (treated as "nothing to compare against" by callers).
func (e *Engine) activeStyleCATID(processID int64) (styleID int64, styleName, expectedCATID string, ok bool) {
	proc, err := e.db.GetProcess(processID)
	if err != nil || proc == nil || proc.ActiveStyleID == nil {
		return 0, "", "", false
	}
	style, err := e.db.GetStyle(*proc.ActiveStyleID)
	if err != nil || style == nil {
		return 0, "", "", false
	}
	return style.ID, style.Name, style.ExpectedCATID, true
}

// catidToString normalizes a WarLink CATID tag value to a comparison string.
// CATID is an integer part id at Hopkinsville (e.g. 40016911) delivered as
// float64 over JSON, but a plant could publish it as a string tag — handle
// both so the value compares cleanly against a hand-entered expected_catid.
func catidToString(v any) (string, bool) {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s), true
	}
	if n, ok := plcTagInt64(v); ok {
		return strconv.FormatInt(n, 10), true
	}
	return "", false
}
