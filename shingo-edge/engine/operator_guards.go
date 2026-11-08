package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"maps"

	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
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
// above reads only this node's slots, and the vacant-line gate only this node's
// rows, so a sibling station's bin was invisible to both and the request made a
// second delivery, which Core then held behind the first until the line next
// emptied. A delivered bin is on the line, not on its way: the line reads
// occupied and the request builds the swap that lifts it.
//
// THE DEPENDENT POSITIONS ARE ASKED BY WHAT IS PLACING THERE, NOT BY
// delivery_node. A live leg's delivery_node is a display value Core may leave
// blank, and a press-index leg that refills an on-deck ends elsewhere, so a
// read by delivery_node missed the legs it had to see (E3) and the request
// primed a position a carrier was already coming to. One PlacingAt statement
// answers every dependent position in one snapshot; a position whose live
// leg cannot be read reads as placing, so nothing is primed on a guess.
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
	rows, err := e.db.ListActiveOrdersByDeliveryNodeSet([]string{claim.CoreNodeName})
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
	dependent := claim.DependentPositions()
	if len(dependent) == 0 {
		return nil, nil
	}
	placing, err := e.db.LiveOrderPlacing(dependent...)
	if err != nil {
		return nil, fmt.Errorf("node %s: cannot tell whether a bin is already on its way to %v (%w) — the next request will re-ask",
			node.Name, dependent, err)
	}
	inbound := make(map[string]bool, len(dependent))
	for _, pos := range dependent {
		inbound[pos] = placing[pos] != storeorders.PlacingNone
	}
	return inbound, nil
}

