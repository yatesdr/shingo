package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shingo/protocol/lockcheck"
	"shingoedge/domain"
	"shingoedge/internal/lockorder"
	"shingoedge/plc"
	"shingoedge/store/processes"
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
	// cutoverRetryInterval is how often a refused auto-cutover is decided again
	// with no poke. A poke re-decides at once.
	cutoverRetryInterval = time.Minute
)

// autoCutoverIntent is the deferred auto-cutover decision collected under cm.mu
// during a tick and executed AFTER the lock releases — completeCutover does
// gate/DB/Core work that must never run under the tick lock.
type autoCutoverIntent struct {
	processID    int64
	processName  string
	changeoverID int64
	catid        string
	// mode is the process's auto-arm mode at the decision. Only auto calls the
	// door; prompt and off log what auto would have done.
	mode string
	// decidedAt is the tick the decision was taken on; a refusal is dated by it.
	decidedAt time.Time
	// force: the counted-part condition holds (forceHolds), so auto completes
	// past a red gate; prompt and off log it.
	force bool
	// switchTo is the automatic switch's style (no changeover open), 0 for a
	// cutover; fromStyleID the style it leaves.
	switchTo, fromStyleID int64
	switchName            string
}

// countedTick is the last counted tick the poll attributed off today (to the
// target, off-plan or held), for the force, the switch and the ambiguity
// prompt.
type countedTick struct {
	catid string
	kind  plc.TickKind
	at    time.Time
}

// catidFacts is what is read of a process's tracker: the counted-part force's
// condition (forceHolds) and the health strip's identity and part items.
type catidFacts struct {
	monitored   bool   // the monitor holds the process
	value       string // the confirmed value; "" until one is confirmed
	confirmedAt time.Time
	baseline    bool // the value was standing at the first read, not a change
	stable      bool // the declaration's stability: 3 reads over 60 s
	tick        countedTick
	lastOK      time.Time // the last good read (the snapshot's)
	lastErr     string    // the last read's failure, "" after a good read
}

// forceHolds is the counted-part force's condition on a changeover started at
// startedAt (pure): the monitor confirmed a CHANGE to a part, after the
// changeover opened, that has been stable for the declaration's window, and a
// counted tick on it has been attributed to the target since. One tick is
// enough. A program already standing when the changeover opened never forces
// at minute zero: every leg just dispatched would become a straggler. started_at
// is stored to the second, so a change confirmed within the start's second
// does not count as after it (the gated declaration still applies).
func forceHolds(f catidFacts, startedAt time.Time) bool {
	return f.stable && !f.baseline && isIdentity(f.value) &&
		!f.confirmedAt.Before(startedAt.Add(time.Second)) &&
		countedSince(f, plc.TickTarget)
}

// countedSince reports whether a tick of kind on the confirmed value has been
// counted since it was confirmed.
func countedSince(f catidFacts, kind plc.TickKind) bool {
	return f.tick.kind == kind && f.tick.catid == f.value && !f.tick.at.Before(f.confirmedAt)
}

// PLC part-identity (CATID) monitor — the A5 guard's data source.
//
// Subscribes to each counter-bound process's CATID_0N tag (derived from
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
	// confirmedAt is when lastConfirmed was confirmed; confirmedBaseline marks
	// the first read after a start, whose value was already standing then.
	// lastErr is the last read's failure, "" after a good read. The health
	// strip reads all three (catidFacts).
	confirmedAt       time.Time
	confirmedBaseline bool
	lastErr           string
	// Auto-arm stability tracker (B1). armCandidate is the debounced value being
	// watched for stability; armCount is how many consecutive ticks it has held;
	// armFirstSeen is when it first appeared. Any value change, unreadable read
	// or value that names no part resets it (resetArm).
	//
	// armDecided is set once a decision has been taken on the stable candidate.
	// It is taken again on a poke (poked: a terminal transition or a changeover
	// start on the process), and, when the door refused it (armRefusedAt),
	// at most once per cutoverRetryInterval. A predicate-false decision has no
	// clock: the pokes cover what could change it.
	armCandidate string
	armCount     int
	armFirstSeen time.Time
	armDecided   bool
	armRefusedAt time.Time
	poked        bool
	// raisedFor is the confirm (its confirmedAt) the switch last answered
	// without switching — the ambiguity prompt, or a line that it would have
	// switched or did not: once per confirmed value, not per poke.
	raisedFor time.Time
}

// raiseOnce reports whether the switch's answer on the confirmed value is yet
// to be raised, and marks it raised.
func (st *catidState) raiseOnce() bool {
	if st.raisedFor.Equal(st.confirmedAt) {
		return false
	}
	st.raisedFor = st.confirmedAt
	return true
}

// stableAt reports whether the confirmed value has held for the declaration's
// window at now: at least autoArmStableReads consecutive reads AND
// autoArmStableWindow since the tracker first saw it.
func (st *catidState) stableAt(now time.Time) bool {
	return st.seenValue && st.armCandidate != "" && st.armCandidate == st.lastConfirmed &&
		st.armCount >= autoArmStableReads && now.Sub(st.armFirstSeen) >= autoArmStableWindow
}

