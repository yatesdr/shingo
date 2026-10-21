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
	"sort"
	"strings"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/lockcheck"
	"shingocore/domain"
	"shingocore/internal/lockorder"
	"shingocore/store/demands"
	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

const (
	// sourceabilityDebounceWindow coalesces a burst of change events into one
	// recompute. Design range 250–500 ms.
	sourceabilityDebounceWindow = 300 * time.Millisecond
	// sourceabilityFullInterval is the safety-net full recompute cadence.
	sourceabilityFullInterval = 2 * time.Minute
	// sourceabilitySnapshotInterval is the backstop for the full per-station
	// snapshot. The 2-minute pass still computes (it takes the samples) and
	// publishes what changed; a full replace of each Edge's cache goes out on the
	// first pass, on this cadence, on an Edge's (re)registration, and when a
	// style disappears — a delta cannot say "gone".
	sourceabilitySnapshotInterval = 30 * time.Minute
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
	// publishFn ships a sourcing-state report to one station (or to every
	// station, protocol.StationBroadcast). A field so the per-station and
	// change-only publish logic can be tested with a capturing stub; production
	// wiring points it at SubjectSourcingState on the outbox.
	publishFn func(station string, r protocol.SourcingStateReport)
	// stationsFn lists the stations to send a full snapshot to: every enrolled
	// Edge's routing id. A field so tests can name their stations.
	stationsFn func() ([]string, error)
	// snapshotEvery is the full-snapshot backstop (sourceabilitySnapshotInterval).
	snapshotEvery time.Duration

	mu lockcheck.Mutex[lockorder.SourceabilityMonitor]
	// dirty is the set of (process, style) awaiting recompute; drained per window.
	dirty map[plantclaims.ProcessKey]struct{}
	// state is the latest verdict per (process, style).
	state map[plantclaims.ProcessKey]sourceability.StyleState
	// index is payload → the styles that require it, cached from the last full
	// recompute so an event maps to affected styles without a per-event query.
	index map[string][]plantclaims.ProcessKey
	// stations is process → the station that published it (process_styles.
	// station_id), refreshed on every full pass. A process absent from it has no
	// recorded station and goes to every station.
	stations map[string]string
	// lastSnapshot is when the full per-station snapshot last went out.
	lastSnapshot time.Time
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
	m.stationsFn = m.enrolledStations
	m.snapshotEvery = sourceabilitySnapshotInterval
	return m
}

// broadcast ships a sourcing-state report to one station — or to every station,
// protocol.StationBroadcast — on SubjectSourcingState. Best-effort: a send
// failure is logged, not fatal — the next change or snapshot re-sends, and each
// edge's persisted cache carries it over a Core partition.
func (m *SourceabilityMonitor) broadcast(station string, report protocol.SourcingStateReport) {
	if err := m.eng.SendDataToEdge(protocol.SubjectSourcingState, station, report); err != nil {
		m.eng.logFn("sourceability: publish to %s (snapshot=%v, %d states): %v",
			station, report.Snapshot, len(report.States), err)
	}
}

// enrolledStations is every enrolled Edge's routing id (protocol.Address.
// Station), the stations a full snapshot goes to.
func (m *SourceabilityMonitor) enrolledStations() ([]string, error) {
	edges, err := m.eng.db.ListEdges()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range edges {
		if e.StationID != "" {
			out = append(out, e.StationID)
		}
	}
	return out, nil
}

// ── WHERE A VERDICT GOES ──────────────────────────────────────────────────
//
// Each Edge reads the sourcing verdicts of its OWN processes and nothing else
// (ListSourcingStateForProcess), and it used to be sent the whole plant's every
// two minutes — a DELETE of its cache plus an INSERT per style in the plant,
// on every Pi, for rows nothing there reads (SYNTH §4). Now a verdict goes to
// the station that published its process (process_styles.station_id). A process
// with no recorded station — every one in the first minutes after the
// migration, and every one an older Edge publishes — goes to every station, as
// before; it is in every station's snapshot too, so each Edge's full replace
// stays complete for what it reads.

// stationOf is where a process's verdicts go: its recorded station, or every
// station when none is recorded. Caller holds m.mu.
func (m *SourceabilityMonitor) stationOf(process string) string {
	if st, ok := m.stations[process]; ok && st != "" {
		return st
	}
	return protocol.StationBroadcast
}

