// uop_stranded_monitor.go — the produce seat's hold sweep, on a scan.
//
// A periodic scan of every node: an unbound produce seat with a hold and no
// live order placing a carrier at the node has the hold dropped with a
// hold_dropped row (Engine.sweepProduceHold, produce_hold.go), every scan and
// once at boot (sweepAtBoot). A consume seat holds nothing to watch: a stroke
// with no cart of its part is logged in the ledger (decision 55), which the
// strip shows.

package engine

import (
	"time"

	"shingo/protocol/clock"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// strandedScanInterval is how often the monitor scans. Variable so tests can
// compress it.
var strandedScanInterval = 60 * time.Second

// strandedMonitor is the live loop.
type strandedMonitor struct {
	eng *Engine
}

func (e *Engine) startStrandedMonitor() {
	sm := &strandedMonitor{eng: e}
	go sm.run()
}

// strandedMonitorClock is the clock the monitor's loop takes its ticker from:
// Config.StrandedClock when set, else the process default, read when the loop
// starts (as reconcilerClock).
func (e *Engine) strandedMonitorClock() clock.Clock {
	if e.strandedClock != nil {
		return e.strandedClock
	}
	return clock.Default()
}

// run sweeps once at boot, then scans every node each strandedScanInterval
// until the engine stops.
func (sm *strandedMonitor) run() {
	sm.sweepAtBoot()
	ticker := sm.eng.strandedMonitorClock().NewTicker(strandedScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sm.eng.stopChan:
			return
		case now := <-ticker.C():
			sm.tick(now)
		}
	}
}

// tick scans every node on the Edge once.
//
// FOUR STATEMENTS, WHATEVER THE PLANT LOOKS LIKE: every node, every process,
// the claims of their styles and the nodes' runtime rows. A node whose
// process_id names no row is absent from the map, and NodeClaimSet.Resolve
// answers nil for a nil process. Each unbound holding produce seat adds its
// one placing read, and a drop.
func (sm *strandedMonitor) tick(time.Time) {
	sm.sweepAtBoot()
}

// sweepAtBoot is the scan's sweep, once, as the monitor starts (after the
// engine's managers exist, so a drop's row can be built and queued; the
// outbox drains it once the drainer runs). It is the write-off of every pile
// that stood before an unbound produce seat stopped holding without a carrier
// coming: no migration could write that row, because a migration runs before
// the engine and the outbox.
func (sm *strandedMonitor) sweepAtBoot() {
	nodes, resolve, runtimes, ok := sm.read()
	if !ok {
		return
	}
	for i := range nodes {
		node := &nodes[i]
		sm.eng.sweepProduceHold(node, resolve(node), runtimes[node.ID])
	}
}

// read is a scan's four statements: every node, every process, the claims of
// their styles and the nodes' runtime rows. ok is false when there is nothing
// to scan or a read failed.
func (sm *strandedMonitor) read() (nodes []processes.Node, resolve func(*processes.Node) *processes.NodeClaim, runtimes map[int64]*processes.RuntimeState, ok bool) {
	e := sm.eng
	nodes, err := e.db.ListProcessNodes()
	if err != nil || len(nodes) == 0 {
		return nil, nil, nil, false
	}
	procs, err := e.db.ListProcesses()
	if err != nil {
		return nil, nil, nil, false
	}
	byID := make(map[int64]*processes.Process, len(procs))
	ptrs := make([]*processes.Process, 0, len(procs))
	for i := range procs {
		byID[procs[i].ID] = &procs[i]
		ptrs = append(ptrs, &procs[i])
	}
	claims, err := e.db.NodeClaimsForStyles(store.ClaimStyleIDs(ptrs...))
	if err != nil {
		return nil, nil, nil, false
	}
	runtimes, err = e.db.ProcessNodeRuntimes(nodeIDsOf(nodes))
	if err != nil {
		return nil, nil, nil, false
	}
	resolve = func(node *processes.Node) *processes.NodeClaim {
		return claims.Resolve(byID[node.ProcessID], node, store.ActiveStyleFirst)
	}
	return nodes, resolve, runtimes, true
}
