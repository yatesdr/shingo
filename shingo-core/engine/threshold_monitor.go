// threshold_monitor.go — UOP-threshold replenishment, Core side.
// See shingo/docs/uop-threshold-replenishment.md for the design
// overview.
//
// The C-push architecture in one paragraph:
//
//   Core owns loader config (the bin_loaders aggregate) and the per-(loader,
//   payload) thresholds derived from it. On any activity for a monitored
//   payload — a BinUOPDelta, a LinesideBucketLevel, or a non-delta bin
//   mutation — Core re-reads the AUTHORITATIVE combined in-loop UOP for
//   that payload (SystemUOPForPayload = SUM(bins.uop_remaining) + active
//   lineside piles) and evaluates it against the configured threshold.
//   When the total is below the threshold for a (loader, payload) pair,
//   Core creates the retrieve orders itself (see fireSignalCached —
//   the 2026-07-31 cutover). Nothing replenishment-related crosses the
//   wire to Edge any more.
//
// Reads the source of truth, holds no private tally. The monitor used to
// keep its own incremental in-memory UOP total (uopCache), moved by each
// delta and re-baselined by a 60s reconcile sweep. That tally was a
// second copy of the same number that could — and did — silently drift
// from DB truth (Springfield 2026-07-21: cache stuck ~139 while DB was
// 31, so a threshold nudge fired nothing; 2026-07-23: cache stuck high
// after a direct-DB reassign suppressed ordering). F-1 benchmarked the
// authoritative read at ~0.43 ms/payload at plant scale, so the monitor
// now just READS it on every evaluation. That deletes the drift failure
// category outright: there is no cached belief left to diverge.
//
// Debounce policy: 15-second window per place — (core_node_name, payload),
// the episode key's grain. In-memory state (lost on Core restart — that's
// intentional; the startup sweep handles the restart case). The debounce timer
// is reset when the threshold value for the place changes, so a newly-applied
// threshold engages immediately.
//
// Startup sweep: on Run() the monitor walks every binding with threshold > 0
// once, seeds the cold-start warm-up allowance, and evaluates each against the
// authoritative DB read. There is no ongoing reconcile sweep for the level —
// every evaluation already reads the truth.
//
// IT KEEPS NO COPY OF ANYTHING A TABLE HOLDS. Which places are monitored, at
// what threshold, comes from demand_registry on every evaluation; whether a
// place already has an open demand, and which one, comes from demand_origins
// on every evaluation. Memory holds only what this process is doing: debounce
// and warm-up timers, log throttles, the contradiction chip's stamp. See the
// ThresholdMonitor struct for the history of the copies it used to hold.
//
// There is no longer a second path to dedup against. The legacy bin-count
// DemandSignal route was removed entirely (2026-08): Core no longer emits
// it and no handler exists on Edge, so the two-signals-race case this used
// to describe cannot happen. Core still sends no LoopBelowThresholdSignal
// for a pair with threshold = 0 — that pair is simply not monitored, and its
// loader is stocked by the operator push instead.
//
// WHAT DEDUPS THE ORDERS IS THE EPISODE, and it is worth saying here because
// this comment used to name the Edge's reservation seam (withLoaderBudget),
// which was deleted with the Edge's half of replenishment. For a while the
// answer to "what stops this firing twice for the same demand" was a function
// that no longer existed, and the real answer was "nothing" — Springfield
// 2026-08-03, 241 duplicate orders at one window. The debounce below is a rate
// limit, not a dedup: it decides how often it is worth ASKING, never whether the
// ask is already outstanding. That question is answered in
// dispatch.ReplenishLoader, which subtracts the episode's own live orders from
// the ask before creating any.
//
// Out of scope: iterate-all-claims for inactive styles (R3),
// queued-retrieve safety net at Edge.

package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/dispatch"
	"shingocore/store/demands"
)

// thresholdDebounceWindow is the per-(loader, payload) suppression
// window for order creation (it guarded the LoopBelowThresholdSignal
// this monitor used to send). v5 brief: 15 seconds. Faster
// than v4's 30s for legitimate-crossing response, still long enough to
// absorb bursts from rapid bin-move / bucket-delta sequences.
const thresholdDebounceWindow = 15 * time.Second

// warmUpFloor is the floor in the per-binding warm-up cap formula
// max(2, ceil(threshold / C)). The capacity C is per-claim and isn't
// trivially queryable from Core, so for Phase 1 we apply only the
// floor — at least 2 fires on cold start so Springfield's fresh-
// start scenario lands one bin in supermarket + one in flight while
// the line consumes the initial bin. A later phase can lift C from
// claim config and apply the ceiling.
const warmUpFloor = 2

// negativeLogWindow throttles the negative-count warning line. The warning is
// evaluated on every incoming delta — i.e. every consume tick — so an
// unthrottled log would emit per binding per tick and bury the plant log in
// the one situation where an operator most needs to read it. Once a minute per
// binding is enough to keep the condition visible without becoming the noise.
const negativeLogWindow = 60 * time.Second

// swapContradictionWindow is how long a manual-swap-vs-ledger contradiction
// (P2-C9) stays surfaced as a Replenishment Health chip, and the throttle
// window for its log line. A human requesting a swap for a payload the ledger
// reads as fully stocked is a standing condition worth showing for a while, not
// a per-request event.
const swapContradictionWindow = 15 * time.Minute