// facts reads the tracker at now; the tick and the last good read are the
// caller's (factsOf). Called with cm.mu held.
func (st *catidState) facts(now time.Time) catidFacts {
	f := catidFacts{monitored: true, confirmedAt: st.confirmedAt, baseline: st.confirmedBaseline,
		stable: st.stableAt(now), lastErr: st.lastErr}
	if st.seenValue {
		f.value = st.lastConfirmed
	}
	return f
}

// resetArm clears the auto-arm stability tracker. Called on any value change,
// unreadable read, or zero/empty value — a blip can never accumulate toward an arm.
func (st *catidState) resetArm() {
	st.armCandidate = ""
	st.armCount = 0
	st.armFirstSeen = time.Time{}
	st.armDecided = false
	st.armRefusedAt = time.Time{}
	st.poked = false
}

// redecide reports whether a stable candidate's decision is to be taken (again)
// at now.
func (st *catidState) redecide(now time.Time) bool {
	if !st.armDecided || st.poked {
		return true
	}
	return !st.armRefusedAt.IsZero() && now.Sub(st.armRefusedAt) >= cutoverRetryInterval
}

// postCutoverVerify is an open post-cutover verification watch for one process:
// within [now, deadline] after a cutover completed, the monitor checks whether
// the press's live part id is one of the new active style's parts, and flags
// changeoverID when it is not. The style's part-identity set is read once, when
// the watch opens: the check runs every 500 ms under cm.mu, which the request
// guard waits on (TestVerify_SetComputedOnceAtOpen).
type postCutoverVerify struct {
	changeoverID int64
	styleID      int64 // the new active style (the changeover's to-style)
	set          map[string]struct{}
	deadline     time.Time
}

// catidSnapshot is the monitor's published view of one process's part identity,
// read without cm.mu: the poll attributes a counted tick by it, and must never
// wait on the monitor. Immutable; a change publishes a new one.
type catidSnapshot struct {
	value       string              // the confirmed CATID; "" until one is confirmed
	confirmedAt time.Time           // when value was confirmed
	lastOK      time.Time           // the last good read of the identity tag
	observed    map[string]struct{} // every value the process has emitted (plc_catid_observations)
	landing     catidLanding        // the process's latest landing; zero until one
}

// catidLanding is a process's latest landing — a changeover completed, or a
// switch — as the snapshot carries it: the style it left and when, on the
// monitor's clock (the one a confirm is dated by).
type catidLanding struct {
	fromStyleID int64
	at          time.Time
}

// runningOut is the style whose part the PLC is still running out: the
// latest landing's from-style while the confirmed value is one confirmed
// before that landing; 0 otherwise.
func (s *catidSnapshot) runningOut() int64 {
	if s.landing.at.IsZero() || !s.confirmedAt.Before(s.landing.at) {
		return 0
	}
	return s.landing.fromStyleID
}

type catidMonitor struct {
	eng    *Engine
	states map[int64]*catidState // processID → state
	// verify holds the open post-cutover verification watch per process (nil until
	// the first cutover opens one). Guarded by mu, like states.
	verify map[int64]*postCutoverVerify
	mu     lockcheck.Mutex[lockorder.CATIDMonitor]
	// snapshots holds each monitored process's *catidSnapshot (processID →
	// snapshot). Written under mu, on each good read and each confirm edge, and
	// swapped by reprime; read with no lock at all.
	snapshots sync.Map
	// countedTicks holds each process's last counted tick placed off today
	// (processID → countedTick), written by the poll with no lock
	// (noteCountedTick) and read by the decisions.
	countedTicks sync.Map
	// slots is the slot watch's last pass (processID → *slotFacts), swapped
	// whole by the watch's own goroutine and read with no lock
	// (catid_slot_watch.go). The tick never touches it.
	slots atomic.Pointer[map[int64]*slotFacts]
	// completeCutover is the seam for the auto-cutover fire, defaulting to
	// eng.completeCutoverLocked (wired in startCatidMonitor); tests inject a
	// spy. Invoked OUTSIDE cm.mu (gate/DB/Core work must not run under the tick
	// lock) and under cutoverMu, which the fire takes to re-read its decision.
	completeCutover func(processID, changeoverID int64, by string, mode CutoverMode) ([]domain.Blocker, error)
}

// startCatidMonitor primes per-process state from the database (every
// counter-bound process whose CATID tag can be derived), enables WarLink
// publishing on each CATID tag, stores the monitor on the engine so the guard
// can query it, and spawns the polling goroutine and the slot watch's. Safe
// to call with no eligible processes — the goroutine just polls an empty map.
func (e *Engine) startCatidMonitor() {
	if e.plcMgr == nil {
		return
	}
	cm := &catidMonitor{eng: e, states: map[int64]*catidState{}}
	cm.completeCutover = e.completeCutoverLocked
	cm.prime()
	e.catidMon = cm
	e.tickMonitor.Store(cm)
	go cm.run()
	go cm.runSlotWatch()
}

