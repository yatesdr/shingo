package engine

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"shingoedge/domain"
)

// Auto-arm stability guard (B1). Before the monitor auto-STARTS a changeover on a
// CATID change it requires the new value to be STABLE — held across at least
// autoArmStableReads consecutive confirmed (debounced) reads AND for at least
// autoArmStableWindow — on top of the value-change debounce. Any flicker, zero,
// unreadable read, or double-flip resets the tracker so it can never arm inside the
// window. Stronger than the single plcEdgeDebounce confirm the prompt/guard use.
const (
	autoArmStableReads  = 3
	autoArmStableWindow = 60 * time.Second
)

// autoCutoverIntent is the deferred auto-cutover decision collected under cm.mu
// during a tick and executed AFTER the lock releases — completeCutover does
// gate/DB/Core work that must never run under the tick lock.
type autoCutoverIntent struct {
	processID    int64
	processName  string
	changeoverID int64
	catid        string
}

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
// This runs for EVERY process with a counter binding. It used to be contrasted
// with a PLC-driven cutover monitor that ran only for opted-in processes; that
// monitor has been removed (its Changeover_Active tag was never wired at any
// plant), so CATID auto-arm is now the only PLC-driven changeover mechanism.
// Poll interval and debounce come from plc_tag_poll.go.
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
	// plcEdgeDebounce before being confirmed. nil outside a change window.
	pending      *string
	pendingSince *time.Time
	// Auto-arm stability tracker (B1). armCandidate is the debounced value being
	// watched for stability; armCount is how many consecutive ticks it has held;
	// armFirstSeen is when it first appeared; armHandled is set once the arm/no-arm
	// decision has been made for the current candidate, so we neither re-fire nor
	// re-log. Any value change, unreadable/zero read resets it (resetArm).
	armCandidate string
	armCount     int
	armFirstSeen time.Time
	armHandled   bool
}

// resetArm clears the auto-arm stability tracker. Called on any value change,
// unreadable read, or zero/empty value — a blip can never accumulate toward an arm.
func (st *catidState) resetArm() {
	st.armCandidate = ""
	st.armCount = 0
	st.armFirstSeen = time.Time{}
	st.armHandled = false
}

// postCutoverVerify is an open post-cutover verification watch for one process:
// within [now, deadline] after a cutover completed, the monitor checks whether
// the press's live part id is one of the new active style's parts, and flags
// changeoverID when it is not. The style's part-identity set is recomputed at
// check time (derived-set model), so styleID is stored rather than a snapshot.
type postCutoverVerify struct {
	changeoverID int64
	styleID      int64 // the new active style (the changeover's to-style)
	deadline     time.Time
}

type catidMonitor struct {
	eng    *Engine
	states map[int64]*catidState // processID → state
	// verify holds the open post-cutover verification watch per process (nil until
	// the first cutover opens one). Guarded by mu, like states.
	verify map[int64]*postCutoverVerify
	mu     sync.Mutex
	// completeCutover is the seam for the auto-cutover fire, defaulting to
	// eng.completeCutover (wired in startCatidMonitor); tests inject a spy.
	// Invoked OUTSIDE cm.mu — gate/DB/Core work must not run under the tick lock.
	completeCutover func(processID int64, triggeredBy string) error
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
	cm.completeCutover = e.completeCutover
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
	ticker := time.NewTicker(plcPollInterval)
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
	var pending []autoCutoverIntent
	cm.mu.Lock()
	for processID, st := range cm.states {
		if intent := cm.evaluateProcess(processID, st, now); intent != nil {
			pending = append(pending, *intent)
		}
	}
	cm.mu.Unlock()
	// Fire auto-arms AFTER releasing cm.mu. StartProcessChangeover does planning,
	// DB writes, robot aborts and a Core preflight; running it under the tick lock
	// would block the guard's liveCATID reads and risk lock-ordering trouble with
	// the changeover path. The intent was fully decided under the lock.
	for _, in := range pending {
		cm.fireAutoCutover(in)
	}
}