// thresholdEntry is one monitored place — (core_node_name, payload) — with its
// configured threshold, read from demand_registry for the evaluation that
// holds it and dropped afterwards. The station is data on the entry (the
// order's station, the episode's station, the log line's station), not part
// of what identifies the place.
type thresholdEntry struct {
	stationID    string
	coreNodeName string
	payloadCode  string
	threshold    int
	loaderID     int64 // the owning loader (cutover); 0 for legacy pre-cutover bindings → no LoaderKey on the signal
}

// ThresholdMonitor creates retrieve orders when a monitored place drops below
// its configured threshold. It no longer emits a wire signal — the
// LoopBelowThresholdSignal it used to send was deleted with the Edge's half of
// replenishment (2026-08-02); see fireSignalCached.
//
// WHAT IT HOLDS IS WHAT IT IS DOING, AND NOTHING A TABLE KNOWS. Four copies
// have come off this struct, and all four ran the same arc: introduced to save
// a hot-path read, then fixed at the doors that forgot to update them, then
// bounded by a reconciler or a rehydrate, then deleted. The uopCache tally went
// first (1e8d542c, net -145 lines). belowThresholdSince went next — its key set
// was openOrigins' and its value had no reader. Then thresholdsByPayload, a
// copy of demand_registry that kept ordering against a withdrawn binding until
// a sweep noticed (Springfield 2026-08-19), and openOrigins, a copy of the open
// rows in demand_origins that kept stamping an origin another pass had closed
// (865 threshold episodes, 195 orders). Both are now read at the point of
// decision: one indexed lookup of the payload's bindings, one indexed probe of
// the place's open episode. Before adding a rehydrate, resync or reconcile to a
// field here, `git log -S` the fields that are gone.
type ThresholdMonitor struct {
	eng *Engine

	// fireHook intercepts a fired signal instead of sending it. Test seam
	// only, same pattern as SourceabilityMonitor's publishFn: nil in
	// production, so the send path below is the only one that ever runs. It
	// exists because fireSignalCached dereferences eng, which the pure unit
	// harness leaves nil — without it, "did this fire?" is only answerable by
	// standing up a whole engine.
	//
	// It is handed the origin the fire would stamp, because "which demand does
	// this order belong to" is half of what a fire decides.
	fireHook func(b thresholdEntry, total int, reason, originID string)

	mu sync.Mutex
	// debounce is the last-fired timestamp per place. An order batch is only
	// created when now > debounce[key] + thresholdDebounceWindow.
	debounce map[string]time.Time
	// warmUp tracks remaining cold-start fires per place. Decremented each time
	// the monitor fires; once at zero, normal debounced operation continues.
	// Seeded by the startup sweep.
	warmUp map[string]int
	// negativeLogged is the last time the negative-count warning was logged
	// per place. Deliberately SEPARATE from debounce: debounce is
	// signal-eligibility budget, this is log volume. Sharing one stamp would
	// mean a negative total consumed the place's right to fire the moment
	// the ledger was corrected.
	negativeLogged map[string]time.Time
	// duplicateLogged is the last time the DUPLICATE BINDING line was logged
	// per place. Log volume only, like negativeLogged: the collision it reports
	// is a standing config condition read on every evaluation.
	duplicateLogged map[string]time.Time
	// swapContradiction is the last time a manual-swap request arrived for a
	// payload the ledger read as fully stocked (P2-C9). Keyed by payload_code;
	// drives the Replenishment Health contradiction chip and throttles the log.
	swapContradiction map[string]time.Time

	// now supplies every timestamp this monitor measures an interval against.
	//
	// It exists because all four of them used to call bare time.Now() while
	// everything they gate moves in SIM time: sim startup installs a
	// fast-forward clock globally (clock.BuildSimClock → clock.SetDefault,
	// cmd/shingocore/sim_enabled.go:47,61) and the rest of the engine reads
	// clock.Now(). So in the sim the 15s debounce, the 60s negative-log window
	// and the 15-minute contradiction window all ran on WALL time while the
	// activity they throttle ran 15× faster — one debounce covering fifteen
	// times more simulated work than it would at a plant. The hysteresis
	// margins get tuned on that sim.
	//
	// Defaults to clock.Now, which IS time.Now in production. Tests that need
	// to drive it set the field; the ones that back-date the maps directly
	// (threshold_monitor_test.go) keep working untouched.
	now func() time.Time
}

// NewThresholdMonitor constructs the monitor. Call Run() to perform
// the startup sweep.
func NewThresholdMonitor(e *Engine) *ThresholdMonitor {
	return &ThresholdMonitor{
		eng:               e,
		debounce:          make(map[string]time.Time),
		warmUp:            make(map[string]int),
		negativeLogged:    make(map[string]time.Time),
		duplicateLogged:   make(map[string]time.Time),
		swapContradiction: make(map[string]time.Time),
		now:               clock.Now,
	}
}

// nowFn returns the monitor's clock, falling back to the shared default.
//
// The fallback is for monitors built as a struct literal rather than through
// NewThresholdMonitor — the pure unit harness does exactly that — so a
// zero-value monitor keeps working instead of panicking on a nil func.
func (m *ThresholdMonitor) nowFn() time.Time {
	if m.now != nil {
		return m.now()
	}
	return clock.Now()
}

// MonitorBinding is one monitored (station, node, payload) threshold binding,
// exported for the Snapshot read model behind the inventory Replenishment
// Health page.
type MonitorBinding struct {
	StationID    string `json:"station_id"`
	CoreNodeName string `json:"core_node_name"`
	Threshold    int    `json:"threshold"`
	LoaderID     int64  `json:"loader_id,omitempty"`
}

