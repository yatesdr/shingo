// sourceability_monitor.go — the always-on sourceability recompute, Core side.
//
// The plant-wide sourceability computation (store/sourceability) answers, for
// every configured (process, style), whether it can be sourced now. This monitor
// keeps that answer fresh WITHOUT polling: bin / order / reservation changes mark
// the affected styles dirty via the plant.claims payload→styles index, and one
// batched recompute runs per debounce window. A periodic full recompute is the
// safety net (and reconciles styles that were added or removed).
//
// It is a pure READ, like the computation it drives — it counts availability and
// never acquires or reserves anything.
//
// State is in-memory (rebuilt by the startup + periodic full recompute), matching
// the SOFT freshness requirement: a skipped cycle is acceptable, and a Core
// restart re-derives everything from the DB. The outbound feed and the Core page
// (later work) read Snapshot().

package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"shingo/protocol"
	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

const (
	// sourceabilityDebounceWindow coalesces a burst of change events into one
	// recompute. Design range 250–500 ms.
	sourceabilityDebounceWindow = 300 * time.Millisecond
	// sourceabilityFullInterval is the safety-net full recompute cadence.
	sourceabilityFullInterval = 2 * time.Minute
)

// SourceabilityMonitor recomputes per-(process, style) sourceability on change,
// debounced, and serves the latest state to readers.
type SourceabilityMonitor struct {
	eng *Engine
	cfg sourceability.Config

	rateWindow     time.Duration
	debounceWindow time.Duration
	fullInterval   time.Duration

	// recomputeFn runs a scoped recompute for the given dirty keys. A field so
	// the debounce logic can be unit-tested without a database; production wiring
	// points it at recomputeKeys.
	recomputeFn func(keys []plantclaims.ProcessKey)
	// publishFn ships a sourcing-state report to the edges. A field so the
	// change-only publish logic can be tested with a capturing stub; production
	// wiring points it at the broadcast to SubjectSourcingState.
	publishFn func(protocol.SourcingStateReport)

	mu sync.Mutex
	// dirty is the set of (process, style) awaiting recompute; drained per window.
	dirty map[plantclaims.ProcessKey]struct{}
	// state is the latest verdict per (process, style).
	state map[plantclaims.ProcessKey]sourceability.StyleState
	// index is payload → the styles that require it, cached from the last full
	// recompute so an event maps to affected styles without a per-event query.
	index map[string][]plantclaims.ProcessKey
	// timer arms the debounced flush; nil when idle.
	timer *time.Timer
	// recomputes counts recompute passes (full + dirty) for the batching test.
	recomputes int
}

// NewSourceabilityMonitor constructs the monitor from engine config. Yellow ships
// dark (EnableAtRisk false) until the owner validates the rate window.
func NewSourceabilityMonitor(e *Engine) *SourceabilityMonitor {
	sc := e.cfg.Sourceability
	m := &SourceabilityMonitor{
		eng:            e,
		cfg:            sourceability.Config{YellowEnabled: sc.EnableAtRisk, Horizon: sc.Horizon},
		rateWindow:     sc.RateWindow,
		debounceWindow: sourceabilityDebounceWindow,
		fullInterval:   sourceabilityFullInterval,
		dirty:          map[plantclaims.ProcessKey]struct{}{},
		state:          map[plantclaims.ProcessKey]sourceability.StyleState{},
		index:          map[string][]plantclaims.ProcessKey{},
	}
	m.recomputeFn = m.recomputeKeys
	m.publishFn = m.broadcast
	return m
}

// broadcast ships a sourcing-state report to every edge on SubjectSourcingState.
// Best-effort: a send failure is logged, not fatal — the next change or the
// periodic snapshot re-sends, and each edge's persisted cache carries it over a
// Core partition.
func (m *SourceabilityMonitor) broadcast(report protocol.SourcingStateReport) {
	if err := m.eng.SendDataToEdge(protocol.SubjectSourcingState, protocol.StationBroadcast, report); err != nil {
		m.eng.logFn("sourceability: publish (snapshot=%v, %d states): %v",
			report.Snapshot, len(report.States), err)
	}
}

// Run does a startup full recompute, then runs the periodic full recompute until
// ctx is cancelled. Event-driven recomputes happen via the bus subscriptions
// wired in SetupEngineListeners.
func (m *SourceabilityMonitor) Run(ctx context.Context) {
	m.recomputeAll()
	go func() {
		t := time.NewTicker(m.fullInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.recomputeAll()
			}
		}
	}()
}