// evaluateProcess reads the current CATID value from the WarLink cache, runs the
// value-change debounce (onConfirmedCATID on a confirm edge — A5 alert + the
// prompt-mode arm), then runs the auto-arm stability tracker EVERY tick (the
// 3-reads/60s window is measured across ticks). Returns a non-nil autoArmIntent
// when a process has met the full auto-arm guard this tick; the caller fires it
// after the lock releases. Called with cm.mu held.
func (cm *catidMonitor) evaluateProcess(processID int64, st *catidState, now time.Time) *autoCutoverIntent {
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
	cm.checkPostCutoverVerify(processID, st, now)
	return cm.evaluateAutoArm(processID, st, ok, now)
}

// openPostCutoverVerify starts a post-cutover verification watch for a process:
// within [now, deadline], the monitor checks whether the press's live part id
// matches the new active style's expected_catid and flags changeoverID if it
// does not. Called from the completion path (outside the tick); locks mu.
func (cm *catidMonitor) openPostCutoverVerify(processID, changeoverID, styleID int64, deadline time.Time) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.verify == nil {
		cm.verify = map[int64]*postCutoverVerify{}
	}
	cm.verify[processID] = &postCutoverVerify{
		changeoverID: changeoverID,
		styleID:      styleID,
		deadline:     deadline,
	}
}

// checkPostCutoverVerify runs the open verification watch each tick. As soon as
// the live part id matches the new style it clears the watch (verified). Once the
// window elapses with a live value that still disagrees, it flags the changeover
// for operator confirmation and emits EventChangeoverVerifyMismatch. Called with
// cm.mu held; one decision per cutover, then the watch closes.
func (cm *catidMonitor) checkPostCutoverVerify(processID int64, st *catidState, now time.Time) {
	pv, ok := cm.verify[processID]
	if !ok {
		return
	}
	// The new style's part-identity set, recomputed live (a side of a two-part
	// style is a valid post-cutover reading).
	var set map[string]struct{}
	if style, err := cm.eng.db.GetStyle(pv.styleID); err == nil && style != nil {
		set = cm.eng.styleCATIDSet(style)
	}
	// Success short-circuit: the moment the live part is one of the new style's
	// parts, the cutover is verified — clear any flag and close the watch.
	if st.seenValue && catidSetHas(set, st.lastConfirmed) {
		if err := cm.eng.db.SetChangeoverVerifyMismatch(pv.changeoverID, ""); err != nil {
			log.Printf("plc-catid: clear post-cutover flag on changeover %d: %v", pv.changeoverID, err)
		}
		delete(cm.verify, processID)
		return
	}
	if now.Before(pv.deadline) {
		return // still inside the window — give the press time to settle
	}
	// Window elapsed. If we saw a live value, the style has parts to check against,
	// and the value is none of them, flag the changeover; otherwise close silently.
	if st.seenValue && len(set) > 0 && !catidSetHas(set, st.lastConfirmed) {
		if err := cm.eng.db.SetChangeoverVerifyMismatch(pv.changeoverID, st.lastConfirmed); err != nil {
			log.Printf("plc-catid: flag post-cutover mismatch on changeover %d: %v", pv.changeoverID, err)
		} else {
			log.Printf("CATID post-cutover mismatch: press %s (process %d) reports %q after cutover to style %d (parts {%s}) — flagged for operator confirmation",
				st.processName, processID, st.lastConfirmed, pv.styleID, formatCATIDSet(set))
			cm.eng.Events.Emit(Event{Type: EventChangeoverVerifyMismatch, Payload: CATIDVerifyMismatchEvent{
				ProcessID:     processID,
				ProcessName:   st.processName,
				ChangeoverID:  pv.changeoverID,
				StyleID:       pv.styleID,
				ExpectedCATID: formatCATIDSet(set),
				LiveCATID:     st.lastConfirmed,
			}})
		}
	}
	delete(cm.verify, processID)
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
	if now.Sub(*st.pendingSince) >= plcEdgeDebounce {
		st.lastConfirmed = cur
		st.pending = nil
		st.pendingSince = nil
		return true, true
	}
	return false, false
}