// MonitorSnapshotEntry is the set of threshold bindings watching one payload —
// which loaders monitor it, at what threshold. It carries no UOP total: the
// monitor holds no cached belief, so the caller reads DB truth
// (SystemUOPForPayload) directly for the on-hand number.
type MonitorSnapshotEntry struct {
	PayloadCode string           `json:"payload_code"`
	Bindings    []MonitorBinding `json:"bindings"`
	// SwapContradiction is true when a manual-swap request arrived for this
	// payload within swapContradictionWindow while the ledger read it as fully
	// stocked (P2-C9). Surfaced as a Replenishment Health chip.
	SwapContradiction bool `json:"swap_contradiction"`
}

// Snapshot returns which payloads are monitored and the binding set watching
// each — the read model behind the inventory Replenishment Health page. It
// reports only the monitored set and thresholds; the caller reads DB on-hand
// itself.
//
// THE BINDINGS ARE READ, NOT REMEMBERED: one ListDemandThresholds per call.
// This page is what an engineer opens to ask "is this loader monitored, at
// what threshold?", and while it rendered a copy, the answer during the
// 2026-08-19 burst would have been the copy's. It shows every registry row,
// duplicates included, because the page is where a duplicate should be seen.
// The contradiction chip is the one thing here no table holds.
//
// A read error is returned, not rendered as "nothing is monitored".
func (m *ThresholdMonitor) Snapshot() ([]MonitorSnapshotEntry, error) {
	if m.eng == nil || m.eng.db == nil {
		return nil, nil
	}
	entries, err := m.eng.db.ListDemandThresholds()
	if err != nil {
		return nil, err
	}
	byPayload := map[string][]MonitorBinding{}
	var order []string
	for _, e := range entries {
		if e.ReplenishUOPThreshold <= 0 {
			continue
		}
		if _, seen := byPayload[e.PayloadCode]; !seen {
			order = append(order, e.PayloadCode)
		}
		byPayload[e.PayloadCode] = append(byPayload[e.PayloadCode], MonitorBinding{
			StationID:    e.StationID,
			CoreNodeName: e.CoreNodeName,
			Threshold:    e.ReplenishUOPThreshold,
			LoaderID:     e.LoaderID,
		})
	}
	now := m.nowFn()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MonitorSnapshotEntry, 0, len(order))
	for _, payload := range order {
		contradiction := false
		if last, ok := m.swapContradiction[payload]; ok && now.Sub(last) < swapContradictionWindow {
			contradiction = true
		}
		out = append(out, MonitorSnapshotEntry{
			PayloadCode:       payload,
			Bindings:          byPayload[payload],
			SwapContradiction: contradiction,
		})
	}
	return out, nil
}

// placeKey is the grain of everything the monitor keys: the episode key,
// (core_node_name, payload). Debounce, warm-up and the log throttles use it,
// and so does demand_origins' partial unique index — so two registry rows for
// one place are one debounce budget and one demand, not two.
func placeKey(coreNodeName, payload string) string {
	return protocol.ThresholdEpisodeKey(coreNodeName, payload)
}

// Run performs the startup sweep then returns. Idempotent — calling
// twice is harmless; the second call is a no-op because the sweep flag
// stays set.
//
// Sweep runs in a goroutine so it doesn't block Engine startup; ordering
// vs. uop_backfill is handled at the cmd/shingocore wiring layer
// (sweep runs after a backfill-completion gate). For Phase 1 the
// monitor itself just waits a short grace period before sweeping.
func (m *ThresholdMonitor) Run(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		m.startupSweep(ctx)
	}()
}

// readTotal reads the authoritative in-loop UOP total for one payload straight
// from the DB (SystemUOPForPayload = SUM(bins.uop_remaining) over the lifecycle
// filter + active lineside piles). This is the single source of truth the
// monitor evaluates on EVERY firing decision; there is no cached belief that
// can drift from it. F-1 benchmarked it at ~0.43 ms/payload at plant scale.
// Returns (0, nil) when there is no engine/inventory service (pure unit
// harness) so callers behave as "zero on-hand" rather than panicking.
func (m *ThresholdMonitor) readTotal(ctx context.Context, payloadCode string) (int, error) {
	if m.eng == nil || m.eng.inventoryService == nil {
		return 0, nil
	}
	uop, err := m.eng.inventoryService.SystemUOPForPayload(ctx, []string{payloadCode})
	if err != nil {
		return 0, err
	}
	if len(uop.Counts) > 0 {
		return uop.Counts[0].TotalUOP, nil
	}
	return 0, nil
}

// evaluatePayload reads a payload's monitored places and, if it has any, the
// authoritative total, and checks each place — the single entry point the
// delta hot path, the bucket path, the non-delta bin-update path and both
// notification doors funnel through (a lineside report is not one of them: it
// is a checksum, compared on ingest). An unmonitored payload costs one indexed
// lookup that returns nothing and stops BEFORE the total is read.
//
// A READ ERROR IS SIDE-EFFECT-FREE: a failed bindings lookup or a failed total
// logs and evaluates nothing — no open, no close, no order. The next trigger
// re-reads. Nothing here ever decides against a level it did not read.
func (m *ThresholdMonitor) evaluatePayload(payloadCode, reason string) {
	if payloadCode == "" || m.eng == nil || m.eng.db == nil {
		return
	}
	places, err := m.monitoredPlaces(payloadCode)
	if err != nil {
		m.eng.logFn("threshold_monitor: bindings for %s: %v (evaluating nothing)", payloadCode, err)
		return
	}
	if len(places) == 0 {
		return
	}
	total, ok := m.decisionTotalFor(context.Background(), payloadCode, "evaluate")
	if !ok {
		return
	}
	m.checkBindings(places, total, reason)
}

