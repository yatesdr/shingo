package engine

import (
	"context"
	"log"
	"sync"
	"time"
)

// PLC-driven changeover-completion monitor.
//
// Subscribes to each enabled process's Changeover_Active tag (derived
// from counter_tag_name's parent struct via deriveCutoverTag). When the
// tag falls 1→0 and stays at 0 for cutoverDebounce, fires
// CompleteProcessProductionCutover for that process — the operator
// changing style at the PLC HMI is authoritative for "production
// resumed on new style", so shingo follows.
//
// The Theme B canCompleteChangeover gate is the safety net: a spurious
// PLC trigger (PLC restart, fault recovery) lands on the gate, which
// blocks if tasks/orders aren't terminal — no state damage. The 2s
// debounce filters out flicker on top of that.

const (
	// cutoverPollInterval is how often the monitor reads each enabled
	// process's tag value from the WarLink cache. Cache freshness is
	// driven by WarLink's own SSE/poll cadence; this interval just
	// controls how quickly we react to a value change shingo already
	// knows about.
	cutoverPollInterval = 500 * time.Millisecond

	// cutoverDebounce is the time the tag must remain at 0 after a
	// 1→0 edge before the monitor fires. PLC restarts and fault-
	// recovery sequences can toggle the bit briefly; this guards
	// against single-tick flicker triggering a spurious cutover.
	cutoverDebounce = 2 * time.Second
)

// cutoverProcessState tracks the falling-edge debounce per process.
type cutoverProcessState struct {
	plcName     string
	tagName     string
	lastValue   int64
	seenValue   bool       // false until the first successful read so we don't false-edge on startup
	pendingFall *time.Time // non-nil while the 2s debounce window is open
}

type cutoverMonitor struct {
	eng    *Engine
	states map[int64]*cutoverProcessState // processID → state
	mu     sync.Mutex
}

// startCutoverMonitor primes the per-process state from the database
// (auto_cutover_enabled processes only), enables WarLink publishing on
// each derived tag, and spawns the polling goroutine. Safe to call
// even when no processes have auto-cutover enabled — the goroutine
// just polls an empty state map.
func (e *Engine) startCutoverMonitor() {
	if e.plcMgr == nil {
		return
	}
	cm := &cutoverMonitor{eng: e, states: map[int64]*cutoverProcessState{}}
	cm.prime()
	go cm.run()
}

// prime enumerates enabled processes, derives the cutover tag, enables
// WarLink publishing, and stores per-process state. Processes whose
// plant doesn't match the MES struct convention (deriveCutoverTag
// returns empty) are logged and skipped — the engine boots cleanly,
// and operators can either fix the counter binding or disable auto-
// cutover for that process.
func (cm *cutoverMonitor) prime() {
	procs, err := cm.eng.db.ListProcesses()
	if err != nil {
		log.Printf("plc-cutover: list processes: %v", err)
		return
	}
	for _, p := range procs {
		if !p.AutoCutoverEnabled {
			continue
		}
		if p.CounterPLCName == "" || p.CounterTagName == "" {
			log.Printf("plc-cutover: process %d (%s) has auto-cutover enabled but no counter binding — skipping", p.ID, p.Name)
			continue
		}
		derived := deriveCutoverTag(p.CounterTagName)
		if derived == "" {
			log.Printf("plc-cutover: process %d (%s) counter tag %q has no parent struct — skipping (plant does not match MES_*.<leaf> convention)", p.ID, p.Name, p.CounterTagName)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := cm.eng.plcMgr.EnableTagPublishing(ctx, p.CounterPLCName, derived); err != nil {
			log.Printf("plc-cutover: enable publish for %s.%s: %v", p.CounterPLCName, derived, err)
			cancel()
			continue
		}
		cancel()
		log.Printf("plc-cutover: monitoring process %d (%s) on %s.%s", p.ID, p.Name, p.CounterPLCName, derived)
		cm.states[p.ID] = &cutoverProcessState{plcName: p.CounterPLCName, tagName: derived}
	}
}

func (cm *cutoverMonitor) run() {
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

func (cm *cutoverMonitor) tick(now time.Time) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for processID, st := range cm.states {
		cm.evaluateProcess(processID, st, now)
	}
}

// evaluateProcess reads the current value from the WarLink cache and
// delegates to applyEdge for the state-machine update. Fires the
// cutover when applyEdge confirms a falling edge.
func (cm *cutoverMonitor) evaluateProcess(processID int64, st *cutoverProcessState, now time.Time) {
	raw, err := cm.eng.plcMgr.ReadTag(st.plcName, st.tagName)
	var cur int64
	ok := err == nil
	if ok {
		cur, ok = plcTagInt64(raw)
		if !ok {
			log.Printf("plc-cutover: unexpected value type %T for %s.%s — skipping", raw, st.plcName, st.tagName)
		}
	}
	if applyEdge(st, cur, ok, now) {
		log.Printf("plc-cutover: confirmed falling edge on %s.%s — firing cutover for process %d", st.plcName, st.tagName, processID)
		if err := cm.eng.CompleteProcessProductionCutoverFromPLC(processID); err != nil {
			log.Printf("plc-cutover: cutover for process %d: %v", processID, err)
		}
	}
}

// applyEdge runs the falling-edge debounce state machine. Returns
// true when the caller should fire the cutover.
//
// Inputs:
//
//	cur — current tag value (only consulted when ok=true)
//	ok  — false signals an unreadable / stale value (PLC disconnect,
//	      tag missing, unexpected type); resets edge tracking so the
//	      next valid reading becomes a fresh baseline rather than
//	      firing a spurious 1→0 edge against stale state.
//
// Pure mutation on st; no I/O. Extracted for testability — the
// state-machine logic is the load-bearing piece of the monitor and
// the tests script value sequences (rising, falling, flicker within
// the debounce window) against this function directly.
func applyEdge(st *cutoverProcessState, cur int64, ok bool, now time.Time) bool {
	if !ok {
		st.seenValue = false
		st.pendingFall = nil
		return false
	}
	if !st.seenValue {
		st.lastValue = cur
		st.seenValue = true
		st.pendingFall = nil
		return false
	}
	fire := false
	if st.pendingFall == nil {
		// Looking for the 1→0 transition.
		if st.lastValue > 0 && cur == 0 {
			t := now
			st.pendingFall = &t
		}
	} else {
		// Inside the 2s debounce window. Either confirm (still 0) or cancel (rebounded).
		if cur != 0 {
			st.pendingFall = nil
		} else if now.Sub(*st.pendingFall) >= cutoverDebounce {
			fire = true
			st.pendingFall = nil
		}
	}
	st.lastValue = cur
	return fire
}

// plcTagInt64 coerces a WarLink tag value (interface{}) to int64.
// WarLink delivers numeric tag values as float64 over JSON; PLC
// integer types come through that path. Falls back to int / int32 /
// int64 / float32 for completeness.
func plcTagInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float32:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}