// onConfirmedCATID handles a newly-confirmed part-identity value for a process.
// Always raises EventCATIDMismatch when the live part diverges from the active
// style (A5 — independent of the auto-arm mode). On an actual CHANGE it consults
// the process's changeover_auto_arm mode: `prompt` raises the operator prompt now
// (round-2 behavior); `auto` defers to the stability tracker (evaluateAutoArm),
// which arms after the settle window — no prompt; `off` is silent. Called with
// cm.mu held; keep DB/event work light.
func (cm *catidMonitor) onConfirmedCATID(processID int64, st *catidState, isChange bool) {
	styleID, styleName, set, ok := cm.eng.activeStyleCATIDSet(processID)
	// A5: alert when the active style has configured/derivable CATIDs and the live
	// part is NONE of them (not just != a single value — a two-position style runs
	// two parts, and either side is fine). Empty set = inert (never alert).
	if ok && len(set) > 0 && !catidSetHas(set, st.lastConfirmed) {
		log.Printf("CATID mismatch: press %s (process %d) live CATID %q not among active style %q parts {%s} — outgoing-style relief is blocked",
			st.processName, processID, st.lastConfirmed, styleName, formatCATIDSet(set))
		cm.eng.Events.Emit(Event{Type: EventCATIDMismatch, Payload: CATIDMismatchEvent{
			ProcessID:     processID,
			ProcessName:   st.processName,
			PLCName:       st.plcName,
			StyleID:       styleID,
			StyleName:     styleName,
			LiveCATID:     st.lastConfirmed,
			ExpectedCATID: formatCATIDSet(set),
		}})
	}
	// B1: on an actual CHANGE of the physical part (not the first-read baseline),
	// prompt ONLY when the process is in prompt mode. auto is handled by the
	// stability tracker; off is silent.
	if isChange && cm.eng.processChangeoverAutoArm(processID) == domain.ChangeoverAutoArmPrompt {
		cm.raiseCATIDChangePrompt(processID, st)
	}
}

// raiseCATIDChangePrompt emits EventCATIDChangePrompt after the press's part
// changed, pre-filling the target style when the new CATID maps to a known
// style's expected_catid. B1 PROMPT half only: it never starts the changeover —
// the operator confirms through the existing Start Changeover flow. Called with
// cm.mu held.
//
// Suppressed when the new part matches the already-active style's expected_catid
// (the line is now correct for the running style — a completed cutover, not a
// pending one), so a change that RESOLVES a mismatch does not nag the operator
// to change to the style they are already running.
func (cm *catidMonitor) raiseCATIDChangePrompt(processID int64, st *catidState) {
	newCATID := st.lastConfirmed
	// Suppress when the new part already belongs to the active style's set (a
	// completed cutover, not a pending one — the line is already correct).
	if _, _, activeSet, ok := cm.eng.activeStyleCATIDSet(processID); ok && catidSetHas(activeSet, newCATID) {
		return
	}
	cm.emitCATIDPrompt(processID, st, newCATID, cm.eng.stylesForCATID(processID, newCATID))
}

