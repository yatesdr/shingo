// uop_stranded_monitor.go — parked-ticks alarm (P2-C7).
//
// A periodic monitor that watches every consume node for the SNF3 parked-ticks
// class: pending_uop_delta growing across consecutive scan intervals while no
// bin is bound. That is consume ticks piling up unattributed because the
// physical carrier at the line was never bound (F1b's residual / no-match
// backstop, or any producer of a staged-but-unbound node). It is a GROWTH
// condition, NOT a fixed threshold — an idle line (pending flat) never alarms,
// and it clears the moment a bin binds. Detection only: it raises an alarm
// (log + EventUOPStranded + tile chip) and NEVER binds, moves, cancels, or
// re-plans anything.

package engine

import (
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// strandedWindow is the number of consecutive rising samples (taken while
// unbound) required before the parked-ticks alarm fires. It is a SHAPE
// requirement (sustained monotonic growth), not a count/threshold on the pending
// value: a node sitting one unit over any figure never alarms; a node climbing
// every interval does. strandedWindow rises => strandedWindow+1 samples.
const strandedWindow = 3

// strandedScanInterval is how often the monitor samples each node — the
// "flush interval" the growth is measured across. Variable so tests can compress
// it; the growth logic (evalStrandedGrowth) is pure and tested without the loop.
var strandedScanInterval = 60 * time.Second

// strandedNodeState is one node's detector. samples holds the recent
// pending_uop_delta readings taken while unbound (capped at strandedWindow+1);
// the window is "growing" iff those are strictly increasing. alarmed dedups the
// log/emit to once per growth window. since marks when the node first went
// unbound-with-pending, for the staged-age the operator sees. carrier caches the
// physical bin label resolved at fire time so per-scan detail refreshes don't
// re-hit Core.
type strandedNodeState struct {
	samples []int
	alarmed bool
	since   time.Time
	carrier string
}

// evalStrandedGrowth advances one node's detector by a single sample and reports
// (fire, active). fire is true exactly once per window — on the sample where the
// unbound growth streak first spans strandedWindow+1 readings. active is true
// whenever the window is currently grown-and-unbound (drives the tile chip).
// Binding, an empty hold, or a stalled/idle line resets the window — so an idle
// line (pending flat) never escalates and a bind clears it immediately.
func evalStrandedGrowth(st *strandedNodeState, pending int, bound bool, now time.Time) (fire, active bool) {
	if bound || pending <= 0 {
		st.samples = st.samples[:0]
		st.alarmed = false
		st.since = time.Time{}
		return false, false
	}
	if len(st.samples) == 0 {
		st.since = now
	}
	st.samples = append(st.samples, pending)
	if len(st.samples) > strandedWindow+1 {
		st.samples = st.samples[len(st.samples)-(strandedWindow+1):]
	}
	if len(st.samples) < strandedWindow+1 || !strictlyIncreasing(st.samples) {
		// Not (yet) a sustained climb — an idle/flat line lands here and never
		// escalates. Drop the alarmed latch so a later genuine climb re-fires.
		st.alarmed = false
		return false, false
	}
	if !st.alarmed {
		st.alarmed = true
		return true, true
	}
	return false, true
}

func strictlyIncreasing(xs []int) bool {
	for i := 1; i < len(xs); i++ {
		if xs[i] <= xs[i-1] {
			return false
		}
	}
	return true
}

// formatStrandedDetail builds the exact operator sentence the tile renders
// verbatim (P2-C8). carrier "" degrades to a generic subject rather than a blank.
func formatStrandedDetail(carrier string, hours int, coreNodeName string) string {
	if carrier == "" {
		carrier = "A carrier"
	}
	return fmt.Sprintf("%s staged %dh at %s, not bound — Record Count on the bin tab.", carrier, hours, coreNodeName)
}

// strandedMonitor is the live loop. states is keyed by process_node id and is
// touched only from the monitor goroutine (no lock); the alarm results are
// published to the engine's strandedAlarms sync.Map, which the view builder
// reads concurrently.
type strandedMonitor struct {
	eng    *Engine
	states map[int64]*strandedNodeState
}

func (e *Engine) startStrandedMonitor() {
	sm := &strandedMonitor{eng: e, states: map[int64]*strandedNodeState{}}
	go sm.run()
}

func (sm *strandedMonitor) run() {
	ticker := time.NewTicker(strandedScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sm.eng.stopChan:
			return
		case now := <-ticker.C:
			sm.tick(now)
		}
	}
}