// prime enumerates the processes with an identity tag (identityTagOf: none for
// a process with no counter PLC or identity "none", the override when typed,
// else derived from the counter tag), enables WarLink publishing on each (a
// failed toggle is logged and the process is monitored anyway), and stores
// per-process state and its first snapshot with every value the process has
// emitted. Called once, from
// startCatidMonitor before the goroutine starts: it writes cm.states with no
// lock. A process with a counter but no derivable tag is skipped and logged —
// the guard stays inert for it; reprime brings an edited process in later.
func (cm *catidMonitor) prime() {
	procs, err := cm.eng.db.ListProcesses()
	if err != nil {
		log.Printf("plc-catid: list processes: %v", err)
		return
	}
	observed := cm.observedByProcess()
	for i := range procs {
		p := &procs[i]
		tag := identityTagOf(p)
		if tag == "" {
			if p.CounterPLCName != "" && p.CounterTagName != "" && !p.IdentityNone {
				log.Printf("plc-catid: process %d (%s) counter tag %q has no parent struct — skipping (plant does not match MES_*.<leaf> convention)", p.ID, p.Name, p.CounterTagName)
			}
			continue
		}
		// A failed publish toggle is logged, not fatal: the tag may already be
		// published, and an unreadable tag is handled on every tick (the read
		// path fails open). Skipping the process here switched A5, the
		// auto-cutover and the verify off for it until the next restart. The
		// managed bit is left as found: at boot the manager's cache cannot
		// say whether WarLink was already publishing the tag, so only a save
		// (republishIdentity, after IsTagPublished) marks it the Edge's.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := cm.eng.plcMgr.EnableTagPublishing(ctx, p.CounterPLCName, tag); err != nil {
			log.Printf("plc-catid: enable publish for %s.%s: %v (monitoring anyway)", p.CounterPLCName, tag, err)
		}
		cancel()
		log.Printf("plc-catid: monitoring process %d (%s) on %s.%s", p.ID, p.Name, p.CounterPLCName, tag)
		cm.states[p.ID] = &catidState{plcName: p.CounterPLCName, tagName: tag, processName: p.Name}
		cm.snapshots.Store(p.ID, &catidSnapshot{observed: observed[p.ID]})
	}
}

// observedByProcess reads every value each process has emitted, from the
// observation record, in one query.
func (cm *catidMonitor) observedByProcess() map[int64]map[string]struct{} {
	out := map[int64]map[string]struct{}{}
	rows, err := cm.eng.db.ListPLCCATIDObservations()
	if err != nil {
		log.Printf("plc-catid: read observations: %v", err)
		return out
	}
	for _, r := range rows {
		if out[r.ProcessID] == nil {
			out[r.ProcessID] = map[string]struct{}{}
		}
		out[r.ProcessID][r.CATID] = struct{}{}
	}
	return out
}

// reprime brings the monitor in line with a process's row after a counter or
// identity edit changed its identity tag: a fresh tracker on the new tag, or no
// state at all when the process has none; the open verify watch is dropped and
// the snapshot swapped (the observed values stay — they are the process's).
// Called after the WarLink PATCH, outside every other lock; it re-reads the row
// under cm.mu so an edit and a tick cannot interleave on the maps the tick
// iterates (TestReprime_ConcurrentWithTick).
func (cm *catidMonitor) reprime(processID int64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.verify, processID)
	p, err := cm.eng.db.GetProcess(processID)
	tag := ""
	if err == nil {
		tag = identityTagOf(p)
	}
	if tag == "" {
		delete(cm.states, processID)
		cm.snapshots.Delete(processID)
		return
	}
	cm.states[processID] = &catidState{plcName: p.CounterPLCName, tagName: tag, processName: p.Name}
	var observed map[string]struct{}
	if old, ok := cm.snapshot(processID); ok {
		observed = old.observed
	}
	cm.snapshots.Store(processID, &catidSnapshot{observed: observed})
	log.Printf("plc-catid: monitoring process %d (%s) on %s.%s", p.ID, p.Name, p.CounterPLCName, tag)
}

// forget drops a deleted process from the monitor.
func (cm *catidMonitor) forget(processID int64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.states, processID)
	delete(cm.verify, processID)
	cm.snapshots.Delete(processID)
}

// snapshot returns the process's published identity snapshot. No lock.
func (cm *catidMonitor) snapshot(processID int64) (*catidSnapshot, bool) {
	v, ok := cm.snapshots.Load(processID)
	if !ok {
		return nil, false
	}
	return v.(*catidSnapshot), true
}