// decisionTotalFor reads the total a fire is judged against: Core's count,
// SystemUOPForPayload — bins plus active lineside piles, the replica the
// Edge's bin deltas and pile levels keep; a stranded pile never counts. Every path into checkBindings calls it, so the boot
// pass, a manual-swap recheck, the notification doors and the delta path judge
// a payload against the same number.
//
// ONE COUNT, AND IT IS CORE'S (seat-count round 1 §5, 2026-09-23; the owner
// ruled that loaders are a Core function). From 2026-07-24 the default was to
// decide off an Edge-report-adjusted total instead (R1, the
// lineside_decision_mode knob). That blend is deleted: the Edge's count is
// Core's seed plus the Edge's ticks, and the ticks are what the delta and
// level streams carry into this total. The Edge's lineside report is now a checksum Core
// compares on ingest (service/lineside_divergence.go); a disagreement opens a
// report_divergence episode and decides nothing. Rolling back is the previous
// build, not a knob.
//
// ok=false means "evaluate nothing": a total that could not be read decides
// nothing — no open, no close, no order — at every path. site names the caller
// in the log line.
func (m *ThresholdMonitor) decisionTotalFor(ctx context.Context, payload, site string) (int, bool) {
	total, err := m.readTotal(ctx, payload)
	if err != nil {
		if m.eng != nil {
			m.eng.logFn("threshold_monitor: %s SystemUOPForPayload(%s): %v (deciding nothing)", site, payload, err)
		}
		return 0, false
	}
	return total, true
}

// monitoredPlaces reads the payload's bindings from demand_registry (one
// indexed lookup, threshold > 0) and collapses them to one entry per place.
func (m *ThresholdMonitor) monitoredPlaces(payloadCode string) ([]thresholdEntry, error) {
	entries, err := m.eng.db.LookupDemandThresholdsByPayload(payloadCode)
	if err != nil {
		return nil, err
	}
	return m.collapsePlaces(entries), nil
}

// collapsePlaces turns registry rows into one binding per place.
//
// ONE PLACE IS ONE DEMAND, whatever the registry says. demand_registry is a
// plant-wide derivation copied once per station, so the same (node, payload)
// can appear under two station ids — Hopkinsville holds every one of its
// pairs twice, one copy under a station last heard from in August. Treating
// each row as a binding gave one place two debounce budgets and two mints, the
// second of which the partial unique index refused on every evaluation, and
// the refused one then fired with no origin at all.
//
// The row that stands for the place is the first by station id, so the choice
// is the same on every evaluation and every restart. The collision is logged,
// throttled per place, because it is two configs claiming one place and a
// person should decide which is real.
func (m *ThresholdMonitor) collapsePlaces(entries []demands.RegistryEntry) []thresholdEntry {
	byPlace := map[string][]demands.RegistryEntry{}
	var order []string
	for _, e := range entries {
		if e.ReplenishUOPThreshold <= 0 {
			continue
		}
		key := placeKey(e.CoreNodeName, e.PayloadCode)
		if _, seen := byPlace[key]; !seen {
			order = append(order, key)
		}
		byPlace[key] = append(byPlace[key], e)
	}
	out := make([]thresholdEntry, 0, len(order))
	for _, key := range order {
		rows := byPlace[key]
		sort.Slice(rows, func(i, j int) bool { return rows[i].StationID < rows[j].StationID })
		if len(rows) > 1 {
			m.logDuplicateBinding(key, rows)
		}
		e := rows[0]
		out = append(out, thresholdEntry{
			stationID:    e.StationID,
			coreNodeName: e.CoreNodeName,
			payloadCode:  e.PayloadCode,
			threshold:    e.ReplenishUOPThreshold,
			loaderID:     e.LoaderID,
		})
	}
	return out
}

// logDuplicateBinding names a place bound under more than one station, at most
// once per swapContradictionWindow per place.
func (m *ThresholdMonitor) logDuplicateBinding(key string, rows []demands.RegistryEntry) {
	now := m.nowFn()
	m.mu.Lock()
	last, seen := m.duplicateLogged[key]
	if seen && now.Sub(last) < swapContradictionWindow {
		m.mu.Unlock()
		return
	}
	m.duplicateLogged[key] = now
	m.mu.Unlock()
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s(threshold=%d loader=%d)", r.StationID, r.ReplenishUOPThreshold, r.LoaderID))
	}
	m.eng.logFn("threshold_monitor: DUPLICATE BINDING node=%s payload=%s stations=%s — one place, %d registry rows; evaluating it once as %s. Two configs claim this place: decide which is real (further occurrences suppressed for %s)",
		rows[0].CoreNodeName, rows[0].PayloadCode, strings.Join(parts, ","), len(rows), rows[0].StationID, swapContradictionWindow)
}