// onPayloadChanged marks every style that requires payload dirty and arms the
// debounce. An event for a payload no style claims is ignored — no wasted
// recompute. Bin/order/reservation events all funnel through here by payload.
func (m *SourceabilityMonitor) onPayloadChanged(payload string) {
	if payload == "" {
		return
	}
	m.mu.Lock()
	keys := m.index[payload]
	if len(keys) == 0 {
		m.mu.Unlock()
		return
	}
	for _, k := range keys {
		m.dirty[k] = struct{}{}
	}
	m.arm()
	m.mu.Unlock()
}

// arm (re)starts the debounce timer. Caller holds mu.
func (m *SourceabilityMonitor) arm() {
	if m.timer == nil {
		m.timer = time.AfterFunc(m.debounceWindow, m.flush)
		return
	}
	m.timer.Reset(m.debounceWindow)
}

// flush drains the dirty set and recomputes it once. Runs off the debounce
// timer, so many events in a window collapse to a single recompute.
func (m *SourceabilityMonitor) flush() {
	m.mu.Lock()
	if len(m.dirty) == 0 {
		m.timer = nil
		m.mu.Unlock()
		return
	}
	keys := make([]plantclaims.ProcessKey, 0, len(m.dirty))
	for k := range m.dirty {
		keys = append(keys, k)
	}
	m.dirty = map[plantclaims.ProcessKey]struct{}{}
	m.timer = nil
	m.mu.Unlock()

	m.recomputeFn(keys)
}

// recomputeKeys recomputes just the dirty styles (set-based) and updates their
// state. The plant snapshot read is shared; only the listed styles are scored.
func (m *SourceabilityMonitor) recomputeKeys(keys []plantclaims.ProcessKey) {
	if len(keys) == 0 {
		return
	}
	in, err := sourceability.BuildInputs(m.eng.db.DB, m.rateWindow)
	if err != nil {
		m.eng.logFn("sourceability: recompute build inputs: %v", err)
		return
	}
	in.Styles = keys
	states := sourceability.Compute(in, m.cfg, time.Now())

	var changed []sourceability.StyleState
	m.mu.Lock()
	for _, s := range states {
		k := plantclaims.ProcessKey{ProcessID: s.ProcessID, StyleID: s.StyleID}
		old, existed := m.state[k]
		m.state[k] = s
		if !existed || wireChanged(old, s) {
			changed = append(changed, s)
		}
	}
	m.recomputes++
	m.mu.Unlock()

	// Change-only publish: nothing moved → nothing on the wire (no steady-state
	// chatter). Only the styles whose verdict actually changed go out, as a delta.
	if len(changed) > 0 && m.publishFn != nil {
		m.publishFn(toReport(changed, false))
	}
	m.emitSourcingUpdated(len(changed))
}

// emitSourcingUpdated tells the Core SSE layer that a verdict MOVED. It is the
// precise signal the /sourcing page refreshes on: the page displays verdicts, so
// it should react to a verdict changing and to nothing else.
//
// No change, no event. That is the whole point — the page previously refreshed
// on bin-update/inventory-update, the pool reads that FEED a verdict, so a bin
// moving anywhere on the plant refreshed a page whose content had not changed.
func (m *SourceabilityMonitor) emitSourcingUpdated(changed int) {
	if changed <= 0 || m.eng == nil || m.eng.Events == nil {
		return
	}
	m.eng.Events.Emit(Event{
		Type:    EventSourcingUpdated,
		Payload: SourcingUpdatedEvent{Changed: changed},
	})
}

// recomputeAll scores every configured style and REPLACES the state map (so
// styles removed from the mirror drop out) and the payload→styles index. Startup
// + periodic safety net.
func (m *SourceabilityMonitor) recomputeAll() {
	in, err := sourceability.BuildInputs(m.eng.db.DB, m.rateWindow)
	if err != nil {
		m.eng.logFn("sourceability: full recompute build inputs: %v", err)
		return
	}
	states := sourceability.Compute(in, m.cfg, time.Now())

	newState := make(map[plantclaims.ProcessKey]sourceability.StyleState, len(states))
	for _, s := range states {
		newState[plantclaims.ProcessKey{ProcessID: s.ProcessID, StyleID: s.StyleID}] = s
	}
	newIndex := buildPayloadIndex(in.Claims)

	m.mu.Lock()
	prevCount := len(m.state)
	// Diff BEFORE replacing. The full recompute publishes a wire snapshot every
	// periodic cycle whether or not anything moved (a late-joining edge needs
	// it), but the SSE event must not follow that cadence: an idle plant would
	// refresh the page on a timer forever. Count real verdict movement instead,
	// including styles that disappeared from the mirror.
	changed := 0
	for k, s := range newState {
		if old, existed := m.state[k]; !existed || wireChanged(old, s) {
			changed++
		}
	}
	for k := range m.state {
		if _, still := newState[k]; !still {
			changed++
		}
	}
	m.state = newState
	m.index = newIndex
	m.recomputes++
	m.mu.Unlock()

	// The periodic + startup full recompute publishes a full snapshot: a
	// late-joining or restarted edge reads it and is current, and it refreshes
	// any at-risk time-to-empty values the change-only delta path intentionally
	// leaves alone (membership drives deltas, not TTE jitter).
	//
	// Skip an empty snapshot unless it clears a previously non-empty cache: a
	// plant (or a just-booted Core) with no styles yet has nothing to broadcast,
	// so it does not spam every edge — or every test engine's outbox — on start.
	if m.publishFn != nil && (len(states) > 0 || prevCount > 0) {
		m.publishFn(toReport(states, true))
	}
	m.emitSourcingUpdated(changed)
}

