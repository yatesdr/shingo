package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// guardNoActiveSwap refuses to dispatch a new swap cycle on a node when
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
		return fmt.Errorf("node %s: a swap is already in progress — wait for the current cycle to complete or abort it before requesting more material", node.Name)
	}
	return nil
}

// guardLineRequest is what every request button of both roles asks of its line
// before anything else is decided: the material request, the produce request
// and the empty-bin request, from the operator or from the level keeper. It runs
// under the cell's lock and before Core is asked anything, so a refusal costs no
// round trip and makes no order. It returns what it read that the plan needs:
// the paired positions of a press a bin is already on its way to.
//
// ONE LINE, ONE LIVE SWAP, IN EVERY SWAP MODE. A swap still working the cell
// refuses the next request, whatever plan that request would have built. A
// sequential line used to be let through, because its backfill is made when the
// removal goes on its way. That backfill is made by the removal's status change
// (handleSequentialBackfill), not by a request, so refusing requests does not
// touch it. What a second request did make there was a second removal: Core
// skipped it on a consume line, and on a produce line it lifted the backfill
// that had just landed, and a RELEASE of it would finalize a fresh empty as a
// filled bin.
//
// ONE BIN COMING TO A LINE AT A TIME, WHOEVER SENT IT. A live order bound for the
// line's core node, not departed and not delivered, refuses the request, from
// any process node: two stations can share one physical position, and a bin one
// of them sent is as much on its way as a bin this one sent. The swap guard
// above reads only this node's slots, and the bare-line gate only this node's
// rows, so a sibling station's bin was invisible to both and the request made a
// second delivery, which Core then held behind the first until the line next
// emptied. A delivered bin is on the line, not on its way: the line reads
// occupied and the request builds the swap that lifts it.
//
// One read covers the line and a press's paired positions: every live order
// bound for any of them, in one snapshot.
//
// LOADERS ARE EXEMPT. A loader window runs a multi-order queue on purpose, so a
// live order there is its normal state and not a reason to refuse the next tap.
//
// FAILS CLOSED. A read error means we do not know what is on its way, and a
// refused request is a tap the operator can repeat.
func (e *Engine) guardLineRequest(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim) (map[string]bool, error) {
	if claim == nil || claim.IsLoaderNode() {
		return nil, nil
	}
	if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
		return nil, err
	}
	var paired []string
	if claim.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		paired = claim.ExtensionPositions()
	}
	rows, err := e.db.ListActiveOrdersByDeliveryNodeSet(append([]string{claim.CoreNodeName}, paired...))
	if err != nil {
		return nil, fmt.Errorf("node %s: cannot tell whether a bin is already on its way (%w) — the next request will re-ask",
			node.Name, err)
	}
	for i := range rows {
		o := &rows[i]
		if o.DeliveryNode != claim.CoreNodeName || o.Departed || o.Status == orders.StatusDelivered {
			continue
		}
		log.Printf("[request] node %s: order %d (%s, %s) is already bringing a bin to %s — refusing the request",
			node.Name, o.ID, o.OrderType, o.Status, claim.CoreNodeName)
		return nil, fmt.Errorf("node %s: order %d is already bringing a bin to %s — wait for it to land",
			node.Name, o.ID, claim.CoreNodeName)
	}
	return pairedPositionsInbound(claim, rows), nil
}