// emitCATIDPrompt raises EventCATIDChangePrompt for a part change, carrying every
// candidate style the new part maps to: exactly one pre-fills the target; more
// than one leaves the operator to pick (naming them); none prompts without a
// target. Shared by the prompt-mode confirm edge and the auto-arm ambiguity
// fallback. Called with cm.mu held.
func (cm *catidMonitor) emitCATIDPrompt(processID int64, st *catidState, newCATID string, matches []styleCATIDMatch) {
	cands := make([]CATIDCandidate, len(matches))
	for i, m := range matches {
		cands[i] = CATIDCandidate{StyleID: m.ID, StyleName: m.Name}
	}
	hasTarget := len(matches) == 1
	var targetID int64
	var targetName string
	if hasTarget {
		targetID, targetName = matches[0].ID, matches[0].Name
	}
	log.Printf("CATID change: press %s (process %d) part is now CATID %q — %d candidate style(s): %s",
		st.processName, processID, newCATID, len(matches), strings.Join(matchNames(matches), ", "))
	cm.eng.Events.Emit(Event{Type: EventCATIDChangePrompt, Payload: CATIDChangePromptEvent{
		ProcessID:       processID,
		ProcessName:     st.processName,
		PLCName:         st.plcName,
		NewCATID:        newCATID,
		HasTarget:       hasTarget,
		TargetStyleID:   targetID,
		TargetStyleName: targetName,
		Candidates:      cands,
	}})
}

// evaluateAutoArm runs the B1 auto-arm stability tracker on the debounced value,
// once per tick. It returns a non-nil intent ONLY when the value has been stable
// long enough (>= autoArmStableReads consecutive confirmed reads AND >=
// autoArmStableWindow) and the full arm guard passes — otherwise nil. Any change
// of value, unreadable read, or zero resets the tracker so a flicker/double-flip
// can never arm inside the window. Called with cm.mu held; the returned intent is
// fired after the lock releases.
func (cm *catidMonitor) evaluateAutoArm(processID int64, st *catidState, ok bool, now time.Time) *autoCutoverIntent {
	// Unreadable, never-seen, or zero/empty value: reset and stay silent. A blip can
	// never accumulate toward an arm.
	if !ok || !st.seenValue || st.lastConfirmed == "" || st.lastConfirmed == "0" {
		st.resetArm()
		return nil
	}
	v := st.lastConfirmed
	// ANY change of the debounced value restarts the count and the window — this is
	// what makes a flicker (reconfirmed new value) or a double-flip A→B→A (landing
	// on a value other than the one being counted) unable to arm mid-window.
	if st.armCandidate != v {
		st.armCandidate = v
		st.armCount = 1
		st.armFirstSeen = now
		st.armHandled = false
		return nil
	}
	st.armCount++
	if st.armHandled {
		return nil // decision already made for this stable candidate
	}
	// Require BOTH >=3 consecutive confirmed reads AND >=60s since first seen.
	if st.armCount < autoArmStableReads || now.Sub(st.armFirstSeen) < autoArmStableWindow {
		return nil
	}
	st.armHandled = true // decide (arm or skip) exactly once per stable candidate
	return cm.decideAutoArm(processID, st, v)
}

// decideAutoArm applies the remaining auto-arm conditions once a value is stable:
// the process must be in `auto` mode (2), the value must map to a configured style
// (3), that style must differ from the active one (4), and no changeover may
// already be in progress (5). Any failure returns nil (silent, at most one debug
// line — armHandled prevents repeats). Called with cm.mu held; does light DB reads
// only, never StartProcessChangeover.
func (cm *catidMonitor) decideAutoArm(processID int64, st *catidState, v string) *autoCutoverIntent {
	// Mode gate: only `auto` processes auto-cutover. prompt/off are handled on the
	// confirm edge (prompt) or silently (off).
	if cm.eng.processChangeoverAutoArm(processID) != domain.ChangeoverAutoArmAuto {
		return nil
	}
	// If the live part is still one of the ACTIVE style's parts, the press has not
	// left the outgoing part yet — there is nothing to cut over to.
	if _, _, activeSet, okActive := cm.eng.activeStyleCATIDSet(processID); okActive && catidSetHas(activeSet, v) {
		return nil
	}
	// A changeover MUST already be in progress. This condition used to be
	// inverted — the monitor skipped when one was running and STARTED one when
	// none was, which is how HK 2026-07-28 put four robots on the floor chasing a
	// style with zero stock.
	//
	// The problem this feature exists for is the opposite: the operator starts a
	// changeover, the material moves, and then they forget to press CUTOVER, so
	// the press stamps the new part while shingo keeps attributing it to the old
	// style for hours. A stable CATID that has left the active style's part set IS
	// the evidence the press already cut over. Press the button for them.
	//
	// We deliberately do NOT check that the live part maps to this changeover's
	// to-style. If the press was set up for some third style, completing is still
	// correct — the changeover physically did what it was told, and leaving it
	// open would block the corrective changeover the operator now needs.
	// finalizeChangeoverRow opens the post-cutover verification watch, which
	// compares the live part against the NEW active style and raises the operator
	// flag (with a one-tap corrective changeover) when they disagree. That path is
	// purpose-built for exactly this and already tested; duplicating the check
	// here would only decide worse, earlier.
	changeover, err := cm.eng.db.GetActiveProcessChangeover(processID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// A real read failure, not the everyday "no active changeover". Say so and
		// stay put: an unknown changeover state is not a licence to cut over.
		log.Printf("plc-catid: cannot read active changeover for process %d: %v — not cutting over", processID, err)
		return nil
	}
	if err != nil || changeover == nil {
		// No changeover in progress. Never start one — starting dispatches robots,
		// and nothing here knows whether the material for it exists.
		return nil
	}
	return &autoCutoverIntent{
		processID:    processID,
		processName:  st.processName,
		changeoverID: changeover.ID,
		catid:        v,
	}
}