// startupSweep evaluates every monitored place once at boot, seeding the
// cold-start warm-up allowance for the ones already below threshold.
//
// IT READS THE OPEN SET ONCE and hands it down for this one pass, rather than
// probing demand_origins per place: ListOpenThresholdEpisodes is one statement
// for the whole plant. The map lives for this function call and no longer, so
// it is not a copy anything else can consult. It is also why a restart does not
// mint a second demand for a place that is still hungry — the open row is read
// before anything is decided. There used to be a rehydrate here that loaded
// the same rows into a map the monitor then trusted for the life of the
// process; that map is gone.
//
// A read failure — the bindings, the open set — evaluates nothing, and the
// next delta re-reads. A place that is below threshold and receives no delta
// stays unevaluated until one arrives or a door (reconnect, config edit) asks.
func (m *ThresholdMonitor) startupSweep(ctx context.Context) {
	entries, err := m.eng.db.ListDemandThresholds()
	if err != nil {
		m.eng.logFn("threshold_monitor: startup sweep ListDemandThresholds: %v", err)
		return
	}
	open, err := m.eng.db.ListOpenThresholdEpisodes()
	if err != nil {
		m.eng.logFn("threshold_monitor: startup sweep list open demand episodes: %v (evaluating nothing)", err)
		return
	}
	openByKey := make(map[string]string, len(open))
	for _, o := range open {
		openByKey[placeKey(o.CoreNodeName, o.PayloadCode)] = o.OriginID
	}
	m.eng.logFn("threshold_monitor: startup sweep — evaluating %d monitored bindings, %d demand(s) already open", len(entries), len(open))

	byPayload := map[string][]demands.RegistryEntry{}
	var payloads []string
	for _, e := range entries {
		if e.ReplenishUOPThreshold <= 0 {
			continue
		}
		if _, seen := byPayload[e.PayloadCode]; !seen {
			payloads = append(payloads, e.PayloadCode)
		}
		byPayload[e.PayloadCode] = append(byPayload[e.PayloadCode], e)
	}

	for _, payload := range payloads {
		if ctx.Err() != nil {
			return
		}
		places := m.collapsePlaces(byPayload[payload])
		total, ok := m.decisionTotalFor(ctx, payload, "startup sweep")
		if !ok {
			continue
		}
		// Warm-up is the sweep's own cold-start concern (Springfield's fresh
		// start wants one bin in the supermarket and one in flight), not a fire
		// decision, and it is seeded only for places below threshold. It must be
		// seeded BEFORE the decision because allow() is what consumes it.
		//
		// It is also the one thing a restart changes about WHEN Core orders:
		// warmUpFloor fires per hungry place bypass the debounce window, the
		// sweep's own and one more. Both carry the open origin read above, so
		// dispatch.ReplenishLoader subtracts the first from the second.
		m.mu.Lock()
		for _, b := range places {
			if total < b.threshold {
				m.warmUp[placeKey(b.coreNodeName, b.payloadCode)] = warmUpFloor
			}
		}
		m.mu.Unlock()
		m.decide(places, total, "warm_up_startup_sweep", openByKey)
	}
	m.eng.logFn("threshold_monitor: startup sweep complete — evaluating from authoritative DB reads")
}

// OnBinUOPDelta applies a bin UOP delta to the cached total and checks
// thresholds. Called by HandleBinUOPDelta after the delta has been applied to
// bins.uop_remaining. The delta value is retained for interface compatibility
// with the messaging layer but no longer drives the math: the monitor re-reads
// the authoritative sum (which already reflects the just-applied write) instead
// of moving a private tally. Unmonitored/empty payloads short-circuit before
// the read.
func (m *ThresholdMonitor) OnBinUOPDelta(payloadCode string, delta int) {
	m.evaluatePayload(payloadCode, "below_threshold")
}

// OnBucketApplied is invoked by the messaging layer after a lineside pile
// level is applied: a level changes on-hand (an active pile counts), so it
// re-reads the authoritative sum and checks thresholds, short-circuiting for
// unmonitored or empty payloads.
func (m *ThresholdMonitor) OnBucketApplied(payloadCode string) {
	m.evaluatePayload(payloadCode, "below_threshold")
}

// handleBinUpdated is the EventBinUpdated subscriber for non-delta bin
// mutations (status changes, manual moves, corrections). These events don't
// carry a UOP delta, so — like every other evaluation now — it re-reads the
// authoritative sum for the payload and checks thresholds.
func (m *ThresholdMonitor) handleBinUpdated(ev BinUpdatedEvent) {
	m.evaluatePayload(ev.PayloadCode, "below_threshold")
}