func (sm *strandedMonitor) tick(now time.Time) {
	nodes, err := sm.eng.db.ListProcessNodes()
	if err != nil {
		return
	}
	for i := range nodes {
		sm.evaluate(&nodes[i], now)
	}
}

// evaluate advances one node and publishes/clears its alarm.
func (sm *strandedMonitor) evaluate(node *processes.Node, now time.Time) {
	e := sm.eng
	// Only consume cells count parts down against a bound bin; the "Record Count
	// on the bin tab" fix and the consuming-active co-condition are consume-
	// specific. Skip produce / manual_swap / unclaimed nodes and drop any state.
	claim := findActiveClaim(e.db, node)
	if claim == nil || claim.Role != protocol.ClaimRoleConsume || claim.SwapMode == protocol.SwapModeManualSwap {
		delete(sm.states, node.ID)
		sm.clear(node.CoreNodeName)
		return
	}
	runtime, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil || runtime == nil {
		return
	}
	st := sm.states[node.ID]
	if st == nil {
		st = &strandedNodeState{}
		sm.states[node.ID] = st
	}

	fire, active := evalStrandedGrowth(st, int(runtime.PendingUOPDelta), runtime.ActiveBinID != nil, now)
	if !active {
		sm.clear(node.CoreNodeName)
		return
	}

	if fire {
		// Resolve the physical carrier once per window (rare) — cached for the
		// per-scan detail refreshes so a lit tile doesn't hammer Core.
		st.carrier = sm.resolveCarrier(node.CoreNodeName)
	}
	hours := int(now.Sub(st.since).Hours())
	detail := formatStrandedDetail(st.carrier, hours, node.CoreNodeName)
	e.strandedAlarms.Store(node.CoreNodeName, detail)

	if fire {
		// One log/audit line per alarm window (P2-C8) — greppable "parked ticks:".
		log.Printf("parked ticks: %s (pending=%d held, no bin bound)", detail, int(runtime.PendingUOPDelta))
		e.Events.Emit(Event{Type: EventUOPStranded, Payload: UOPStrandedEvent{
			CoreNodeName: node.CoreNodeName,
			Carrier:      st.carrier,
			StagedHours:  hours,
			PendingDelta: int(runtime.PendingUOPDelta),
			Detail:       detail,
		}})
	}
}

// clear removes any active alarm for a node (bind, idle, or role change).
func (sm *strandedMonitor) clear(coreNodeName string) {
	sm.eng.strandedAlarms.Delete(coreNodeName)
}

// resolveCarrier best-effort resolves the physical bin's label at the node from
// Core. Returns "" when Core is unconfigured/unreachable or the node is empty —
// the alarm still fires, just without the carrier label.
func (sm *strandedMonitor) resolveCarrier(coreNodeName string) string {
	if sm.eng.coreClient == nil {
		return ""
	}
	bin, known, err := sm.eng.coreClient.BinAtLineside(coreNodeName)
	if err != nil || !known || bin == nil {
		return ""
	}
	return bin.BinLabel
}

// StrandedAlarmDetail returns the active parked-ticks alarm sentence for a core
// node, or "" when none. Read by the operator-station view builder so the tile
// renders the chip on load and on refresh. Concurrency-safe (sync.Map).
func (e *Engine) StrandedAlarmDetail(coreNodeName string) string {
	if v, ok := e.strandedAlarms.Load(coreNodeName); ok {
		return v.(string)
	}
	return ""
}