// fireAutoArm executes a collected auto-arm intent OUTSIDE cm.mu: it starts the
// changeover to the mapped style, writes the operator-readable audit line, and
// raises the station notification. A failed start logs and returns without
// notifying (nothing was armed). Never cancels or re-plans anything.
func (cm *catidMonitor) fireAutoCutover(in autoCutoverIntent) {
	complete := cm.completeCutover
	if complete == nil {
		complete = cm.eng.completeCutover
	}
	// completeCutover runs canCompleteChangeover first, which refuses while any
	// node task is non-terminal or an order is still placing a bin at a
	// participant node. That gate IS the "has the material actually landed"
	// check — an error here is the normal "not finished yet" answer on most
	// ticks, not a fault, so it stays a log line and the next tick retries.
	if err := complete(in.processID, "auto-catid"); err != nil {
		log.Printf("plc-catid: auto-cutover for process %d (changeover %d, CATID %s) not completed: %v",
			in.processID, in.changeoverID, in.catid, err)
		return
	}
	log.Printf("auto-cutover: completed changeover %d at %s from CATID %s — the press had already cut over",
		in.changeoverID, in.processName, in.catid)
	cm.eng.Events.Emit(Event{Type: EventCATIDAutoCutover, Payload: CATIDAutoCutoverEvent{
		ProcessID:    in.processID,
		ProcessName:  in.processName,
		ChangeoverID: in.changeoverID,
		NewCATID:     in.catid,
	}})
}

// processChangeoverAutoArm returns the process's normalized CATID auto-arm mode
// (auto|prompt|off).
//
// A missing row or read error ⇒ OFF. This used to return auto on the reasoning
// that auto is "inert without an expected_catid match anyway" — but that only
// holds when there is no match, and the case that matters is precisely the one
// where there IS a match and we could not read the operator's stated preference.
// Failing open means a transient SQLite error arms an unattended changeover. A
// process whose mode we cannot read has not opted in to anything, so the honest
// answer is to stay silent until the read succeeds.
func (e *Engine) processChangeoverAutoArm(processID int64) string {
	proc, err := e.db.GetProcess(processID)
	if err != nil || proc == nil {
		log.Printf("plc-catid: cannot read auto-arm mode for process %d (%v) — treating as off", processID, err)
		return domain.ChangeoverAutoArmOff
	}
	return domain.NormalizeChangeoverAutoArm(proc.ChangeoverAutoArm)
}

// (changeoverInProgress removed: the auto path needs the changeover's ID to log
// and report against, so it reads the row directly in decideAutoArm. A bool no
// longer carries enough.)

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
