package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
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

// guardPositionSpokenFor refuses the node-empty DOWNGRADE while this position is
// still mid-cycle.
//
// "Core reports no bin on it" and "this position is bare" are different
// sentences, and the downgrade in BuildConsumePlan reads the first as the
// second. They agree whenever a person has pulled a carrier off by hand, which
// is the case the downgrade was written for. They stop agreeing during a swap:
// between the robot lifting the old carrier and setting the new one down, the
// position genuinely holds no bin AND already has one on its way. Core, asked
// in that window, correctly says empty.
//
// SIM 2026-08-31, ALN_004, four cells dead in one run. The swap's own pickup
// nulled the runtime's ActiveOrderID at 05:51:45 — correct, for its own reasons
// (handler_bin_picked_up.go). Core's intermediate-store dropoff re-bound a bin
// at 05:51:50, which re-armed the consume tick's evaluator. At 05:51:52 the
// cell asked for material with both runtime pointers empty and Core reporting
// the position free, so Edge downgraded to a bare delivery. Two seconds later
// the in-flight swap set its own bin down, and the second robot arrived at a
// full position it could not place onto. That second order then sat in the
// cell's ActiveOrderID, so the removal that would have freed the position could
// never be raised: machine, robot and carrier locked together until a person
// intervenes. Same family as the Hopkinsville swap deadlock.
//
// TWO ARMS, BECAUSE THE POINTER ALONE IS NOT ENOUGH:
//
//  1. guardNoActiveSwap — the Bug 3 guard, "refuse to start a second swap on
//     top of an in-flight one", written for exactly this failure and until now
//     unreachable from the one path that causes it. The downgrade returns from
//     BuildConsumePlan before plan.Dispatch exists, and requestNodeFromClaim
//     runs the guard only when it does.
//  2. THE ORDER ROW, NOT THE POINTER. A swap's own pickup nulls ActiveOrderID
//     mid-cycle, so at the moment that matters most the pointer says nothing
//     while orders.process_node_id still names this cell. It is also the only
//     witness a single_robot swap leaves here: that swap is ONE complex order
//     whose delivery_node is the supermarket the old carrier ends at, and the
//     new carrier lands at this position as an intermediate dropoff — so a
//     delivery-node lookup finds nothing. Asking the durable row rather than a
//     pointer scoped to a moment is the same reading outboundMoveInFlight takes.
//
// LOADERS ARE EXEMPT (manual_swap), the same exemption guardStyleTransition and
// guardCatidMismatch carry. A loader window runs a multi-order queue on purpose
// — CanAcceptOrders returns true for one while its orders are in flight — and
// holding it to a one-bin-at-a-time rule would stall the empties it exists to
// supply.
//
// FAILS CLOSED on a read error, unlike hasActiveSwap. The two wrong answers do
// not cost the same: a wrong "wait" delays supply by one tick and the next tick
// re-asks, while a wrong "go" mints the second delivery this guard exists to
// prevent, and that one never clears itself.
//
// THE RELEASER IS THE BLOCKING ORDER GOING TERMINAL, AND "ONE TICK" ASSUMES IT
// DOES. Arm 2 refuses on ANY non-terminal order at this process node
// (ListActiveOrdersByProcessNode — `status NOT IN (terminal)`, which includes
// `queued`), so the cost is one tick only while the blocker is moving. An order
// that is stuck — a robot HOLDING at an occupied position, a dead robot pinning
// the runtime slot — refuses supply here for as long as it stays non-terminal,
// and nothing in this guard will time it out.
//
// THAT INCLUDES THE OPERATOR'S OWN REQUEST. This runs on the downgrade path in
// requestNodeFromClaim regardless of trigger (operator_stations.go:131), so a
// person pressing REQUEST at the HMI is refused by the same arm, with the same
// releaser. It is the right refusal — a second carrier into a position a robot
// is standing at is the failure this exists to stop — but it means the floor's
// escape from a stuck cell is terminalizing that order (abandon / force-complete
// / cancel), not re-asking. Say so if this ever reads as "the button is dead".
//
// SCOPE IS THE DOWNGRADE. A plan that is SimpleMove because the claim's mode is
// "simple", and a plan that carries a Dispatch, both reach their own gates and
// are not this function's business.
func (e *Engine) guardPositionSpokenFor(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim) error {
	if node == nil || claim == nil {
		return nil
	}
	if claim.SwapMode == protocol.SwapModeManualSwap {
		return nil
	}
	if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
		log.Printf("[request-material] node %s reads empty but its runtime still names an in-flight order — "+
			"refusing the simple-delivery downgrade", node.Name)
		return err
	}
	rows, err := e.db.ListActiveOrdersByProcessNode(node.ID)
	if err != nil {
		log.Printf("[request-material] node %s: could not read in-flight orders (%v) — "+
			"refusing the simple-delivery downgrade", node.Name, err)
		return fmt.Errorf("node %s: cannot tell whether a bin is already on its way (%w) — the next tick will re-ask", node.Name, err)
	}
	// THE DURABLE-ROW TWIN OF THE SLOT CHECK, and it must give the same answer.
	// The query is `status NOT IN (terminal)`; the cell question is
	// orderWorksTheCell, which also excludes a leg that has departed. Filtering
	// here rather than in the query keeps the SQL one shape for every caller
	// and keeps the predicate in one place — see leg_departure.go.
	var active []domain.Order
	for _, o := range rows {
		if orderWorksTheCell(&o) {
			active = append(active, o)
		}
	}
	if len(active) == 0 {
		return nil
	}
	// Oldest first (the query orders by created_at), so the sentence names the
	// order the cell has been waiting on rather than whichever one sorted last.
	o := active[0]
	log.Printf("[request-material] node %s reads empty but order %d (%s, %s) is still working this position — "+
		"refusing the simple-delivery downgrade", node.Name, o.ID, o.OrderType, o.Status)
	return fmt.Errorf("node %s: order %d is still working this position — a bin is already on its way; wait for it to land",
		node.Name, o.ID)
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
// ChangeoverArmedError is returned by guardStyleTransition when a material
// request is refused because a changeover is armed on the node's process. It
// carries the process id and both style names so the HTTP handler can offer the
// operator an inline exit — abandon the changeover, and the same request then
// proceeds — instead of a dead-end refusal (2026-07-24). Error() builds the
// operator-facing sentence from those fields, so the value is fully
// constructible (handlers/tests) without a hidden message.
type ChangeoverArmedError struct {
	ProcessID     int64
	ToStyleName   string
	OutgoingStyle string
}

