package engine

import (
	"fmt"

	"shingo/protocol"
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

// guardStyleTransition refuses a material request for the OUTGOING style while a
// changeover is armed on the node's process (A2, hop 2026-07-23). Once a target
// style is set, the active-style claim this request resolved is the style the
// cutover is replacing; firing outgoing-style produce/consume relief into a
// half-cutover line is what stranded the Hopkinsville presses (LK41 relief raced
// the KK21 cutover). The changeover's OWN evac/supply orders are created by
// StartProcessChangeover, not through the RequestNodeMaterial / RequestProduceSwap
// paths this guards, so they are unaffected.
//
// Loaders (manual_swap) are exempt: they supply empties ACROSS a changeover and
// must stay available — gating them on the process changeover is exactly the
// Springfield regression (see CanAcceptOrders). Only line produce/consume relief
// is transition-sensitive.
//
// Edge-DB only, like guardNoActiveSwap — no Core round-trip. A read error is
// treated as "no changeover" (fail-open): a transient read blip must not shut
// down the line, and the changeover machinery has its own preflight/cutover gates.
func (e *Engine) guardStyleTransition(node *processes.Node, claim *processes.NodeClaim) error {
	if node == nil || claim == nil {
		return nil
	}
	if claim.SwapMode == protocol.SwapModeManualSwap {
		return nil
	}
	co, err := e.db.GetActiveProcessChangeover(node.ProcessID)
	if err != nil || co == nil {
		return nil
	}
	// Block when the request's claim is the changeover's FROM (outgoing) style.
	// When from-style is unrecorded, still block: pre-cutover the active style IS
	// the outgoing one, so any line relief here is outgoing by construction (once
	// the cutover completes the changeover is no longer active and this passes).
	if co.FromStyleID == nil || claim.StyleID == *co.FromStyleID {
		return fmt.Errorf("node %s: a changeover is armed on this process (switching styles) — material requests for the outgoing style are blocked until cutover completes", node.Name)
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
