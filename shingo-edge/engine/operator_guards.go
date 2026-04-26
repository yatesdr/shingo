package engine

import (
	"fmt"

	"shingoedge/orders"
	"shingoedge/store/processes"
)

// guardNoActiveSwap refuses to dispatch a new two-robot cycle on a node when
// the runtime slots (ActiveOrderID / StagedOrderID) still reference orders
// that are non-terminal — another swap is already in motion locally and
// dispatching a second one would race with the first.
//
// Scope is intentionally narrow: this check is based ONLY on Edge's own DB
// (its own dispatched-orders state) — never on Core telemetry. A Core anomaly
// (stale bin telemetry, replication blip, manual move not yet synced) must
// not be allowed to shut down the line. Stuck bins from prior failed cycles
// are surfaced via the multi_bin_at_non_storage_node reconciliation anomaly
// (item 3.1) so operators see them on the diagnostics page and decide whether
// to clear them via admin bin-move — but the operator station still lets them
// drive the line in the meantime.
//
// hasActiveSwap is the disambiguation helper that distinguishes "there's a
// real cycle in flight on this node right now" from "the runtime row still
// has historical pointers to orders that have all gone terminal" — the latter
// falls through (no refusal). See bug-fix-plan-final-dev-d.md item 3.2.
//
// Architectural follow-up: ideally this guard lives at Core (single source of
// truth for dispatched orders), but moving it requires either a protocol
// extension to send Edge runtime state on every ComplexOrderRequest or
// duplicating runtime tracking in Core. Keeping it Edge-side for this ship.
func (e *Engine) guardNoActiveSwap(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim) error {
	if claim == nil {
		return nil // caller already short-circuited on claim==nil; defense.
	}
	if hasActiveSwap(e, runtime) {
		return fmt.Errorf("node %s: two-robot swap already in progress — wait for the current cycle to complete or abort it before requesting more material", node.Name)
	}
	return nil
}

// hasActiveSwap reports whether the runtime slots reference any non-terminal
// order. Pure Edge-DB check — no Core round-trip.
func hasActiveSwap(e *Engine, runtime *processes.RuntimeState) bool {
	if runtime == nil {
		return false
	}
	for _, oidPtr := range []*int64{runtime.ActiveOrderID, runtime.StagedOrderID} {
		if oidPtr == nil {
			continue
		}
		o, err := e.db.GetOrder(*oidPtr)
		if err != nil || o == nil {
			continue
		}
		if !orders.IsTerminal(o.Status) {
			return true
		}
	}
	return false
}