// NoteSwapRequestContradiction is the P2-C9 contradiction check. Called when a
// manual swap request (a complex order — the shape the incident's operator
// swap took) arrives for a payload. If the monitor's ledger reads that payload
// as fully stocked (in-loop total at or above its highest binding threshold)
// yet a human at the line is requesting a swap, that is a contradiction — the
// SNF3 CARRIER-0024 shape, where Core held a 150-UOP phantom on-hand while the
// operator's tile read 46 and the line starved. It logs a
// manual_request_vs_ledger warning, raises a Replenishment Health chip, and
// immediately re-evaluates the payload.
//
// With the private tally gone, "re-evaluate" is simply "re-read" — the only
// mode there is now. It creates NO orders: the re-read fires the normal
// (debounced) signal only if the ledger is genuinely below threshold, which is
// the non-contradiction case; when it reads stocked, nothing fires. The
// contradiction log/chip is throttled to once per swapContradictionWindow per
// payload, so a burst of complex orders can't spam it.
func (m *ThresholdMonitor) NoteSwapRequestContradiction(payloadCode string) {
	if payloadCode == "" || m.eng == nil || m.eng.db == nil {
		return
	}
	bindings, err := m.monitoredPlaces(payloadCode)
	if err != nil {
		m.eng.logFn("threshold_monitor: NoteSwapRequestContradiction bindings for %s: %v", payloadCode, err)
		return
	}
	if len(bindings) == 0 {
		return
	}
	total, ok := m.decisionTotalFor(context.Background(), payloadCode, "NoteSwapRequestContradiction")
	if !ok {
		return
	}
	// THE CONTRADICTION IS ABOUT THE LEDGER: a human is asking for material the
	// ledger says is there. The ledger is also the total every fire decision
	// reads, so the chip and the recheck below judge the same number.
	maxThreshold := 0
	for _, b := range bindings {
		if b.threshold > maxThreshold {
			maxThreshold = b.threshold
		}
	}
	if maxThreshold > 0 && total >= maxThreshold {
		if m.recordSwapContradiction(payloadCode) && m.eng != nil {
			m.eng.logFn("threshold_monitor: manual_request_vs_ledger — swap requested for payload=%s while the ledger reads STOCKED (in-loop total=%d >= max binding threshold=%d); the line may be starving behind a phantom on-hand — check this payload's bins for a stale staged bin (further occurrences suppressed for %s)",
				payloadCode, total, maxThreshold, swapContradictionWindow)
		}
	}
	// Immediately re-evaluate — a re-read now. Creates no orders when stocked.
	m.checkBindings(bindings, total, "manual_swap_recheck")
}

// recordSwapContradiction stamps a swap-vs-ledger contradiction for the payload
// if the last one is outside the window, returning whether it was newly
// recorded (i.e. should be logged now). Fixed-window throttle: the log fires
// once per window and the chip reads the stamp for the rest of it.
func (m *ThresholdMonitor) recordSwapContradiction(payloadCode string) bool {
	now := m.nowFn()
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.swapContradiction[payloadCode]; ok && now.Sub(last) < swapContradictionWindow {
		return false
	}
	m.swapContradiction[payloadCode] = now
	return true
}

// checkBindings evaluates all threshold bindings for a given total and
// fires signals for any that are below threshold and past debounce, reading
// each place's open episode from demand_origins as it goes.
func (m *ThresholdMonitor) checkBindings(bindings []thresholdEntry, total int, reason string) {
	m.decide(bindings, total, reason, nil)
}

// decide is checkBindings with an optional open set. A nil open set means
// "probe demand_origins per place" — every caller but the startup sweep, which
// has read the whole open set once and passes it for its one pass.
func (m *ThresholdMonitor) decide(bindings []thresholdEntry, total int, reason string, open map[string]string) {
	// No database, no episode, and so no decision: every edge below reads
	// demand_origins, and an order with no episode is refused anyway. Only the
	// pure unit harness builds a monitor without one.
	if m.eng == nil || m.eng.db == nil {
		return
	}
	// A NEGATIVE TOTAL NO LONGER SUPPRESSES REPLENISHMENT.
	//
	// It used to. The reasoning was "a negative total is a broken ledger, so
	// refusing to act on it is input validation" — and that is exactly
	// backwards on a plant floor.
	//
	// What a negative count actually means, per the people who run the line:
	// a press overpacked, or a fork truck delivered parts outside ShinGo and
	// nobody told it, or some other human intervention it cannot see. It is a
	// data-quality problem for a person to look at. It is NOT a reason to stop
	// the plant.
	//
	// And the direction is wrong too. A negative reading is too LOW, not too
	// high — so the honest response to it is "order material", which is what
	// the threshold check below already does. Suppressing instead produced the
	// worst possible pairing: a number saying the line is empty, and a system
	// answering by ordering nothing. Springfield logged that refusal 1,119
	// times a DAY, and it is the first link in the 2026-07-21 chain —
	// ledger negative, replenishment silent, payload genuinely dry, changeover
	// arming onto a dry source.
	//
	// So: fall through and evaluate normally. The count still gets flagged for
	// a human — loudly here, and as an exception row on the inventory page —
	// but the line keeps getting material while they sort it out. Over-ordering
	// is recoverable. Starving a line because a count was wrong is not.
	if total < 0 {
		for _, b := range bindings {
			if b.threshold <= 0 {
				continue
			}
			// Throttled per binding: this runs on every incoming delta, so an
			// unthrottled line would bury the plant log exactly when it needs
			// reading. shouldLogNegative touches ONLY negativeLogged — the
			// binding's debounce budget is untouched.
			if !m.shouldLogNegative(placeKey(b.coreNodeName, b.payloadCode)) {
				continue
			}
			m.eng.logFn("threshold_monitor: NEGATIVE COUNT station=%s loader=%s payload=%s — in-loop total is %d (threshold %d); the bins ledger for this payload is wrong (overpack, an untracked delivery, or a manual move) and needs a recount. Replenishment CONTINUES on this reading — a wrong count must not starve the line (further occurrences suppressed for %s)",
				b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold, negativeLogWindow)
		}
		// Deliberately NO return — fall through to the normal evaluation.
	}
	for _, b := range bindings {
		if b.threshold <= 0 {
			continue
		}
		key := placeKey(b.coreNodeName, b.payloadCode)
		// READ, THEN WRITE. One indexed probe answers every question the edges
		// ask — is an episode open here, which one, what does a fire stamp — so
		// nothing can disagree with the row it describes. A failed read decides
		// nothing: no open, no close, no order.
		originID, err := m.openOrigin(key, open)
		if err != nil {
			m.eng.logFn("threshold_monitor: read open demand for %s: %v (deciding nothing)", key, err)
			continue
		}
		if total >= b.threshold {
			// THE RISING EDGE. Until the demand grain existed this branch did
			// nothing at all — recovery was simply the absence of firing, which
			// is why there was no way to say when a demand ENDED, and therefore
			// no way to say what one had cost.
			if originID != "" {
				m.closeThresholdEpisodeByID(originID, key, protocol.CloseReasonRecovered, protocol.ClosedByNotification)
			}
			continue
		}
		// THE FALLING EDGE, AND IT IS MINTED BEFORE THE DEBOUNCE GATE ON
		// PURPOSE. The episode is the DEMAND; the fire is the ACTION taken
		// about it. Debounce decides how often it is worth acting — it must not
		// decide whether the need is recorded, or a demand that fired once and
		// then stayed suppressed for hours would look like it lasted an
		// instant. The episode opens when the place goes hungry.
		if originID == "" {
			originID = m.openThresholdEpisode(key, b, total)
		}
		// NO EPISODE, NO ORDER. An order with no origin is one the next ask
		// cannot subtract — dispatch.ReplenishLoader counts the episode's live
		// orders by origin, and a blank one counts nothing — which is how
		// never-2N's sizing arm comes undone (Springfield 2026-08-03, 241
		// orders at one window). openThresholdEpisode has already logged why.
		if originID == "" {
			continue
		}
		if !m.allow(key) {
			m.eng.dbg("threshold_monitor: suppress station=%s loader=%s payload=%s total=%d threshold=%d (debounce)",
				b.stationID, b.coreNodeName, b.payloadCode, total, b.threshold)
			continue
		}
		m.fireSignalCached(b, total, reason, originID)
	}
}