// publishRead refreshes the process's snapshot after a good read at now: the
// confirmed value, its confirm time and the read's time, and, on a confirm
// edge, the value added to the observed set (copied, never written in place).
// The latest landing carries over. Called with cm.mu held.
func (cm *catidMonitor) publishRead(processID int64, st *catidState, now time.Time, confirmed bool) {
	next := &catidSnapshot{lastOK: now}
	if old, ok := cm.snapshot(processID); ok {
		next.observed, next.landing = old.observed, old.landing
	}
	if st.seenValue {
		next.value, next.confirmedAt = st.lastConfirmed, st.confirmedAt
	}
	if confirmed && isIdentity(next.value) && !catidSetHas(next.observed, next.value) {
		grown := make(map[string]struct{}, len(next.observed)+1)
		for v := range next.observed {
			grown[v] = struct{}{}
		}
		grown[next.value] = struct{}{}
		next.observed = grown
	}
	cm.snapshots.Store(processID, next)
}

// noteLanding publishes the process's latest landing on its snapshot. Under
// cm.mu, so a read's publish (which carries the landing over) cannot drop it.
func (cm *catidMonitor) noteLanding(processID, fromStyleID int64, at time.Time) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	old, ok := cm.snapshot(processID)
	if !ok {
		return
	}
	next := *old
	next.landing = catidLanding{fromStyleID: fromStyleID, at: at}
	cm.snapshots.Store(processID, &next)
}

// landedAt is when the process last landed, zero if never. No lock.
func (cm *catidMonitor) landedAt(processID int64) time.Time {
	if s, ok := cm.snapshot(processID); ok {
		return s.landing.at
	}
	return time.Time{}
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
		case pid := <-cm.eng.cutoverPokes:
			cm.notePoke(pid)
		}
	}
}

// notePoke marks the process's decision to be taken again on the next tick.
func (cm *catidMonitor) notePoke(processID int64) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if st, ok := cm.states[processID]; ok {
		st.poked = true
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
	// Fire AFTER releasing cm.mu. A fire calls the door (completeCutover) or the
	// switch (autoSwitchStyle), each under cutoverMu with the gate, the flip and
	// their DB writes; running that under the tick lock would block the guard's
	// liveCATID reads, and cutoverMu is outer to cm.mu (lockorder rank 8). The
	// intent was fully decided under the lock.
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
	st.lastErr = ""
	if !ok {
		st.lastErr = err.Error()
	} else {
		cur, ok = catidToString(raw)
		if !ok {
			st.lastErr = fmt.Sprintf("unexpected value type %T", raw)
			log.Printf("plc-catid: unexpected value type %T for %s.%s — skipping", raw, st.plcName, st.tagName)
		}
	}
	confirmed, isChange := applyCatidEdge(st, cur, ok, now)
	if confirmed {
		st.confirmedAt, st.confirmedBaseline = now, !isChange
		cm.onConfirmedCATID(processID, st, isChange)
	}
	if ok {
		cm.publishRead(processID, st, now, confirmed)
	}
	cm.checkPostCutoverVerify(processID, st, now)
	return cm.evaluateAutoArm(processID, st, ok, now)
}

// openPostCutoverVerify starts a post-cutover verification watch for a process:
// within [now, deadline], the monitor checks whether the press's live part id
// is one of set (the new active style's parts, read by the caller) and flags
// changeoverID if it is not. A process the monitor does not hold gets none.
// Called from the completion path (outside the tick); locks mu.
func (cm *catidMonitor) openPostCutoverVerify(processID, changeoverID, styleID int64, set map[string]struct{}, deadline time.Time) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if _, ok := cm.states[processID]; !ok {
		return
	}
	if cm.verify == nil {
		cm.verify = map[int64]*postCutoverVerify{}
	}
	cm.verify[processID] = &postCutoverVerify{
		changeoverID: changeoverID,
		styleID:      styleID,
		set:          set,
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
	// The new style's part-identity set, read when the watch opened (a side of
	// a two-part style is a valid post-cutover reading).
	set := pv.set
	// Success short-circuit: the moment the live part is one of the new style's
	// parts, the cutover is verified — clear any flag and close the watch.
	if st.seenValue && catidSetHas(set, st.lastConfirmed) {
		if err := cm.eng.db.SetChangeoverVerifyMismatch(pv.changeoverID, ""); err != nil {
			log.Printf("plc-catid: clear post-cutover flag on changeover %d: %v", pv.changeoverID, err)
		}
		cm.recordEndCATID(pv, st)
		delete(cm.verify, processID)
		cm.eng.catidHealthChanged(processID)
		return
	}
	if now.Before(pv.deadline) {
		return // still inside the window — give the press time to settle
	}
	// Window elapsed: record what the PLC is on (the pin suggestion's evidence),
	// and if the style has parts to check against and the value names a part
	// none of them is, flag the changeover; otherwise close silently.
	cm.recordEndCATID(pv, st)
	if st.seenValue && isIdentity(st.lastConfirmed) && len(set) > 0 && !catidSetHas(set, st.lastConfirmed) {
		if err := cm.eng.db.SetChangeoverVerifyMismatch(pv.changeoverID, st.lastConfirmed); err != nil {
			log.Printf("plc-catid: flag post-cutover mismatch on changeover %d: %v", pv.changeoverID, err)
		} else {
			log.Printf("CATID post-cutover mismatch: process %s (process %d) reports %q after cutover to style %d (parts {%s}) — flagged for operator confirmation",
				st.processName, processID, st.lastConfirmed, pv.styleID, formatCATIDSet(set))
			cm.eng.Events.Emit(Event{Type: EventChangeoverVerifyMismatch, Payload: CATIDVerifyMismatchEvent{
				ProcessID:     processID,
				ProcessName:   st.processName,
				ChangeoverID:  pv.changeoverID,
				StyleID:       pv.styleID,
				ExpectedCATID: formatCATIDSet(set),
				LiveCATID:     st.lastConfirmed,
			}})
			cm.eng.catidHealthChanged(processID)
		}
	}
	delete(cm.verify, processID)
}