// gateLineRows is the vacant-line gate, the same for both roles, over one read of
// the line's own rows. Only a plan that brings a bin to a vacant line (vacant) pays
// the read.
//
// THE GATE REFUSES THE BARE-LINE DELIVERY WHILE THIS POSITION IS STILL
// MID-CYCLE. "Core reports no bin on it" and "this position is vacant" are
// different sentences, and planVacantLine reads the first as the second. They
// agree whenever a person has pulled a carrier off by hand, which is the case
// the vacant-line delivery was written for. They stop agreeing during a swap:
// between the robot lifting the old carrier and setting the new one down, the
// position genuinely holds no bin AND already has one on its way. Core, asked
// in that window, correctly says empty.
//
// SIM 2026-08-31, ALN_004, four cells dead in one run. The swap's own pickup
// made the position read empty at 05:51:45 — Core's intermediate-store
// dropoff then re-bound a bin at 05:51:50, which re-armed the consume tick's
// evaluator. At 05:51:52 the cell asked for material with both runtime
// pointers empty and Core reporting the position free, so Edge downgraded to a
// vacant-line delivery. Two seconds later the in-flight swap set its own bin down, and
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
func (e *Engine) gateLineRows(node *processes.Node, claim *processes.NodeClaim, vacant bool) error {
	if !vacant || claim.IsLoaderNode() {
		return nil
	}
	rows, err := e.db.ListActiveOrdersByProcessNode(node.ID)
	if err != nil {
		log.Printf("[request] node %s: could not read its in-flight orders (%v) — refusing the request", node.Name, err)
		return fmt.Errorf("node %s: cannot tell what is on its way to it (%w) — the next request will re-ask", node.Name, err)
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
// It refuses by position: a position the changeover changes nothing at — the
// outgoing and incoming styles' claims there name the same parts, and the
// changeover's task there, if any, is "unchanged" — is not refused
// (changesNothingAt). Its part is the same under both styles, so relief there
// is the next style's supply as much as this one's; refused, a common
// position that runs out while a changeover stands open for hours got no cart
// from Shingo at all, and the cell ran on carts nobody told it about.
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
	return fmt.Sprintf("A changeover to %s is armed on this process — cancel it to request %s material.",
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
	return e.guardStyleTransitionIn(nil, node, claim)
}

// guardStyleTransitionIn is guardStyleTransition on a door call's reads
// (guardReads; nil reads afresh).
func (e *Engine) guardStyleTransitionIn(g *guardReads, node *processes.Node, claim *processes.NodeClaim) error {
	if node == nil || claim == nil {
		return nil
	}
	if claim.IsLoaderNode() {
		return nil
	}
	g = readsFor(g, node)
	co := g.changeover(e)
	if co == nil {
		return nil
	}
	// Block when the request's claim is the changeover's FROM (outgoing) style.
	// When from-style is unrecorded, still block: pre-cutover the active style IS
	// the outgoing one, so any line relief here is outgoing by construction (once
	// the cutover completes the changeover is no longer active and this passes).
	if co.FromStyleID == nil || claim.StyleID == *co.FromStyleID {
		if co.FromStyleID != nil && e.changesNothingAt(g, co, node) {
			return nil
		}
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
// press's live PLC part identity (WarLink CATID_01) is none of the active
// style's part-identity set — its pin, else its parts' catalog CATIDs (A5,
// 2026-07-23). It is a SIBLING trigger to
// guardStyleTransition for the same outcome — block outgoing-style relief — but
// keyed on ground truth (the physical part on the press) rather than an armed
// changeover. In the 07-23 incident the press was physically KK21 while shingo
// fired LK41 relief; this check catches exactly that divergence at the source
// and refuses the relief before it can strand the line.
//
// Inert by construction unless BOTH sides are known:
//   - The active style's set must be non-empty (a pin, or catalog CATIDs on its
//     produce parts). Empty = never block. This is the documented "inert on
//     empty" rule.
//   - A live CATID must have been observed and debounced. No monitor, no
//     observation yet, or an unreadable tag ⇒ fail-open (no block), matching
//     guardStyleTransition's read-blip policy: a transient PLC read must not
//     shut the line down.
//
// Loaders (manual_swap) are exempt — they supply empties ACROSS a changeover
// and must stay available (the Springfield regression), same as
// guardStyleTransition. Only line produce/consume relief is part-sensitive.
//
// It refuses by position, as guardStyleTransition does: a position whose claim
// names the same parts under the style the PLC's part belongs to as under the
// request's (samePartUnder, every style whose set holds the live CATID) is
// not refused — after "Mark as running" onto a part the PLC is not making
// yet, a common position is supplied and a position whose part differs waits
// for the PLC. A live part no style runs refuses everywhere, as before.
//
// Edge-DB + in-memory monitor only; no Core round-trip. Never cancels an
// existing order — it only refuses to START new outgoing-style relief.
func (e *Engine) guardCatidMismatch(node *processes.Node, claim *processes.NodeClaim) error {
	return e.guardCatidMismatchIn(nil, node, claim)
}

// guardCatidMismatchIn is guardCatidMismatch on a door call's reads
// (guardReads; nil reads afresh).
func (e *Engine) guardCatidMismatchIn(g *guardReads, node *processes.Node, claim *processes.NodeClaim) error {
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
	if !catidSetHas(set, live) && !e.sameUnderTheLivePart(readsFor(g, node), node, claim.StyleID, live) {
		return fmt.Errorf("The process reports CATID %s; active style is %s (runs CATID %s) — the wrong part is on the process. %s",
			live, style.Name, formatCATIDSet(set), e.catidResolutionHint(node.ProcessID))
	}
	return nil
}

// changesNothingAt reports whether the open changeover co changes nothing at
// node, on a door call's reads (guardReads): the outgoing and incoming styles'
// claims there name the same parts, and the changeover's task there is
// "unchanged" (a same-part position the incoming claim evacuates keeps the
// guard). Every diff mints a task row, the unchanged ones included
// (planChangeover), so a position with no task is one whose claims changed
// after the changeover was planned: absent reads as a change (decision 48). A
// read that fails reads as a change, and both are logged: they turn a request
// away.
func (e *Engine) changesNothingAt(g *guardReads, co *processes.Changeover, node *processes.Node) bool {
	if !e.samePartUnder(g, node, *co.FromStyleID, co.ToStyleID) {
		return false
	}
	task, err := g.taskAt(e, co)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && task == nil):
		log.Printf("changeover guard: changeover %d has no task at %s — its claims changed after it was planned; read as a change",
			co.ID, node.Name)
		return false
	case err != nil:
		log.Printf("changeover guard: read changeover %d's task at %s: %v — read as a change", co.ID, node.Name, err)
		return false
	}
	return task.Situation == string(SituationUnchanged)
}

// sameUnderTheLivePart reports whether node's claim names the same parts
// under every style whose set holds the live CATID as under styleID; false
// when no style's does.
func (e *Engine) sameUnderTheLivePart(g *guardReads, node *processes.Node, styleID int64, live string) bool {
	matches := e.stylesForCATID(node.ProcessID, live)
	if len(matches) == 0 {
		return false
	}
	for _, m := range matches {
		if !e.samePartUnder(g, node, styleID, m.ID) {
			return false
		}
	}
	return true
}

// samePartUnder reports whether node's claims under styles a and b name the
// same parts, on a door call's reads (the process read once, each pair of
// styles' claims once): each style claims the node exactly (claims.Resolve,
// ExactStyle) and their AllowedPayloads() hold the same codes. A read that
// fails, or a claim that names no part, is not the same part; a failed read is
// logged.
func (e *Engine) samePartUnder(g *guardReads, node *processes.Node, a, b int64) bool {
	key := [2]int64{min(a, b), max(a, b)}
	if v, ok := g.pairs[key]; ok {
		return v
	}
	v := e.readSamePart(g, node, a, b)
	if g.pairs == nil {
		g.pairs = map[[2]int64]bool{}
	}
	g.pairs[key] = v
	return v
}

func (e *Engine) readSamePart(g *guardReads, node *processes.Node, a, b int64) bool {
	proc, err := g.process(e)
	if err != nil || proc == nil {
		log.Printf("changeover guard: read process %d for %s: %v — not the same part", node.ProcessID, node.Name, err)
		return false
	}
	claims, err := e.db.NodeClaimsForStyles([]int64{a, b})
	if err != nil {
		log.Printf("changeover guard: read styles %d and %d's claims at %s: %v — not the same part", a, b, node.Name, err)
		return false
	}
	x := claims.Resolve(proc, node, store.ExactStyle(a))
	y := claims.Resolve(proc, node, store.ExactStyle(b))
	if x == nil || y == nil {
		return false
	}
	return sameCodes(x.AllowedPayloads(), y.AllowedPayloads())
}

// guardReads is one door call's reads for the changeover guards (decision
// 48): the process's open changeover row, the process, each pair of styles'
// claims at the node (samePartUnder) and the changeover's task there — each read
// once however many guards ask: the sweep's participant gate (CanAcceptOrders),
// A2, A5 and A2's re-ask under the cell's lock. A door makes one
// (newGuardReads) and passes it down; nil reads afresh at each guard, as
// before. The re-ask under the lock re-reads only the changeover row
// (recheck): the same row keeps the rest, another starts over. Not shared
// across door calls or goroutines.
type guardReads struct {
	node *processes.Node

	coRead bool
	co     *processes.Changeover

	procRead bool
	proc     *processes.Process
	procErr  error

	pairs map[[2]int64]bool

	taskFor int64 // the changeover id the task was read under, 0 for none
	task    *processes.NodeTask
	taskErr error
}

func newGuardReads(node *processes.Node) *guardReads { return &guardReads{node: node} }

// readsFor is g when it reads for node, else a fresh one.
func readsFor(g *guardReads, node *processes.Node) *guardReads {
	if g == nil || g.node == nil || node == nil || g.node.ID != node.ID {
		return newGuardReads(node)
	}
	return g
}

// changeover is the process's open changeover, nil for none. A read that
// fails reads as none (the guards' fail-open on the outer lookup).
func (g *guardReads) changeover(e *Engine) *processes.Changeover {
	if !g.coRead {
		co, err := e.db.GetActiveProcessChangeover(g.node.ProcessID)
		if err != nil {
			co = nil
		}
		g.co, g.coRead = co, true
	}
	return g.co
}

// recheck re-reads the changeover row, the re-ask under the cell's lock: a
// changeover may have armed while the request waited for the cell. Another
// row than the one read drops what was read against it.
func (g *guardReads) recheck(e *Engine) {
	was := g.co
	g.coRead = false
	if now := g.changeover(e); was == nil || now == nil || was.ID != now.ID {
		g.taskFor, g.task, g.taskErr = 0, nil, nil
	}
}

func (g *guardReads) process(e *Engine) (*processes.Process, error) {
	if !g.procRead {
		g.proc, g.procErr = e.db.GetProcess(g.node.ProcessID)
		g.procRead = true
	}
	return g.proc, g.procErr
}

func (g *guardReads) taskAt(e *Engine, co *processes.Changeover) (*processes.NodeTask, error) {
	if g.taskFor != co.ID {
		g.task, g.taskErr = e.db.GetChangeoverNodeTaskByNode(co.ID, g.node.ID)
		g.taskFor = co.ID
	}
	return g.task, g.taskErr
}

// sameCodes reports whether two part lists hold the same codes, neither empty.
func sameCodes(x, y []string) bool {
	if len(x) == 0 || len(y) == 0 {
		return false
	}
	xs, ys := map[string]bool{}, map[string]bool{}
	for _, c := range x {
		xs[c] = true
	}
	for _, c := range y {
		ys[c] = true
	}
	return maps.Equal(xs, ys)
}

// catidResolutionHint tells the operator where the fix for a CATID mismatch is
// coming from, keyed to the process's auto-arm mode: on `auto` the monitor
// presses CUTOVER on a changeover already started once the new part settles,
// and starts none; on `prompt` the operator gets a changeover prompt on the
// station; otherwise (`off`) they start one. Pinned by
// TestCatidResolutionHint_SaysWhatEachModeDoes. Read-only and fail-soft — an unknown/blank mode reads
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
		return "start a changeover to the matching style; the automatic arm presses CUTOVER on a changeover already started once the part settles"
	}
}

// hasActiveSwap reports whether the runtime slots reference any order that is
// still working this cell. Pure Edge-DB check — no Core round-trip.
//
// It asks orderWorksTheCell, the same predicate CanAcceptOrders and
// the vacant-line gate's rows (gateLineRows) ask, so the three cannot disagree about
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