func (e *ChangeoverArmedError) Error() string {
	return fmt.Sprintf("A changeover to %s is armed on this press — abandon it to request %s material.",
		e.ToStyleName, e.OutgoingStyle)
}

// styleName resolves a style id to its display name for operator-facing
// messages, falling back to a stable "style <id>" label when the lookup fails
// or the row has no name — a refusal message must never blank out mid-sentence.
func (e *Engine) styleName(id int64) string {
	if s, err := e.db.GetStyle(id); err == nil && s != nil && s.Name != "" {
		return s.Name
	}
	return fmt.Sprintf("style %d", id)
}

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
		toName := e.styleName(co.ToStyleID)
		outName := e.styleName(claim.StyleID)
		return &ChangeoverArmedError{
			ProcessID:     node.ProcessID,
			ToStyleName:   toName,
			OutgoingStyle: outName,
		}
	}
	return nil
}

// guardCatidMismatch refuses a material request for a line claim when the
// press's live PLC part identity (WarLink CATID_01) diverges from the active
// style's expected_catid (A5, hop 2026-07-23). It is a SIBLING trigger to
// guardStyleTransition for the same outcome — block outgoing-style relief — but
// keyed on ground truth (the physical part on the press) rather than an armed
// changeover. In the 07-23 incident the press was physically KK21 while shingo
// fired LK41 relief; this check catches exactly that divergence at the source
// and refuses the relief before it can strand the line.
//
// Inert by construction unless BOTH sides are known:
//   - The active style must have expected_catid configured. Empty = never
//     block (unconfigured guard). This is the documented "inert on empty" rule.
//   - A live CATID must have been observed and debounced. No monitor, no
//     observation yet, or an unreadable tag ⇒ fail-open (no block), matching
//     guardStyleTransition's read-blip policy: a transient PLC read must not
//     shut the line down.
//
// Loaders (manual_swap) are exempt — they supply empties ACROSS a changeover
// and must stay available (the Springfield regression), same as
// guardStyleTransition. Only line produce/consume relief is part-sensitive.
//
// Edge-DB + in-memory monitor only; no Core round-trip. Never cancels an
// existing order — it only refuses to START new outgoing-style relief.
func (e *Engine) guardCatidMismatch(node *processes.Node, claim *processes.NodeClaim) error {
	if node == nil || claim == nil {
		return nil
	}
	if claim.SwapMode == protocol.SwapModeManualSwap {
		return nil
	}
	if e.catidMon == nil {
		return nil // monitor not started (test fixtures / no PLC) — inert.
	}
	style, err := e.db.GetStyle(claim.StyleID)
	if err != nil || style == nil {
		return nil
	}
	// The style's valid part identities: its produce claims' CATIDs (left/right on
	// a two-position press), or a manual pin. Empty ⇒ inert, never block.
	set := e.styleCATIDSet(style)
	if len(set) == 0 {
		return nil
	}
	live, ok := e.catidMon.liveCATID(node.ProcessID)
	if !ok {
		return nil // no debounced observation yet ⇒ fail-open.
	}
	// The live part must be ONE OF the style's parts — whichever side the press is
	// reporting. It only blocks when the live part belongs to none of them.
	if !catidSetHas(set, live) {
		return fmt.Errorf("Press reports CATID %s; active style is %s (runs CATID %s) — the wrong part is on the press. %s",
			live, style.Name, formatCATIDSet(set), e.catidResolutionHint(node.ProcessID))
	}
	return nil
}

// catidResolutionHint tells the operator where the fix for a CATID mismatch is
// coming from, keyed to the process's auto-arm mode: on `auto` the monitor arms
// a changeover itself once the new part settles and maps to a style; on
// `prompt` the operator gets a changeover prompt on the station; otherwise
// (`off`) they start one. Read-only and fail-soft — an unknown/blank mode reads
// as the default (auto), so the refusal always points somewhere.
func (e *Engine) catidResolutionHint(processID int64) string {
	mode := domain.ChangeoverAutoArmAuto
	if proc, err := e.db.GetProcess(processID); err == nil && proc != nil {
		mode = domain.NormalizeChangeoverAutoArm(proc.ChangeoverAutoArm)
	}
	switch mode {
	case domain.ChangeoverAutoArmPrompt:
		return "use the changeover prompt on this station, or start a changeover to the matching style"
	case domain.ChangeoverAutoArmOff:
		return "start a changeover to the matching style"
	default:
		return "the automatic changeover arm will start it once the part settles, or start a changeover to the matching style"
	}
}

// hasActiveSwap reports whether the runtime slots reference any order that is
// still working this cell. Pure Edge-DB check — no Core round-trip.
//
// It asks orderWorksTheCell, the same predicate CanAcceptOrders and
// guardPositionSpokenFor's row arm ask, so the three cannot disagree about
// whether a cell is busy. A departed leg — a robot driving a bin to the
// supermarket — is live but is not the cell's.
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
		if orderWorksTheCell(o) {
			return true
		}
	}
	return false
}