// guardSourceKnownDry refuses to ARM a coordinated swap pair whose supply leg
// would be created against a payload Core reports no stock for.
//
// ── THIS IS THE SPRINGFIELD 2026-07-21 CHURN'S ACTUAL FIX ─────────────────
//
// SYN-PART07A.06, zero system stock, hundreds of doomed swaps in one
// changeover. The loop: the evac leg died, Core's peer-terminal handler
// cancelled the supply with it, the monitor saw that cancel move the in-loop
// UOP, re-armed the changeover, the planner rebuilt the pair, the supply parked
// on the same dry source, the evac died again.
//
// The workaround was a SPARE at the far end of that loop — Core kept the parked
// supply alive so the cancel never happened. It is deleted: it was managing a
// half-dispatched pair, and under the pair rule there is no such thing. What
// replaces it is this, at the near end, and it is the honest place: the churn's
// cause was never the cancellation. It was ARMING A PAIR INTO A SOURCE THAT WAS
// ALREADY KNOWN TO BE DRY. Two orders that cannot possibly source were created,
// hundreds of times, and every downstream mechanism was trying to survive them.
//
// REFUSING TO ARM DEAD-ENDS NOBODY, which is what makes it safe here and not at
// StartProcessChangeover. No order is created, so no order is churned; the level
// keeper re-asks on its next sweep, and the moment the operator stocks the
// payload the pair arms normally. A refusal that creates nothing is a wait, and
// wait-not-fail is the house law. (The changeover preflight is deliberately
// ADVISORY for the opposite reason: it is a deliberate operator action, and
// refusing it dead-ended the floor with idle robots — Springfield NF SPOT 3,
// 2026-06-03. A level-keeper tick has no operator standing at it to dead-end.)
//
// ── IT FAILS OPEN, EVERYWHERE, ON PURPOSE ─────────────────────────────────
//
// Core unreachable, the call erroring, a payload Core cannot count — every one
// of those passes. This guard exists to stop a KNOWN-dry source, and "we could
// not find out" is not knowing. A guard that shut the line down when the Core
// API blipped would be a worse failure than the churn it prevents.
//
// THE EMPTY-CARRIER CASE IS ONE OF THOSE, and it is worth naming because it is
// not an edge case: a PRODUCE swap's supply leg fetches an empty carrier, not a
// payload (BuildSwapDispatch marks the inbound pickup Empty), and the preflight
// endpoint counts bins OF a payload. It cannot answer "are there empty carriers"
// and this guard does not pretend it can. Produce-direction pairs are not
// covered; the consume direction, which is where 07-21 happened, is.
//
// NO MODE NAME. The caller gates on the dispatch shape — a second leg (StepsB)
// — so this reads "is a pair about to be armed", never "is this two_robot". A
// single-robot swap is one order: refused, it
// would not arm, and it is not churned by a dry source, so it is created and
// waits for stock.
func (e *Engine) guardSourceKnownDry(node *processes.Node, claim *processes.NodeClaim) error {
	if node == nil || claim == nil {
		return nil
	}
	payload := claim.PayloadCode
	if payload == "" || payload == "__empty__" {
		return nil // nothing Core can be asked about; see the empty-carrier note
	}
	if e.coreClient == nil || !e.coreClient.Available() {
		return nil
	}
	result, err := e.coreClient.PreflightInventory(e.cfg.StationID(), []string{payload})
	if err != nil || result == nil {
		log.Printf("[request-material] node %s: could not check stock for %s (%v) — arming the pair anyway; "+
			"this guard refuses a KNOWN-dry source, and an unanswered question is not knowing", node.Name, payload, err)
		return nil
	}
	// ABSENT, NOT MISSING. This read Missing, which is "no bin FREE right now":
	// a market whose every bin was reserved or claimed by other pairs read as
	// dry, and the REQUEST was refused for what was only congestion. Stock that
	// is spoken for comes free; the pair should be created and wait for it. Only
	// a payload Core has no bin of at all has nothing to wait for.
	if !result.AbsentKnown {
		log.Printf("[request-material] node %s: core does not say whether it has any %s at all (an older core) — "+
			"arming the pair; not knowing is not knowing it is dry", node.Name, payload)
		return nil
	}
	for _, absent := range result.Absent {
		if absent != payload {
			continue
		}
		log.Printf("[request-material] node %s: NOT arming a swap pair for %s — core has no bin of it anywhere, free "+
			"or spoken for. Two legs that cannot source would be created and cancelled together; the level keeper "+
			"re-asks once stock lands", node.Name, payload)
		return fmt.Errorf("node %s: core has no %s bin anywhere — the swap needs a replacement bin before both robots "+
			"can be committed; load the payload and this will arm itself", node.Name, payload)
	}
	return nil
}