// recordEndCATID writes the PLC's confirmed part on the watched changeover as
// the watch closes, when it names one. Called with cm.mu held.
func (cm *catidMonitor) recordEndCATID(pv *postCutoverVerify, st *catidState) {
	if !st.seenValue || !isIdentity(st.lastConfirmed) {
		return
	}
	if err := cm.eng.db.SetChangeoverEndCATID(pv.changeoverID, st.lastConfirmed); err != nil {
		log.Printf("plc-catid: record end CATID on changeover %d: %v", pv.changeoverID, err)
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
	// Record what the press actually said, matching or not. Until this existed
	// the plant had no persisted evidence of what a PLC sends — only the values
	// people typed into Core, and only the readings that DISAGREED (on
	// process_changeovers.verify_live_catid), which is a record of exceptions
	// and never of the norm. One decision reads it: whether the running
	// style's set is proven (catid_attribute.go), which gates off-plan
	// attribution and the switch.
	if err := cm.eng.db.RecordPLCCATIDObservation(processID, st.plcName, st.lastConfirmed); err != nil {
		log.Printf("plc-catid: record observation for process %d: %v", processID, err)
	}
	cm.eng.catidHealthChanged(processID)

	// A value that names no part (no program, an unused slot) is neither a
	// wrong part nor a change of part.
	if !isIdentity(st.lastConfirmed) {
		return
	}
	styleID, styleName, set, ok := cm.eng.activeStyleCATIDSet(processID)
	// A5: alert when the active style has configured/derivable CATIDs and the live
	// part is NONE of them (not just != a single value — a two-position style runs
	// two parts, and either side is fine). Empty set = inert (never alert).
	if ok && len(set) > 0 && !catidSetHas(set, st.lastConfirmed) {
		toStyle := ""
		co, waiting := cm.eng.plannedChangeoverTo(processID, st.lastConfirmed)
		if co != nil {
			toStyle = co.ToStyleName
		}
		log.Printf("CATID mismatch: process %s (process %d) live CATID %q not among active style %q parts {%s} — outgoing-style relief is blocked",
			st.processName, processID, st.lastConfirmed, styleName, formatCATIDSet(set))
		cm.eng.Events.Emit(Event{Type: EventCATIDMismatch, Payload: CATIDMismatchEvent{
			ProcessID:         processID,
			ProcessName:       st.processName,
			PLCName:           st.plcName,
			StyleID:           styleID,
			StyleName:         styleName,
			LiveCATID:         st.lastConfirmed,
			ExpectedCATID:     formatCATIDSet(set),
			ChangeoverToStyle: toStyle,
			WaitingOn:         waiting,
		}})
	}
	// On an actual CHANGE of the physical part (not the first-read baseline),
	// prompt now only in prompt mode. Auto decides after the stability window
	// (evaluateAutoArm): the cutover, the switch, or — a part several styles
	// run — this same prompt (decideAutoSwitch). Off is silent.
	if isChange && cm.eng.processChangeoverAutoArm(processID) == domain.ChangeoverAutoArmPrompt {
		cm.raiseCATIDChangePrompt(processID, st)
	}
}

// raiseCATIDChangePrompt emits EventCATIDChangePrompt after the press's part
// changed, pre-filling the target style when the new CATID maps to a known
// style's expected_catid. B1 PROMPT half only: it never starts the changeover —
// the operator confirms through the existing Start Changeover flow. Only while
// no changeover is open (TestPrompt_OnlyWhenNoChangeoverOpen). Called with
// cm.mu held.
//
// Suppressed when the new part matches the already-active style's expected_catid
// (the line is now correct for the running style — a completed cutover, not a
// pending one), so a change that RESOLVES a mismatch does not nag the operator
// to change to the style they are already running.
func (cm *catidMonitor) raiseCATIDChangePrompt(processID int64, st *catidState) {
	// The prompt offers to START a changeover: with one already open the start
	// path would refuse it, and the cutover is the monitor's or the operator's.
	if co, err := cm.eng.db.GetActiveProcessChangeover(processID); err == nil && co != nil {
		return
	}
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
// target. Shared by the prompt-mode confirm edge and auto's ambiguity
// fallback (decideAutoSwitch, a counted part several styles run). Called with
// cm.mu held.
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
	log.Printf("CATID change: process %s (process %d) part is now CATID %q — %d candidate style(s): %s",
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
	// Unreadable, never-seen, or a value that names no part: reset and stay
	// silent. A blip can never accumulate toward an arm.
	if !ok || !st.seenValue || !isIdentity(st.lastConfirmed) {
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
		st.armDecided, st.armRefusedAt, st.poked = false, time.Time{}, false
		return nil
	}
	st.armCount++
	// Require BOTH >=3 consecutive confirmed reads AND >=60s since first seen.
	if !st.stableAt(now) {
		return nil
	}
	if !st.redecide(now) {
		return nil
	}
	st.armDecided, st.poked, st.armRefusedAt = true, false, time.Time{}
	intent := cm.decideAutoArm(processID, st, v, now)
	if intent != nil {
		intent.decidedAt = now
	}
	return intent
}

// decideAutoArm makes the decision once a value is stable. With a changeover
// open, it is the cutover: cutoverPredicate must hold over the active style's
// and the open changeover's target's CATID sets, read in one process-wide
// query. With none open, it is decideAutoSwitch: a switch, never a start (the
// monitor never starts a changeover: when it did, on 2026-07-28, it put four
// robots on the floor chasing a style with zero stock). The mode is read with
// the predicate: the intent carries it, and only auto calls the door or
// switches; prompt and off log what auto would have done. Called with cm.mu
// held; light DB reads only.
//
// With the predicate held, the intent is forced when the counted-part
// condition also holds (forceHolds): auto then completes past a red gate.
//
// The live part need not be the target's when the outgoing style has parts:
// a press set up for some third style has still left the outgoing part, and
// leaving the changeover open would block the corrective changeover the
// operator now needs. The post-cutover verify compares the live part against
// the new active style and raises the operator flag when they disagree.
func (cm *catidMonitor) decideAutoArm(processID int64, st *catidState, v string, now time.Time) *autoCutoverIntent {
	proc, err := cm.eng.db.GetProcess(processID)
	if err != nil || proc == nil {
		log.Printf("plc-catid: cannot read process %d (%v) — not cutting over", processID, err)
		return nil
	}
	changeover, err := cm.eng.db.GetActiveProcessChangeover(processID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// A real read failure, not the everyday "no active changeover". Say so and
		// stay put: an unknown changeover state is not a licence to cut over.
		log.Printf("plc-catid: cannot read active changeover for process %d: %v — not cutting over", processID, err)
		return nil
	}
	if err != nil || changeover == nil {
		return cm.decideAutoSwitch(proc, st, v, now)
	}
	out, in := cm.eng.cutoverCATIDSets(proc, changeover.ToStyleID)
	if !cutoverPredicate(v, out, in) {
		return nil
	}
	f := st.facts(now)
	f.tick = cm.lastCountedTick(processID)
	return &autoCutoverIntent{
		processID:    processID,
		processName:  st.processName,
		changeoverID: changeover.ID,
		catid:        v,
		mode:         domain.NormalizeChangeoverAutoArm(proc.ChangeoverAutoArm),
		force:        forceHolds(f, changeover.StartedAt),
	}
}

// decideAutoSwitch is the decision with no changeover open (N6), on a stable
// part v the process counted a stroke of since it was confirmed:
//
//   - v off-plan (A proven, v in exactly one other live style S): switch to S
//     when S's set is a person's pin; a catalog-derived set may attribute but
//     never moves a cell.
//   - v held because several styles run it: the change prompt, with them as
//     its candidates, in auto (prompt mode raised it at the change).
//
// v must be a change, never a baseline, confirmed after the process's latest
// landing: a part standing from before it is the run-out of the style the
// landing left (or a mismatch the cell already ran with), not the cell
// running another style (TestAutoSwitch_RunOutAfterAnEarlyCutoverNeverSwitchesBack,
// TestAutoSwitch_BaselineNeverSwitches). The declaration alone does nothing:
// a part no stroke was counted on moves nothing. An answer that switches
// nothing — the prompt, the derived-set line, prompt and off's "would have
// switched" — is raised once per confirmed value (raiseOnce), however often a
// poke decides it again. Called with cm.mu held; light DB reads only.
func (cm *catidMonitor) decideAutoSwitch(proc *processes.Process, st *catidState, v string, now time.Time) *autoCutoverIntent {
	f := st.facts(now)
	if f.baseline || !f.confirmedAt.After(cm.landedAt(proc.ID)) {
		return nil
	}
	f.tick = cm.lastCountedTick(proc.ID)
	mode := domain.NormalizeChangeoverAutoArm(proc.ChangeoverAutoArm)
	switch {
	case countedSince(f, plc.TickOffPlan):
	case countedSince(f, plc.TickHeld):
		if mode == domain.ChangeoverAutoArmAuto && len(cm.eng.stylesForCATID(proc.ID, v)) > 1 && st.raiseOnce() {
			cm.raiseCATIDChangePrompt(proc.ID, st)
		}
		return nil
	default:
		return nil
	}
	if proc.ActiveStyleID == nil {
		return nil
	}
	to := switchTarget(cm.eng.processStyleCATIDSets(proc.ID), *proc.ActiveStyleID, v)
	if to == nil {
		return nil
	}
	if !to.Pinned {
		if st.raiseOnce() {
			cm.eng.logFn("plc-catid: process %s: CATID %s counted on %s, whose set is derived from the catalog — not switching",
				st.processName, v, to.Name)
		}
		return nil
	}
	if mode != domain.ChangeoverAutoArmAuto && !st.raiseOnce() {
		return nil
	}
	return &autoCutoverIntent{processID: proc.ID, processName: st.processName, catid: v, mode: mode,
		switchTo: to.ID, switchName: to.Name, fromStyleID: *proc.ActiveStyleID}
}

// switchTarget is the one live style other than the active one whose set holds
// v; nil when none or several do. The decision and the switch's re-read.
func switchTarget(sets []styleCATIDEntry, activeStyleID int64, v string) *styleCATIDEntry {
	var to *styleCATIDEntry
	for i := range sets {
		if sets[i].ID != activeStyleID && catidSetHas(sets[i].Set, v) {
			if to != nil {
				return nil
			}
			to = &sets[i]
		}
	}
	return to
}

// lastCountedTick returns the process's last counted tick placed off today,
// zero if none. No lock.
func (cm *catidMonitor) lastCountedTick(processID int64) countedTick {
	if v, ok := cm.countedTicks.Load(processID); ok {
		return v.(countedTick)
	}
	return countedTick{}
}

// factsOf reads a process's facts at now for a caller outside the tick (the
// strip); the zero value, not monitored, when the monitor does not hold it.
// Takes cm.mu briefly: an HTTP read, never the poll.
func (cm *catidMonitor) factsOf(processID int64, now time.Time) catidFacts {
	cm.mu.Lock()
	var f catidFacts
	if st, ok := cm.states[processID]; ok {
		f = st.facts(now)
	}
	cm.mu.Unlock()
	f.tick = cm.lastCountedTick(processID)
	if snap, ok := cm.snapshot(processID); ok {
		f.lastOK = snap.lastOK
	}
	return f
}

// cutoverPredicate is the auto-cutover's decision on a stable CATID v: v names
// a part, the outgoing style does not run it, and either the outgoing style
// has parts to have left or v is one the incoming style runs. A CATID both
// styles run never fires (TestPredicate_SharedCATIDNeverFires), and with both
// sets empty a value says nothing (TestPredicate_BothSetsEmptyIsInert).
func cutoverPredicate(v string, out, in map[string]struct{}) bool {
	return isIdentity(v) && !catidSetHas(out, v) && (len(out) > 0 || catidSetHas(in, v))
}

// isIdentity reports whether a confirmed CATID names a part. "" (nothing read),
// "0" (no program) and negative values (an unused slot reads -1) do not.
func isIdentity(v string) bool {
	if v == "" {
		return false
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n <= 0 {
		return false
	}
	return true
}

// fireAutoCutover acts on a decided intent OUTSIDE cm.mu. In auto mode it calls
// the door — forced when the counted-part condition holds, gated otherwise —
// and, on success, raises the station notification; in prompt and off it reads
// the gate (read-only) and logs what auto would have done. Every mode logs the
// decision line. Never starts, cancels or re-plans anything.
func (cm *catidMonitor) fireAutoCutover(in autoCutoverIntent) {
	if in.switchTo != 0 {
		cm.fireAutoSwitch(in)
		return
	}
	counted := ""
	if in.force {
		counted = ", counted on the target since the change"
	}
	if in.mode != domain.ChangeoverAutoArmAuto {
		gate := "gate green"
		if ok, blockers, err := cm.eng.canCompleteChangeover(in.changeoverID); err != nil {
			gate = fmt.Sprintf("gate unreadable (%v)", err)
		} else if !ok {
			gate = "waiting on " + strings.Join(domain.BlockersToReasons(blockers), "; ")
		}
		cm.eng.logFn("plc-catid: process %s changeover %d: predicate held on CATID %s%s, %s, mode=%s — would have cut over",
			in.processName, in.changeoverID, in.catid, counted, gate, in.mode)
		return
	}
	blockers, moved, err := cm.callDoor(in)
	if moved != "" {
		cm.eng.logFn("plc-catid: process %s changeover %d: %s since the decision on CATID %s — not cut over",
			in.processName, in.changeoverID, moved, in.catid)
		return
	}
	if errors.Is(err, errNotTheOpenChangeover) {
		// Completed (or cancelled) by someone else since the decision: nothing
		// was cut over here, so no notice and no retry.
		cm.eng.logFn("plc-catid: process %s changeover %d: no longer open — nothing to cut over",
			in.processName, in.changeoverID)
		return
	}
	if err != nil {
		cm.eng.logFn("plc-catid: process %s changeover %d: predicate held on CATID %s%s, waiting on %v, mode=%s — not cut over",
			in.processName, in.changeoverID, in.catid, counted, err, in.mode)
		cm.noteRefusal(in)
		return
	}
	gate := "gate green"
	if len(blockers) > 0 {
		gate = "forced past " + strings.Join(domain.BlockersToReasons(blockers), "; ")
	}
	cm.eng.logFn("plc-catid: process %s changeover %d: predicate held on CATID %s%s, %s, mode=%s — cut over",
		in.processName, in.changeoverID, in.catid, counted, gate, in.mode)
	cm.eng.Events.Emit(Event{Type: EventCATIDAutoCutover, Payload: CATIDAutoCutoverEvent{
		ProcessID:    in.processID,
		ProcessName:  in.processName,
		ChangeoverID: in.changeoverID,
		NewCATID:     in.catid,
		Forced:       len(blockers) > 0,
		Blockers:     blockers,
	}})
}

// callDoor calls the door for an auto-mode cutover intent under cutoverMu,
// after re-reading there what the decision stood on (autoCutoverMoved): moved
// names what changed since, and the door is then not called. Gated, the door
// runs canCompleteChangeover first, which refuses while any node task is
// non-terminal or an order is still placing a bin at a participant node: the
// normal "not finished yet" answer, decided again on the process's next poke,
// or within a minute. Forced — the press has made the incoming part and it was
// counted — the door completes past what the gate waits on, writes it in the
// row's notes, and leaves the legs running.
func (cm *catidMonitor) callDoor(in autoCutoverIntent) (blockers []domain.Blocker, moved string, err error) {
	complete := cm.completeCutover
	if complete == nil {
		complete = cm.eng.completeCutoverLocked
	}
	cm.eng.cutoverMu.Lock()
	defer cm.eng.cutoverMu.Unlock()
	if moved = cm.eng.autoCutoverMoved(in); moved != "" {
		return nil, moved, nil
	}
	if in.force {
		blockers, err = complete(in.processID, in.changeoverID, triggeredAutoCATID, CutoverForced)
	} else {
		blockers, err = complete(in.processID, in.changeoverID, triggeredAutoCATID, CutoverGated)
	}
	return blockers, "", err
}

// autoCutoverMoved re-reads, under cutoverMu, what an automatic cutover was
// decided on: the process still in auto, and the predicate still holding on
// the decided value over the CATID sets as they stand now (a pin saved since
// the decision moves them). "" when the decision stands, else what moved
// (TestFire_AModeTurnedOffSinceTheDecisionNeverCutsOver,
// TestFire_APinMovedSinceTheDecisionNeverCutsOver). A changeover no longer
// open is left to the door, which says so.
func (e *Engine) autoCutoverMoved(in autoCutoverIntent) string {
	proc, err := e.db.GetProcess(in.processID)
	if err != nil || proc == nil {
		return fmt.Sprintf("the process could not be read (%v)", err)
	}
	if mode := domain.NormalizeChangeoverAutoArm(proc.ChangeoverAutoArm); mode != domain.ChangeoverAutoArmAuto {
		return "the process is in mode " + mode
	}
	co, err := e.db.GetActiveProcessChangeover(in.processID)
	if err != nil || co == nil || co.ID != in.changeoverID {
		return ""
	}
	if out, target := e.cutoverCATIDSets(proc, co.ToStyleID); !cutoverPredicate(in.catid, out, target) {
		return "the CATID sets no longer cut over on it"
	}
	return ""
}

// fireAutoSwitch acts on an automatic-switch intent OUTSIDE cm.mu: in auto it
// switches (autoSwitchStyle); in prompt and off it logs what auto would have
// done.
func (cm *catidMonitor) fireAutoSwitch(in autoCutoverIntent) {
	if in.mode != domain.ChangeoverAutoArmAuto {
		cm.eng.logFn("plc-catid: process %s: CATID %s counted on %s with no changeover open, mode=%s — would have switched",
			in.processName, in.catid, in.switchName, in.mode)
		return
	}
	if err := cm.eng.autoSwitchStyle(in.processID, in.fromStyleID, in.switchTo, in.catid); err != nil {
		cm.eng.logFn("plc-catid: process %s: switch to %s on CATID %s failed: %v", in.processName, in.switchName, in.catid, err)
	}
}

// noteRefusal dates the door's refusal of an intent on the process's state,
// when the candidate is still the one decided: the clock half of the retry.
func (cm *catidMonitor) noteRefusal(in autoCutoverIntent) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if st, ok := cm.states[in.processID]; ok && st.armCandidate == in.catid {
		st.armRefusedAt = in.decidedAt
	}
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
// process not monitored) or the value names no part (isIdentity). The guard
// reads this; ok=false ⇒ guard stays inert.
func (cm *catidMonitor) liveCATID(processID int64) (string, bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	st, ok := cm.states[processID]
	if !ok || !st.seenValue || !isIdentity(st.lastConfirmed) {
		return "", false
	}
	return st.lastConfirmed, true
}

// catidToString normalizes a WarLink CATID tag value to a comparison string.
// CATID is an integer part id (eight digits where it is in use) delivered as
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