// wireChanged reports whether the operator-visible verdict changed between two
// computes: status, the missing list, or which lines are at risk. It ignores
// time-to-empty magnitude and ComputedAt, so steady-state TTE drift does not
// spam the feed — the periodic snapshot refreshes those.
func wireChanged(a, b sourceability.StyleState) bool {
	if a.Status != b.Status {
		return true
	}
	if !equalStrings(a.Missing, b.Missing) {
		return true
	}
	if len(a.AtRisk) != len(b.AtRisk) {
		return true
	}
	for i := range a.AtRisk {
		if a.AtRisk[i].NodeName != b.AtRisk[i].NodeName || a.AtRisk[i].PayloadCode != b.AtRisk[i].PayloadCode {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// toReport projects computed styles onto the wire shape, generating each one's
// operator sentence. snapshot marks a full replace vs a change delta.
func toReport(states []sourceability.StyleState, snapshot bool) protocol.SourcingStateReport {
	out := protocol.SourcingStateReport{Snapshot: snapshot}
	for _, s := range states {
		out.States = append(out.States, toWire(s))
	}
	return out
}

func toWire(s sourceability.StyleState) protocol.SourcingState {
	ws := protocol.SourcingState{
		ProcessID:  s.ProcessID,
		StyleID:    s.StyleID,
		Status:     string(s.Status),
		Missing:    s.Missing,
		Reason:     reasonFor(s),
		ComputedAt: s.ComputedAt,
	}
	for _, r := range s.AtRisk {
		ws.AtRisk = append(ws.AtRisk, protocol.SourcingAtRisk{
			PayloadCode:        r.PayloadCode,
			Node:               r.NodeName,
			TimeToEmptySeconds: r.TimeToEmpty.Seconds(),
		})
	}
	return ws
}

// reasonFor builds the operator-facing sentence Core owns and the HMI displays
// verbatim — the generated-sentence style (no new vocabulary), matching the
// design's operator-reads column.
func reasonFor(s sourceability.StyleState) string {
	switch s.Status {
	case sourceability.StatusNotConfigured:
		// Deliberately not phrased as a capability. The style has no claims, so
		// the system knows nothing about whether it could be sourced.
		return "Not set up — no sourceability claims configured for this style."
	case sourceability.StatusRed:
		return "Cannot change over — missing " + strings.Join(s.Missing, ", ") + "."
	case sourceability.StatusYellow:
		payloads := make([]string, 0, len(s.AtRisk))
		for _, r := range s.AtRisk {
			payloads = append(payloads, r.PayloadCode)
		}
		return fmt.Sprintf("Can change over, but %s running low — refill first.", strings.Join(payloads, ", "))
	default:
		return "Can change over."
	}
}

// buildPayloadIndex derives payload → styles from the claim set: a claim
// contributes its primary payload plus every allowed alternative.
func buildPayloadIndex(claims map[plantclaims.ProcessKey][]plantclaims.ClaimRow) map[string][]plantclaims.ProcessKey {
	seen := make(map[string]map[plantclaims.ProcessKey]struct{})
	add := func(payload string, k plantclaims.ProcessKey) {
		if payload == "" {
			return
		}
		set, ok := seen[payload]
		if !ok {
			set = make(map[plantclaims.ProcessKey]struct{})
			seen[payload] = set
		}
		set[k] = struct{}{}
	}
	for k, cs := range claims {
		for _, c := range cs {
			add(c.PayloadCode, k)
			for _, a := range c.AllowedPayloadCodes {
				add(a, k)
			}
		}
	}
	out := make(map[string][]plantclaims.ProcessKey, len(seen))
	for payload, set := range seen {
		keys := make([]plantclaims.ProcessKey, 0, len(set))
		for k := range set {
			keys = append(keys, k)
		}
		out[payload] = keys
	}
	return out
}

// Snapshot returns the latest verdict for every known (process, style). The
// outbound feed and the Core page read this. Order is not stable.
func (m *SourceabilityMonitor) Snapshot() []sourceability.StyleState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sourceability.StyleState, 0, len(m.state))
	for _, s := range m.state {
		out = append(out, s)
	}
	return out
}