// openOrigin is the open episode for a place: from the pass's open set when
// the caller read one, from demand_origins otherwise. "" means none is open.
func (m *ThresholdMonitor) openOrigin(key string, open map[string]string) (string, error) {
	if open != nil {
		return open[key], nil
	}
	return m.eng.db.OpenOriginForKey(key)
}

// shouldLogNegative reports whether the broken-ledger refusal should be logged
// for this binding now, stamping it when it should. Pure log-volume control —
// it never touches debounce or warm-up, so refusing to signal on a garbage
// total costs the binding nothing once the total is real again.
func (m *ThresholdMonitor) shouldLogNegative(key string) bool {
	now := m.nowFn()
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, seen := m.negativeLogged[key]; seen && now.Sub(last) < negativeLogWindow {
		return false
	}
	m.negativeLogged[key] = now
	return true
}

// allow returns true if the binding may fire now under the debounce
// + warm-up policy. Records the firing time on success so a follow-up
// call within the window returns false.
func (m *ThresholdMonitor) allow(key string) bool {
	now := m.nowFn()
	m.mu.Lock()
	defer m.mu.Unlock()
	if w, ok := m.warmUp[key]; ok && w > 0 {
		m.warmUp[key] = w - 1
		m.debounce[key] = now
		return true
	}
	last, seen := m.debounce[key]
	if seen && now.Sub(last) < thresholdDebounceWindow {
		return false
	}
	m.debounce[key] = now
	return true
}

// fireSignalCached decides a loader's replenishment from a cached threshold
// entry. checkBindings is its only caller — the startup sweep used to reach it
// directly with a thresholdEntry it built inline, and stopped, so that every
// fire decision goes through one set of guards (see startupSweep).
//
// THIS IS THE CUTOVER. It used to build a LoopBelowThresholdSignal and send it
// to the Edge, which then worked out how many carriers were needed and where
// they should go. That split is what this whole program exists to end: the two
// halves counted different things, and only one of them could see the plant.
// On 2026-07-31 a loader at Springfield ordered far more carriers than it had
// places to put them, because the half that sized the ask could not see that
// the windows were full.
//
// Core now decides the whole thing. It sizes the ask from the same reading, it
// resolves which windows may take a carrier, and it creates one order per free
// window — so the bound is the window list, and a reading that asks for a
// hundred carriers at a three-window loader creates three.
//
// This is the ONE construction-and-send site, and every path reaches it through
// checkBindings → allow() → here, so it is post-debounce and the per-bin
// capacity read added below is per-fire rather than per-tick.
func (m *ThresholdMonitor) fireSignalCached(b thresholdEntry, total int, reason, originID string) {
	if m.fireHook != nil {
		m.fireHook(b, total, reason, originID)
		return
	}
	if m.eng == nil || m.eng.dispatcher == nil {
		// The dispatcher is built in Start(); the monitor's startup sweep can in
		// principle beat it. Same nil-guard the wiring uses.
		return
	}
	// Per-bin capacity is the one datum the binding does not already carry. Zero
	// means the catalog has no answer for this part, and ReplenishLoader refuses
	// rather than guessing — a guessed carrier count is how a loader ends up with
	// more carriers than places.
	var perBin int
	if pl, err := m.eng.db.GetPayloadByCode(b.payloadCode); err == nil && pl != nil {
		perBin = pl.UOPCapacity
	}

	cfg, ok, err := m.eng.dispatcher.LoadReplenishConfig(b.loaderID)
	if err != nil {
		m.eng.logFn("threshold_monitor: load loader %d config for %s/%s: %v",
			b.loaderID, b.coreNodeName, b.payloadCode, err)
		return
	}
	if !ok {
		// Legacy binding with no loader id, or a loader that has been archived.
		// Nothing to decide against; refusing is the safe answer and it is not an
		// error worth a line every debounce window.
		m.eng.dbg("threshold_monitor: no loader config for binding %s/%s (loader=%d) — not replenishing",
			b.coreNodeName, b.payloadCode, b.loaderID)
		return
	}

	res, err := m.eng.dispatcher.ReplenishLoader(dispatch.ReplenishRequest{
		StationID:      b.stationID,
		LoaderID:       b.loaderID,
		PayloadCode:    b.payloadCode,
		MemberNode:     b.coreNodeName,
		Threshold:      b.threshold,
		CurrentUOP:     total,
		PerBinCapacity: perBin,
		// The demand episode the orders belong to. On the old wire path this
		// travelled with the LoopBelowThresholdSignal and came back on the
		// Edge's orders; now the order is created here, so it is simply
		// stamped.
		OriginID:    originID,
		OriginClass: string(protocol.OriginClassAttached),
	}, cfg)
	if err != nil {
		m.eng.logFn("threshold_monitor: replenish loader=%d station=%s payload=%s: %v",
			b.loaderID, b.stationID, b.payloadCode, err)
		return
	}
	if res.Skipped != "" {
		m.eng.logFn("threshold_monitor: loader_replenish station=%s loader=%d payload=%s current=%d threshold=%d reason=%s skipped=%s",
			b.stationID, b.loaderID, b.payloadCode, total, b.threshold, reason, res.Skipped)
		return
	}
	m.eng.logFn("threshold_monitor: loader_replenish station=%s loader=%d payload=%s current=%d threshold=%d reason=%s created=%d want=%d held=%v",
		b.stationID, b.loaderID, b.payloadCode, total, b.threshold, reason, len(res.Created), res.Want, res.HeldBy)
}