// publishDeltas sends each station the change delta for its own processes, and
// every station the delta for unrouted ones. Change-only by construction: a
// station with nothing changed is sent nothing.
func (m *SourceabilityMonitor) publishDeltas(changed []sourceability.StyleState) {
	if len(changed) == 0 || m.publishFn == nil {
		return
	}
	m.mu.Lock()
	byStation := map[string][]sourceability.StyleState{}
	var order []string
	for _, s := range changed {
		st := m.stationOf(s.ProcessID)
		if _, seen := byStation[st]; !seen {
			order = append(order, st)
		}
		byStation[st] = append(byStation[st], s)
	}
	m.mu.Unlock()
	for _, st := range order {
		m.publishFn(st, toReport(byStation[st], false))
	}
}

// publishSnapshots sends each named station a full snapshot of its own
// processes plus every unrouted one. only == nil means every station: the
// enrolled Edges and every station a process is recorded against.
//
// If the enrolled stations cannot be read, it falls back to the whole plant to
// every station — today's snapshot, which every Edge applies correctly — rather
// than send nothing.
func (m *SourceabilityMonitor) publishSnapshots(states []sourceability.StyleState, only map[string]bool) {
	if m.publishFn == nil {
		return
	}
	targets := map[string]bool{}
	if only != nil {
		for st := range only {
			targets[st] = true
		}
	} else {
		enrolled, err := m.stationsFn()
		if err != nil {
			m.eng.logFn("sourceability: list stations for the snapshot: %v — sending the whole plant to every station", err)
			m.publishFn(protocol.StationBroadcast, toReport(states, true))
			return
		}
		for _, st := range enrolled {
			targets[st] = true
		}
		m.mu.Lock()
		for _, st := range m.stations {
			targets[st] = true
		}
		m.mu.Unlock()
	}
	if targets[protocol.StationBroadcast] {
		// A snapshot "to every station" is the whole plant — the fallback, and
		// what an unrouted removal asks for.
		m.publishFn(protocol.StationBroadcast, toReport(states, true))
		return
	}
	names := make([]string, 0, len(targets))
	for st := range targets {
		names = append(names, st)
	}
	sort.Strings(names)
	for _, st := range names {
		m.publishFn(st, toReport(m.snapshotFor(st, states), true))
	}
}

// snapshotFor is one station's view of the plant: its own processes' verdicts
// and every unrouted process's.
func (m *SourceabilityMonitor) snapshotFor(station string, states []sourceability.StyleState) []sourceability.StyleState {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sourceability.StyleState
	for _, s := range states {
		if st := m.stationOf(s.ProcessID); st == station || st == protocol.StationBroadcast {
			out = append(out, s)
		}
	}
	return out
}

// SnapshotStation sends one station its full snapshot now — on an Edge's
// (re)registration, so a Pi that reconnects is current at once rather than at
// the next backstop. Nothing is sent before the monitor's first pass has run.
func (m *SourceabilityMonitor) SnapshotStation(station string) {
	if station == "" || m.publishFn == nil {
		return
	}
	m.mu.Lock()
	if m.lastSnapshot.IsZero() {
		m.mu.Unlock()
		return
	}
	states := make([]sourceability.StyleState, 0, len(m.state))
	for _, s := range m.state {
		states = append(states, s)
	}
	m.mu.Unlock()
	sort.Slice(states, func(i, j int) bool {
		if states[i].ProcessID != states[j].ProcessID {
			return states[i].ProcessID < states[j].ProcessID
		}
		return states[i].StyleID < states[j].StyleID
	})
	m.publishFn(station, toReport(m.snapshotFor(station, states), true))
}