// gateLineRows is the bare-line gate, the same for both roles, and the
// keep-staged spot's count, over one read of the line's own rows. Only a plan
// that brings a bin to a bare line (bare), or a keep-staged claim whose spot
// Core answered for, pays the read. planSpot is the role's spot planner, given
// what stands on the spot less what is leaving it and what is coming to it.
//
// THE GATE REFUSES THE BARE-LINE DELIVERY WHILE THIS POSITION IS STILL
// MID-CYCLE. "Core reports no bin on it" and "this position is bare" are
// different sentences, and planBareLine reads the first as the second. They
// agree whenever a person has pulled a carrier off by hand, which is the case
// the bare-line delivery was written for. They stop agreeing during a swap:
// between the robot lifting the old carrier and setting the new one down, the
// position genuinely holds no bin AND already has one on its way. Core, asked
// in that window, correctly says empty.
//
// SIM 2026-08-31, ALN_004, four cells dead in one run. The swap's own pickup
// made the position read empty at 05:51:45 — Core's intermediate-store
// dropoff then re-bound a bin at 05:51:50, which re-armed the consume tick's
// evaluator. At 05:51:52 the cell asked for material with both runtime
// pointers empty and Core reporting the position free, so Edge downgraded to a
// bare delivery. Two seconds later the in-flight swap set its own bin down, and
// the second robot arrived at a full position it could not place onto. That
// second order then sat in the cell's ActiveOrderID, so the removal that would
// have freed the position could never be raised: machine, robot and carrier
// locked together until a person intervenes. Same family as the Hopkinsville
// swap deadlock.
//
// THE POINTER ALONE IS NOT ENOUGH. Every request has already asked the runtime
// slots (guardNoActiveSwap, in guardLineRequest) and every live order bound for
// the line. This asks THE ORDER ROW of this process node, not the pointer. The
// pointer is slot-scoped, not lifecycle-scoped, so at the moments that matter
// most it says nothing (a changeover-cancel nils both refs mid-swap;
// pre-departure cleanup paths can too), while orders.process_node_id still
// names this cell. It is also the only witness a single_robot swap leaves here:
// that swap is ONE complex order whose delivery_node is the supermarket the old
// carrier ends at, and the new carrier lands at this position as an
// intermediate dropoff — so a delivery-node lookup finds nothing. Asking the
// durable row rather than a pointer scoped to a moment is the same reading
// outboundMoveInFlight takes.
//
// LOADERS ARE EXEMPT, for both roles, as guardLineRequest, guardStyleTransition
// and guardCatidMismatch exempt them. A loader window runs a multi-order queue
// on purpose — CanAcceptOrders returns true for one while its orders are in
// flight — and holding it to a one-bin-at-a-time rule would stall the empties
// it exists to supply (the Springfield regression shape: a loader with a live
// order refusing the operator's next tap).
//
// FAILS CLOSED on a read error, unlike hasActiveSwap. The two wrong answers do
// not cost the same: a wrong "wait" delays supply by one request and the next
// re-asks, while a wrong "go" mints the second delivery this gate exists to
// prevent, and that one never clears itself.
//
// THE RELEASER IS THE BLOCKING ORDER GOING TERMINAL. The gate refuses on ANY
// order at this process node that still works the cell
// (ListActiveOrdersByProcessNode — `status NOT IN (terminal)`, which includes
// `queued`), so the wait is short only while the blocker is moving. An order
// that is stuck — a robot HOLDING at an occupied position, a dead robot pinning
// the runtime slot — refuses supply here for as long as it stays non-terminal,
// and nothing in this gate will time it out. THAT INCLUDES THE OPERATOR'S OWN
// REQUEST: a person pressing a button at the HMI is refused by the same gate,
// with the same releaser. It is the right refusal — a second carrier into a
// position a robot is standing at is the failure this exists to stop — but it
// means the floor's escape from a stuck cell is terminalizing that order
// (abandon / force-complete / cancel), not re-asking.
func (e *Engine) gateLineRows(node *processes.Node, claim *processes.NodeClaim, bare bool, spot spotRead, planSpot func(spotRead, int)) error {
	if claim.IsLoaderNode() {
		bare = false
	}
	spotKnown := claim.KeepStaged && spot.known && planSpot != nil
	if !spotKnown && !bare {
		return nil
	}
	rows, err := e.db.ListActiveOrdersByProcessNode(node.ID)
	if err != nil {
		log.Printf("[request] node %s: could not read its in-flight orders (%v) — refusing the request", node.Name, err)
		return fmt.Errorf("node %s: cannot tell what is on its way to it (%w) — the next request will re-ask", node.Name, err)
	}
	if spotKnown {
		leaving := spotLeaving(rows, claim.InboundStaging, claim.CoreNodeName)
		planSpot(spot.lessLeaving(leaving), spotComing(rows, claim))
	}
	if !bare {
		return nil
	}
	return positionWorkedBy(node, claim, rows)
}

// positionWorkedBy is gateLineRows' refusal over the line's own rows.
func positionWorkedBy(node *processes.Node, claim *processes.NodeClaim, rows []domain.Order) error {
	// THE DURABLE-ROW TWIN OF THE SLOT CHECK, and it must give the same answer.
	// The query is `status NOT IN (terminal)`; the cell question is
	// orderWorksTheCell, which also excludes a leg that has departed. Filtering
	// here rather than in the query keeps the SQL one shape for every caller
	// and keeps the predicate in one place — see leg_departure.go.
	//
	// A KEEP-STAGED REFILL IS EXCUSED (isSpotRefill): a plain order bound for the
	// claim's inbound staging that never touches the line. It is attributed to
	// the line and never departs, so read as working the cell it would refuse this
	// downgrade with "a bin is already on its way" for the whole trip, and with a
	// dry market for ever, while no bin is on its way to the line at all.
	var active []domain.Order
	for _, o := range rows {
		if worksTheCell(&o, claim) {
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
	if claim.IsLoaderNode() {
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
	if claim.IsLoaderNode() {
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
// the bare-line gate's rows (gateLineRows) ask, so the three cannot disagree about
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