// OnThresholdChanges is the loader config-edit door. Called by
// service/loader_service.go after the registry derive returns its change list.
// For every place whose threshold value moved it clears the debounce and
// warm-up state, closes the open episode (the denominator moved, or the
// binding went), and evaluates the affected payloads.
//
// It EVALUATES, and does not wait for a delta, because a newly-added or
// raised threshold on a payload with no stock moving would otherwise stay
// silent until something else woke it — the Springfield 6883 case, a threshold
// configured and never triggered because no delta arrived. A failed read here
// is side-effect-free like everywhere else: the door no longer fires off a
// fabricated zero.
func (m *ThresholdMonitor) OnThresholdChanges(changes []demands.RegistryChange) {
	if len(changes) == 0 {
		return
	}
	affected := map[string]bool{}
	m.mu.Lock()
	for _, c := range changes {
		key := placeKey(c.CoreNodeName, c.PayloadCode)
		delete(m.debounce, key)
		delete(m.warmUp, key)
		affected[c.PayloadCode] = true
		if m.eng != nil {
			m.eng.dbg("threshold_monitor: reset debounce station=%s loader=%s payload=%s old=%d new=%d",
				c.StationID, c.CoreNodeName, c.PayloadCode, c.OldThreshold, c.NewThreshold)
		}
	}
	m.mu.Unlock()

	// The denominator moved, so the episode ends here and the evaluation below
	// opens a fresh one if the place is still hungry. Carrying one episode
	// across the change would measure it against a threshold that was not in
	// force for most of its life. BEFORE the evaluation, or it would find the
	// old episode open and the new threshold would never get one of its own.
	m.closeThresholdEpisodesForChangedBindings(changes)

	m.evaluatePayloads(affected)
}

// Resync is the Edge (re)connect door: Core has just re-derived the station's
// registry from the loader aggregate, so every place the station is bound to
// has its debounce and warm-up cleared and is evaluated now — a binding seeded
// or edited while the station was away engages without waiting for a delta.
//
// THERE IS NO DROP HALF ANY MORE, because there is no memory to drop from. A
// binding that vanished while the station was away is simply not in the
// registry, so nothing evaluates it and nothing can mint for it; its open
// episode, if it had one, is closed `threshold_removed` by the reconciling
// sweep (reconcileThresholdBindings), which reads the same table.
//
// A READ FAILURE IS NOT AN EMPTY BINDING SET: it logs and does nothing.
func (m *ThresholdMonitor) Resync(stationID string) {
	if m.eng == nil || m.eng.db == nil {
		return
	}
	entries, err := m.eng.db.ListDemandThresholds()
	if err != nil {
		m.eng.logFn("threshold_monitor: Resync(%s) list thresholds: %v", stationID, err)
		return
	}
	affected := map[string]bool{}
	m.mu.Lock()
	for _, e := range entries {
		if e.StationID != stationID || e.ReplenishUOPThreshold <= 0 {
			continue
		}
		key := placeKey(e.CoreNodeName, e.PayloadCode)
		delete(m.debounce, key)
		delete(m.warmUp, key)
		affected[e.PayloadCode] = true
	}
	m.mu.Unlock()
	if len(affected) == 0 {
		return
	}
	m.eng.logFn("threshold_monitor: Resync station=%s — re-engaging %d monitored payload(s)", stationID, len(affected))
	m.evaluatePayloads(affected)
}

// evaluatePayloads evaluates a door's affected payloads, in sorted order so two
// journals of the same door diff.
func (m *ThresholdMonitor) evaluatePayloads(affected map[string]bool) {
	payloads := make([]string, 0, len(affected))
	for p := range affected {
		payloads = append(payloads, p)
	}
	sort.Strings(payloads)
	for _, p := range payloads {
		m.evaluatePayload(p, "below_threshold")
	}
}