// Run does a startup full recompute, then runs the periodic full recompute until
// ctx is cancelled. Event-driven recomputes happen via the bus subscriptions
// wired in SetupEngineListeners.
func (m *SourceabilityMonitor) Run(ctx context.Context) {
	m.recomputeAll()
	go func() {
		t := clock.Default().NewTicker(m.fullInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C():
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
	var persist []verdictChange
	m.mu.Lock()
	for _, s := range states {
		k := plantclaims.ProcessKey{ProcessID: s.ProcessID, StyleID: s.StyleID}
		old, existed := m.state[k]
		m.state[k] = s
		if !existed || wireChanged(old, s) {
			changed = append(changed, s)
			prev := ""
			if existed {
				prev = string(old.Status)
			}
			persist = append(persist, verdictChange{k.ProcessID, k.StyleID, s, prev})
		}
	}
	m.recomputes++
	m.mu.Unlock()

	// Change-only publish: nothing moved → nothing on the wire (no steady-state
	// chatter). Only the styles whose verdict actually changed go out, as a
	// delta, each to the station its process belongs to.
	m.publishDeltas(changed)
	// This is the path that runs on a real plant change — the debounced,
	// scoped recompute. recomputeAll is the periodic safety net. Both persist,
	// or the history only ever records what the two-minute sweep happened to
	// catch.
	m.persistChanges(persist)
	m.emitSourcingUpdated(len(changed))
}

// loaderSamples builds one projection per monitored loader binding: the
// payload's whole in-loop total against the rate the plant burns it at.
//
// ON THE FULL PASS ONLY, like the cell samples beside it, and for the same
// reason — the debounced path reacts to a bin moving and must not carry a read
// that exists for history.
//
// BINDINGS COME FROM THE DATABASE. demand_registry is the record this pass and
// the threshold monitor both read; the monitor used to keep a copy of it in
// memory, and this pass was written not to depend on where that copy's
// reconciliation happened to be. The copy is gone, and the rule is the same.
//
// THE TOTAL IS THE LEDGER'S. SystemUOPForPayload sums bins plus lineside
// buckets — what Core believes is in the loop — and it is the number every
// threshold decision is made against (seat-count round 1 §5: one count, Core's).
// Where the Edge disagrees, the lineside report opens a report_divergence
// episode; the sample does not fold that in, so the score shows the difference
// rather than hiding it inside the numerator.
//
// ONE CALL WITH EVERY PAYLOAD, not one per payload: SystemUOPForPayload is two
// statements whatever the list length (one bins sum, one buckets sum), so the
// whole set costs two and a per-payload loop would cost twice the bindings.
//
// BEST-EFFORT. A failed read logs and returns nothing: the verdict publish and
// the cell samples are not this function's to lose.
//
// It returns the bindings and the in-loop totals it read too: the forecaster
// plans the same loader places from them, for no statement of its own.
func (m *SourceabilityMonitor) loaderSamples(in sourceability.Inputs) ([]sourceability.TTESample, []demands.RegistryEntry, map[string]int) {
	bindings, err := m.eng.db.ListDemandThresholds()
	if err != nil {
		m.eng.logFn("sourceability: loader samples: list monitored bindings: %v — none this pass", err)
		return nil, nil, nil
	}
	if len(bindings) == 0 {
		return nil, nil, nil
	}

	seen := make(map[string]struct{}, len(bindings))
	payloads := make([]string, 0, len(bindings))
	for _, b := range bindings {
		// An empty payload code is not a payload. SystemUOPForPayload rejects
		// the whole request if one reaches it, which would cost every binding's
		// sample for one malformed row.
		if b.PayloadCode == "" {
			continue
		}
		if _, dup := seen[b.PayloadCode]; dup {
			continue
		}
		seen[b.PayloadCode] = struct{}{}
		payloads = append(payloads, b.PayloadCode)
	}
	if len(payloads) == 0 {
		return nil, bindings, nil
	}

	totals, err := m.eng.InventoryService().SystemUOPForPayload(context.Background(), payloads)
	if err != nil {
		m.eng.logFn("sourceability: loader samples: system UOP: %v — none this pass", err)
		return nil, bindings, nil
	}
	total := make(map[string]int, len(totals.Counts))
	for _, c := range totals.Counts {
		total[c.PayloadCode] = c.TotalUOP
	}

	out := make([]sourceability.TTESample, 0, len(bindings))
	for _, b := range bindings {
		if b.PayloadCode == "" {
			continue
		}
		out = append(out, sourceability.LoaderSample(
			b.CoreNodeName, b.PayloadCode, total[b.PayloadCode],
			in.RatePerSec[b.PayloadCode], b.ReplenishUOPThreshold))
	}
	return out, bindings, total
}

// verdictChange is one persisted verdict movement, collected under the
// monitor's lock and written outside it — a DB round trip must not hold a
// mutex that the SSE broadcast and every read path also take.
type verdictChange struct {
	processID, styleID string
	state              sourceability.StyleState
	prevStatus         string
}

// persistChanges writes verdict movements to sourceability_events.
//
// This is the whole point of Phase 5: the diff has existed since the monitor
// was written, has been broadcast to every Edge, and has never been persisted
// — so the root physical condition of 2026-07-21 (zero system stock on
// SYN-PART07A.06) was known continuously and recorded nowhere.
//
// Edge-triggered, so steady state writes nothing. Best-effort: a failed write
// is logged, never fatal — losing a history row must not stop the monitor
// telling Edge what it can source.
func (m *SourceabilityMonitor) persistChanges(changes []verdictChange) {
	for _, c := range changes {
		if err := sourceability.RecordChange(m.eng.db.DB, c.processID, c.styleID, c.state, c.prevStatus); err != nil {
			m.eng.logFn("sourceability: record verdict change %s/%s: %v", c.processID, c.styleID, err)
		}
	}
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

	// Where each process's verdicts go, read with the plant. It is swapped in
	// AFTER the diff below, so a style that disappeared is routed by where it
	// was being sent, not by a map that no longer names its process. A failed
	// read keeps the last map: a wrong route for two minutes is a delta to the
	// wrong Pi's unread rows, not a lost one, and a process it cannot place
	// still goes to every station.
	stations, serr := m.eng.db.ProcessStations()
	if serr != nil {
		m.eng.logFn("sourceability: read process stations: %v — keeping the last routing", serr)
	}

	// THE RUNNING STYLE IS READ HERE, not in BuildInputs, because only this path
	// uses it — it scopes the kept cell samples and no verdict consults it. Read
	// on the debounced path it was a query per bin movement for an answer that
	// path discards, and a read error there failed the verdict itself. A failure
	// here costs the cell samples for one pass and nothing else.
	active, err := sourceability.ActiveStyles(m.eng.db.DB)
	if err != nil {
		m.eng.logFn("sourceability: full recompute active styles: %v — no cell samples this pass", err)
	}
	in.ActiveStyles = active

	states, samples := sourceability.ComputeWithSamples(in, m.cfg, time.Now())

	// A LOADER PLACE GETS A SAMPLE TOO. Measured at Springfield 2026-09-21: 22
	// monitored bindings, NONE at a node any style claim names for that payload,
	// against a history of 2,931 threshold episodes to 937 cell ones. So the
	// score's threshold arm had nothing to join and three quarters of the
	// plant's demand was unscoreable for the whole collection window — not
	// because the forecast was wrong but because no forecast was ever recorded
	// about the places the demand came from.
	//
	// A sample that is not taken cannot be recovered. A scorer can be fixed
	// later against kept rows, so this takes the sample and leaves the scoring
	// arithmetic alone.
	loaderRows, bindings, totals := m.loaderSamples(in)
	samples = append(samples, loaderRows...)

	// KEEP THE PROJECTION THIS PASS ALREADY MADE. lineTTE has run on every full
	// pass since the monitor was written and the number has been discarded every
	// time — surfaced only for GREEN/YELLOW styles, and only with the yellow
	// tier on, which it is at neither plant. So the forecast the near tier will
	// be built on has never once been checked against what actually happened.
	//
	// ON THE FULL PASS ONLY. recomputeKeys is the 300 ms debounce, fired by bin
	// movement: writing there would put a DB round trip on the path that reacts
	// to a bin moving, at a cadence set by plant traffic, for samples that would
	// be near-duplicates of each other. The two-minute pass is the right grain
	// and it is already paid for.
	//
	// Best-effort, like persistChanges: a lost history row must not stop the
	// monitor telling Edge what it can source.
	if len(samples) > 0 {
		if err := sourceability.RecordTTESamples(m.eng.db.DB, samples, sourceability.TTERetention); err != nil {
			m.eng.logFn("sourceability: record tte samples: %v", err)
		}
	}

	// THE FORECAST, on this pass only and from what it already read
	// (forecaster.go): the places, their stock and their rates.
	m.forecast(in, stations, bindings, totals)

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
	// persist collects the changes to write down. Built under the lock,
	// written outside it — a DB round trip must not hold the monitor's mutex,
	// which the SSE broadcast and every read path also take.
	var persist []verdictChange
	var moved []sourceability.StyleState
	for k, s := range newState {
		old, existed := m.state[k]
		if !existed || wireChanged(old, s) {
			changed++
			moved = append(moved, s)
			prev := ""
			if existed {
				prev = string(old.Status)
			}
			persist = append(persist, verdictChange{k.ProcessID, k.StyleID, s, prev})
		}
	}
	// A style that disappeared cannot be said with a delta — the Edge upserts
	// deltas — so the stations it was sent to get a fresh snapshot now.
	var gone map[string]bool
	for k := range m.state {
		if _, still := newState[k]; !still {
			changed++
			if gone == nil {
				gone = map[string]bool{}
			}
			gone[m.stationOf(k.ProcessID)] = true
		}
	}
	m.state = newState
	m.index = newIndex
	if serr == nil {
		m.stations = stations
	}
	m.recomputes++
	now := time.Now()
	due := m.lastSnapshot.IsZero() || now.Sub(m.lastSnapshot) >= m.snapshotEvery
	if due {
		m.lastSnapshot = now
	}
	m.mu.Unlock()

	if len(states) > 0 || prevCount > 0 {
		m.publishFullPass(states, moved, gone, due)
	}

	m.persistChanges(persist)

	m.emitSourcingUpdated(changed)
}

// publishFullPass is what the full pass publishes. It used to publish a
// whole-plant snapshot to every Edge every two minutes. Now:
//   - the first pass, and every sourceabilitySnapshotInterval after it, sends
//     each station its own snapshot (publishSnapshots) — the backstop that
//     also refreshes the at-risk time-to-empty values deltas leave alone;
//   - between backstops, what changed goes out as per-station deltas, and a
//     station whose style disappeared gets its snapshot at once;
//   - an Edge that (re)registers gets its snapshot then (SnapshotStation), so
//     a reconnecting Pi does not wait for the backstop.
//
// The caller skips it entirely when there is nothing and never was: a plant
// (or a just-booted Core) with no styles has nothing to send, so it does not
// spam every edge — or every test engine's outbox — on start.
func (m *SourceabilityMonitor) publishFullPass(states, moved []sourceability.StyleState, gone map[string]bool, due bool) {
	if due {
		m.publishSnapshots(states, nil)
		return
	}
	m.publishDeltas(moved)
	if gone != nil {
		m.publishSnapshots(states, gone)
	}
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
	// Absent decides the Edge's dry-source guard, so its flip goes out now and
	// not at the next snapshot.
	if !equalStrings(a.Absent, b.Absent) {
		return true
	}
	// THE FLAGGED PAYLOAD LIST, NOT THE COUNTS. Whether a missing payload has
	// stock in an undeclared carrier changes the operator's ACTION, so it is an
	// operator-visible change. How MANY such bins there are is magnitude — the
	// same class as time-to-empty drift, which this function deliberately
	// ignores — and the periodic snapshot carries the current number.
	if !equalStrings(undeclaredPayloads(a), undeclaredPayloads(b)) {
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

// undeclaredPayloads projects the flagged findings onto their payload codes.
// Order is Compute's, which follows the sorted Missing list, so two equal sets
// compare equal without a sort here.
func undeclaredPayloads(s sourceability.StyleState) []string {
	if len(s.UndeclaredCarriers) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.UndeclaredCarriers))
	for _, f := range s.UndeclaredCarriers {
		out = append(out, f.PayloadCode)
	}
	return out
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
		Absent:     s.Absent,
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
		// TWO FACTS, TWO SENTENCES. "There is no stock" and "the stock is in a
		// carrier the part may not travel in" have different actions — make
		// more, versus resolve the bin type — and an operator who reads them as
		// one sentence takes the first action for the second problem. The
		// payload stays in the missing list because it genuinely is missing:
		// every sourcing reader refuses that carrier. The second sentence says
		// why, and names the person who can fix it by naming the fix.
		return "Cannot change over — missing " + strings.Join(s.Missing, ", ") + "." +
			undeclaredCarrierSentence(s.UndeclaredCarriers)
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

// undeclaredCarrierSentence is the second half of a RED reason, and it is empty
// on every plant where nothing is flagged — which is the ordinary state and
// leaves the sentence byte-identical to what it was before this existed.
//
// It names the COUNT as well as the payload because "resolve bin type" is a
// different-sized job against one carrier and against thirty, and it names the
// action rather than a person: the fix is either moving the parts into a
// declared carrier or declaring this one on the payload, and which of those is
// right is not something Core knows.
func undeclaredCarrierSentence(flagged []sourceability.PayloadCount) string {
	if len(flagged) == 0 {
		return ""
	}
	parts := make([]string, 0, len(flagged))
	for _, f := range flagged {
		unit := "bins"
		if f.Bins == 1 {
			unit = "bin"
		}
		parts = append(parts, fmt.Sprintf("%d %s of %s", f.Bins, unit, f.PayloadCode))
	}
	return " " + strings.Join(parts, ", ") +
		" in undeclared carriers — not sourceable; resolve bin type."
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

// SourceabilityEvents returns recorded verdict changes since `since`, newest
// first — the persisted half of the diff this monitor computes. processID /
// payload "" mean "all".
//
// "SNF2 went unsourceable at 09:14 missing SYN-PART02B.06, recovered 09:41" is the
// sentence this answers, and the substrate for starvation minutes.
func (e *Engine) SourceabilityEvents(since time.Time, processID, payload string, limit int) ([]domain.SourceabilityEvent, error) {
	return e.db.ListSourceabilityEvents(since, processID, payload, limit)
}
