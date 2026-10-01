//go:build sim

package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/config"
	"shingoedge/release"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// sim_operator.go — the sim-mode auto operator (brief T3.2, D4). It subscribes
// to the engine EventBus and performs, after a configurable delay, the manual-
// swap LOAD / CLEAR a human operator would: an empty bin delivered to a
// manual_swap+produce node gets LOADed; a full bin delivered to a
// manual_swap+consume node gets CLEARed.
//
// It lives in the engine package (sim-tagged) rather than a subpackage so it can
// use the unexported node classifier (loadActiveNode) and the LoadBin/ClearBin
// methods directly — exporting those purely for sim would widen the engine API
// for no production benefit. Being //go:build sim, it is absent from every
// non-sim build (so it can't affect the production engine or its test suites).
//
// IT DOES NOT CUT A CHANGEOVER OVER, and nothing in the sim does. A changeover
// reaches `{"can_complete":true,"blockers":[]}` and then waits for somebody to
// press the button: POST /api/processes/{id}/changeover/cutover. Drive it from
// the harness.
//
// This note used to say auto-cutover was "deferred within T3.2", beside TWO
// config keys that agreed with it: operators.changeover_auto_cutover, which
// defaulted true and was set true in both dev yamls, and operators.cutover_delay,
// which EDGE 2 set to 4m under a comment giving the cutover a time. So a reader
// had a setting, a default, a duration, a timeline and a deferral note all
// describing a cutover that was coming, and no code anywhere that would cause
// one. Two agents in two days polled gate-status waiting for it. Both keys and
// both fields deleted 2026-09-10; this paragraph is what replaces them.
//
// The EventCounterDelta→0 unloader trigger is still genuinely deferred.
//
// NOT TO BE CONFUSED WITH THE CATID MONITOR (plc_catid_monitor.go), which is
// production, is wired, and does press CUTOVER — on a real PLC reporting the
// new part on an `auto` process. That is a different mechanism on a different
// signal; it is not scaffolding and it was never what this key meant.
//
// The delivery-driven LOAD/CLEAR below is the core of the four loops.

type simOperator struct {
	e   *Engine
	ops config.SimOperatorsConfig
	clk clock.Clock // sim clock; its After scales delays by live speed
	ctx context.Context

	// classify maps a delivered-to node to its operator action; a function
	// field so tests can inject a stub (the default reads the node's claim).
	classify func(nodeID int64) (delay time.Duration, label string, action func() error, ok bool)

	mu         sync.Mutex
	pending    map[int64]bool // nodes with a LOAD/CLEAR scheduled/in-flight (idempotence)
	releasing  map[int64]bool // orders with a swap-ready release scheduled/in-flight
	confirming map[int64]bool // delivered swap legs with a confirm scheduled/in-flight

	// marketSlots caches the combined market's storage-slot node names for the
	// negative-bin sweep (clearNegativeBins). Populated lazily; read only by the
	// single reconcile goroutine, so no lock is needed.
	marketSlots []string
}

// StartSimOperator wires the sim operator to the EventBus. Sim builds only;
// called from the edge composition root's startSimSubsystems when
// sim.operators.enabled. The driver/fake run on their own clocks today; a
// shared clock for a manual-clock integration harness is deferred (J16).
func (e *Engine) StartSimOperator(ctx context.Context, simCfg config.SimConfig, clk clock.Clock) {
	op := &simOperator{
		e:          e,
		ops:        simCfg.Operators,
		clk:        clk,
		ctx:        ctx,
		pending:    make(map[int64]bool),
		releasing:  make(map[int64]bool),
		confirming: make(map[int64]bool),
	}
	op.classify = op.classifyFromClaim
	// The bus is synchronous (D4): handlers must not block — they dedupe and
	// spawn a delayed worker, then return. onDelivered drives the post-delivery
	// LOAD/CLEAR; onStatusChanged drives the swap-ready release. There is no
	// cutover handler: the sim has no PLC bit to stand in for, because the
	// release trunk flips the pair itself (2026-09-27, fact-owners Lane G).
	e.Events.SubscribeTypes(op.onDelivered, EventOrderDelivered)
	e.Events.SubscribeTypes(op.onStatusChanged, EventOrderStatusChanged)
	e.logFn("[sim] sim operator started (loader_auto_load=%s unloader_auto_clear=%s swap_release=%s)",
		op.loaderDelay(), op.unloaderDelay(), op.swapReleaseDelay())

	// Reconciliation sweep (restart-safety). The SubscribeTypes handlers above
	// only fire on LIVE transitions, so any order already mid-choreography when
	// this operator starts — e.g. after an edge restart — is invisible to them
	// and orphans: its swap never releases/confirms, the consumer never
	// resupplies, and the loop wedges. runReconcileLoop re-derives pending
	// operator actions from current DB state on startup and on a periodic tick,
	// routing them through the same idempotent schedule* helpers, so a restart
	// mid-loop resumes cleanly instead of deadlocking.
	go op.runReconcileLoop()
}

// dwell waits out a simulated operator's reaction time and reports whether the
// worker that called it may still act.
//
// THE RE-CHECK AFTER THE WAKE IS THE POINT, and it is what four hand-written
// copies of this select did not have. `select` picks uniformly at random among
// ready cases, so a worker whose timer and whose cancellation come due together
// takes the timer half the time and walks on into an operator that is stopping.
// Asking op.ctx a second time, after the wait rather than during it, makes
// cancellation win whenever it has happened at all.
func (op *simOperator) dwell(d time.Duration) bool {
	select {
	case <-op.ctx.Done():
		return false
	case <-op.clk.After(d):
	}
	return op.ctx.Err() == nil
}

// hasStore reports whether this operator still has an Edge store to read.
//
// ── WHY A WORKER CAN WAKE WITHOUT ONE ──
//
// A scheduled worker holds a POINTER TO THE ENGINE across a wait the sim clock
// can stretch arbitrarily, so what it holds and what still exists are two
// different questions. newTestSimOperator builds an Engine with no db, which is
// deliberate, and a test that advances its manual clock also fires every
// release timer scheduleRelease left pending; those workers wake into the
// hollow Engine:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	shingoedge/store.(*DB).GetOrder(...)
//	shingoedge/engine.(*simOperator).runRelease(...)
//
// It is `go test -count=20 -tags sim ./engine/` on main, intermittent because
// the worker has to be scheduled before the binary exits, and it is why the sim
// step of scripts/gate.sh cannot be relied on to be green.
//
// A worker that wakes must re-establish that it still has an engine before it
// touches one.
func (op *simOperator) hasStore() bool { return op.e != nil && op.e.db != nil }

func (op *simOperator) loaderDelay() time.Duration {
	d := 5 * time.Second
	if op.ops.LoaderAutoLoad > 0 {
		d = op.ops.LoaderAutoLoad
	}
	// Base (simulated) delay; the sim clock's After applies the live speed
	// multiplier, so scaling here too would double-count.
	return d
}

func (op *simOperator) unloaderDelay() time.Duration {
	d := 8 * time.Second
	if op.ops.UnloaderAutoClear > 0 {
		d = op.ops.UnloaderAutoClear
	}
	return d // base delay; the sim clock's After applies live speed
}

func (op *simOperator) onDelivered(ev Event) {
	d, ok := ev.Payload.(OrderDeliveredEvent)
	if !ok || d.ProcessNodeID == nil {
		return
	}
	if !op.deliveryLandedHere(d) {
		// The bin this order was carrying came to rest somewhere else. Scheduling
		// a LOAD/CLEAR here would act on a node the delivery did not fill.
		op.e.debugFn("[sim] operator: order %d is tracked at node %d but its bin landed elsewhere — no LOAD/CLEAR",
			d.OrderID, *d.ProcessNodeID)
		return
	}
	op.schedule(*d.ProcessNodeID)                   // LOAD/CLEAR for manual_swap nodes
	op.scheduleConfirm(d.OrderID, *d.ProcessNodeID) // sign off swap legs delivered to a line node
}

// deliveryLandedHere reports whether the delivered order actually left a bin at
// the process node it is tracked against.
//
// ── AN UNLOADER'S OWN EMPTY-OUT WAS SCHEDULING ITS CLEAR ──────────────────
//
// A manual_swap unloader's U2 leg carries the drained carrier AWAY — source
// FGN_001, destination SYN_PRESS_EMPTIES — and it is tracked at FGN_001's
// process node. So its delivery scheduled a CLEAR at a node the same order had
// just emptied. The worker waited its 8s, found nothing, burned all eight
// retries in about three sim-seconds, and gave up nine sim-seconds before the
// NEXT carrier arrived:
//
//	08:21:35  bin_picked_up bin=21 at FGN_001            <- U2 lifts the carrier
//	08:21:38  auto-clear node 21 attempt 1..8: no bin at node FGN_001
//	08:21:47  delivered fallback: bound bin 20 to node FGN_001   <- the real arrival
//
// AND THE DEDUPE MADE IT WORSE, because `schedule` drops a second call while one
// is pending: a spurious run from the outbound leg could swallow the genuine one
// from the inbound arrival. Sim 2026-08-30 measured 115 give-ups in a single run
// — every one of them a clear that never happened, on a fixture whose empty pool
// only refills when carriers ARE cleared.
//
// The test is the one outboundMoveInFlight already uses on the order side: a
// move whose SOURCE is this slot is this slot's bin LEAVING. Anything else —
// a delivery to here, a complex leg with no delivery_node, an unreadable row —
// schedules as before, which keeps this a narrowing of a known-noisy trigger
// rather than a new gate with its own failure mode.
func (op *simOperator) deliveryLandedHere(d OrderDeliveredEvent) bool {
	if op.e == nil || op.e.db == nil {
		return true // no store wired (unit fixtures) — behave exactly as before
	}
	node, err := op.e.db.GetProcessNode(*d.ProcessNodeID)
	if err != nil || node == nil || node.CoreNodeName == "" {
		return true // cannot tell — behave exactly as before
	}
	order, err := op.e.db.GetOrder(d.OrderID)
	if err != nil || order == nil {
		return true
	}
	return !(order.SourceNode == node.CoreNodeName && order.DeliveryNode != node.CoreNodeName)
}

// confirmDelay is the operator's reaction time before signing off a delivered
// swap leg — the headless equivalent of confirming receipt at the line. The sim
// clock's After scales it by live speed.
const confirmDelay = 2 * time.Second

// scheduleConfirm dedupes by order and spawns the confirm worker. Safe on the
// synchronous bus — it never blocks.
func (op *simOperator) scheduleConfirm(orderID, nodeID int64) {
	op.mu.Lock()
	if op.confirming[orderID] {
		op.mu.Unlock()
		return
	}
	op.confirming[orderID] = true
	op.mu.Unlock()
	go op.runConfirm(orderID, nodeID)
}

// runConfirm signs off a swap leg that delivered a bin TO a produce/consume line
// node. Why this exists: a produce/consume resupply (or A/B backfill) leg lands
// `delivered` and stays non-terminal until something confirms it. The sim has no
// human operator to confirm, and the only other confirm path — Core's
// reconciliation auto-confirm sweep — confirms the CORE order but cannot
// transition the EDGE order, so the Edge leg sits `delivered` forever and
// CanAcceptOrders reports "active/staged order in progress", blocking the next
// relief until the cell/press overfills (PLN_003 → hundreds of uop over cap).
// Issuing the Edge receipt here (ConfirmDelivery) is the design's "Edge receipt"
// confirm path (sim.md §5): it transitions the Edge order AND notifies Core, so
// both sides reach `confirmed` and the swap loop self-clears.
//
// Scope guards keep it to exactly the legs that need it:
//   - manual_swap loader/unloader nodes are LOAD/CLEAR-driven (skip_auto_confirm);
//     never auto-confirmed here.
//   - an AUTO-CONFIRM leg signs itself off on FINISHED. It needs no receipt, and
//     issuing one races its own transition.
//   - the leg must actually do something at this node.
//   - re-checks status==delivered after the dwell so a racing confirm is a no-op.
//
// The node test reads STEPS for a complex order, not delivery_node. That column
// cannot answer it: an auto-confirmed leg's is blanked outright, and a
// press-index R1's names the index node it stages at rather than the press it
// serves. The old test (DeliveryNode == CoreNodeName) therefore skipped exactly
// the legs that most need a receipt — press-index R1 and single-robot A, neither
// of which auto-confirms — and they would sit `delivered` forever, with
// CanAcceptOrders reporting "active/staged order in progress" until the cell
// overfilled (the PLN_003 shape this function exists to prevent).
//
// "Touches this node" is the right question, and it is weaker than "leaves a bin
// here" on purpose: press-index R1 serves the press by CLEARING it, so it leaves
// no bin behind and still needs signing off. Simple orders keep using
// delivery_node, which is unambiguous for them — the same split the delivered
// gate makes in wiring_delivered.go.
func (op *simOperator) runConfirm(orderID, nodeID int64) {
	defer func() {
		op.mu.Lock()
		delete(op.confirming, orderID)
		op.mu.Unlock()
	}()

	node, _, claim, err := loadActiveNode(op.e.db, nodeID)
	if err != nil || node == nil || claim == nil {
		return
	}
	if claim.IsLoaderNode() {
		return // loader/unloader — LOAD/CLEAR owns its lifecycle
	}
	order, err := op.e.db.GetOrder(orderID)
	if err != nil || order == nil {
		return
	}
	if order.AutoConfirm {
		return // signs itself off on FINISHED; a receipt here would race it
	}
	if !op.legServesNode(order, node.CoreNodeName) {
		return
	}

	if !op.dwell(confirmDelay) {
		return
	}

	// Re-read after the dwell — Core's sweep or a sibling may have advanced it.
	order, err = op.e.db.GetOrder(orderID)
	if err != nil || order == nil || order.Status != protocol.StatusDelivered {
		return
	}
	if err := op.e.orderMgr.ConfirmDelivery(orderID, order.Quantity); err != nil {
		op.e.debugFn("[sim] operator auto-confirm order %d rejected: %v", orderID, err)
		return
	}
	op.e.logFn("[sim] operator auto-confirm delivered leg order %d at %s", orderID, node.CoreNodeName)
}

// legServesNode reports whether this order is one the operator at coreNodeName
// would sign for. A complex leg is judged by its STEPS (see runConfirm); a simple
// order by its delivery node, which says exactly where its one bin goes.
func (op *simOperator) legServesNode(order *storeorders.Order, coreNodeName string) bool {
	if order.OrderType != protocol.OrderTypeComplex {
		return order.DeliveryNode == coreNodeName
	}
	stepsJSON, err := op.e.db.GetOrderStepsJSON(order.ID)
	if err != nil {
		op.e.debugFn("[sim] operator confirm: order %d — cannot load steps: %v", order.ID, err)
		return false
	}
	steps, err := decodeSteps(stepsJSON)
	if err != nil {
		op.e.debugFn("[sim] operator confirm: order %d — %v", order.ID, err)
		return false
	}
	return legTouchesNode(steps, coreNodeName)
}

// legTouchesNode reports whether the leg does anything at node at all — waits,
// picks up, or drops off. Weaker than legPlacesBinAt, and deliberately so: it
// answers "is this node part of this leg's job?", not "does the bin end up here".
// Press-index R1 serves the press by CLEARING it, so it leaves no bin behind and
// still needs signing off.
func legTouchesNode(steps []protocol.ComplexOrderStep, node string) bool {
	if node == "" {
		return false
	}
	for _, s := range steps {
		if s.Node == node {
			return true
		}
	}
	return false
}

// schedule dedupes by node and spawns the LOAD/CLEAR worker. It is
// safe on the synchronous EventBus. A second delivery to a node already in the
// delay window is dropped — engine validation is the backstop if it slips
// through.
func (op *simOperator) schedule(nodeID int64) {
	op.mu.Lock()
	if op.pending[nodeID] {
		op.mu.Unlock()
		return
	}
	op.pending[nodeID] = true
	op.mu.Unlock()
	go op.run(nodeID)
}

func (op *simOperator) run(nodeID int64) {
	defer func() {
		op.mu.Lock()
		delete(op.pending, nodeID)
		op.mu.Unlock()
	}()

	delay, label, action, ok := op.classify(nodeID)
	if !ok {
		return
	}
	if !op.dwell(delay) {
		return
	}

	// A manual_swap LOAD/CLEAR can land in a transient gap: the empty hasn't been
	// placed at the slot yet, or the previous bin is still awaiting its outbound
	// move. A single attempt that hits that gap orphans the order at `delivered`
	// (the manual_swap node has no human to come back and act when the slot is
	// ready). So retry a bounded number of times instead of firing once. action()
	// is idempotent — it re-reads the node's bins each call — so a retry that still
	// finds the slot not-ready is a harmless no-op until it is.
	const (
		maxAttempts = 8
		retryDelay  = 4 * time.Second
	)
	for attempt := 1; ; attempt++ {
		err := action()
		if err == nil {
			op.e.logFn("[sim] operator auto-%s node %d (attempt %d)", label, nodeID, attempt)
			return
		}
		if attempt >= maxAttempts {
			// Gave up: a precondition stayed unmet (order cancelled, slot never freed).
			op.e.debugFn("[sim] operator auto-%s node %d gave up after %d attempts: %v", label, nodeID, attempt, err)
			return
		}
		op.e.debugFn("[sim] operator auto-%s node %d attempt %d not ready, retrying: %v", label, nodeID, attempt, err)
		if !op.dwell(retryDelay) {
			return
		}
	}
}

// defaultSwapReleaseDelay is the simulated operator reaction time between a
// swap reaching its swap-ready wait (status "staged") and the operator pushing
// Release. The sim clock's After scales it by live speed.
const defaultSwapReleaseDelay = 3 * time.Second

// swapReleaseDelay is the configured reaction time, or the default.
//
// A KNOB BECAUSE THE DEFAULT CLOSES THE WINDOW UNDER TEST. Three seconds is a
// good imitation of a person and a poor instrument: a scenario built to observe
// a HELD release — one leg staged, its sibling still coming — has three seconds
// to look before the operator releases anyway. A run against the round-4
// collision gate lost that window 480 times to this timer and reported the gate
// as never firing.
func (op *simOperator) swapReleaseDelay() time.Duration {
	if op.ops.SwapRelease > 0 {
		return op.ops.SwapRelease
	}
	return defaultSwapReleaseDelay
}

// onStatusChanged is the swap-ready auto-release trigger. Produce and consume
// single/two-robot swaps share one choreography (BuildSwapDispatch): both dwell
// at a "wait" leg until the operator confirms the swap, at which point the order
// is "staged" and the HMI lights a Release button. The sim has no human, so when
// an order reaches "staged" we fire that same release after a short delay — the
// headless equivalent of the click. Simple moves and ingest-only modes never
// stage, so they're untouched. Must not block (synchronous bus): it dedupes and
// spawns a delayed worker, then returns.
func (op *simOperator) onStatusChanged(ev Event) {
	d, ok := ev.Payload.(OrderStatusChangedEvent)
	if !ok {
		return
	}
	if d.NewStatus == "staged" {
		op.scheduleRelease(d.OrderID)
	}
}

// scheduleRelease dedupes by order and spawns the swap-ready release worker.
// Called from both the live staged-transition event and the reconciliation
// sweep, so it must be idempotent — the releasing map guarantees at most one
// runRelease per order.
func (op *simOperator) scheduleRelease(orderID int64) {
	op.mu.Lock()
	if op.releasing[orderID] {
		op.mu.Unlock()
		return
	}
	op.releasing[orderID] = true
	op.mu.Unlock()
	go op.runRelease(orderID)
}

// reconcileInterval is how often the restart-safety sweep re-derives pending
// operator actions from current state. The sim clock's ticker scales it by live
// speed, matching the other operator delays.
const reconcileInterval = 10 * time.Second

// runReconcileLoop drives reconcile() once immediately (the restart-safety net)
// then on every reconcileInterval tick until ctx is done.
func (op *simOperator) runReconcileLoop() {
	op.reconcile()
	t := op.clk.NewTicker(reconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-op.ctx.Done():
			return
		case <-t.C():
			op.reconcile()
		}
	}
}

// reconcile scans current non-terminal orders and drives any pending operator
// action through the same idempotent schedule* helpers the live-event handlers
// use. This is what makes the operator restart-safe: on startup (and periodically
// thereafter) it acts on orders that were already staged/delivered before this
// process existed — which the event subscriptions, being live-only, never see.
// The dedupe maps make redundant calls (event + sweep) harmless no-ops.
func (op *simOperator) reconcile() {
	active, err := op.e.db.ListActiveOrders()
	if err != nil {
		op.e.debugFn("[sim] reconcile: list active orders: %v", err)
		return
	}
	pending := 0
	for i := range active {
		o := active[i]
		switch o.Status {
		case protocol.StatusStaged:
			// ── NO CUTOVER ARM, AND ITS ABSENCE IS THE CHANGE ─────────────
			//
			// This arm used to call scheduleFlip. It existed because the
			// cutover was a separate operator action the sim had to take before
			// a release could succeed — the 2026-08-30 deadlock this loop was
			// written to close ("444 refusals, five robots pinned, four
			// sim-hours"). Since 2026-09-27 (fact-owners Lane G) the release
			// trunk flips the pair itself when the partner can feed the line,
			// so scheduleRelease is the whole action: nothing left to re-derive.
			op.scheduleRelease(o.ID)
			pending++
		case protocol.StatusDelivered:
			// The test onDelivered makes: a leg whose bin LEFT this node (a U2
			// empty-out) is not a delivery here, so neither a LOAD/CLEAR nor a
			// confirm belongs to it.
			// PIN: TestSimOperator_ReconcileOutboundDeliveryDoesNotScheduleAClear
			if o.ProcessNodeID != nil &&
				op.deliveryLandedHere(OrderDeliveredEvent{OrderID: o.ID, ProcessNodeID: o.ProcessNodeID}) {
				op.schedule(*o.ProcessNodeID)              // LOAD/CLEAR for manual_swap nodes
				op.scheduleConfirm(o.ID, *o.ProcessNodeID) // confirm delivered-at-line legs
				pending++
			}
		}
	}
	if pending > 0 {
		op.e.debugFn("[sim] reconcile: drove %d pending order(s)", pending)
	}
	// Negative-bin sweep: partials are fine in the combined market, but negative-UOP
	// bins must not circulate — reset them to clean empties. See clearNegativeBins.
	op.clearNegativeBins()
	// And the NODE sweep, which is the other half of restart-safety. See below.
	op.sweepManualSwapNodes()
}

// sweepManualSwapNodes drives any manual_swap node that is holding a bin it
// should have acted on, whether or not an order says so.
//
// ── WHY THE ORDER SWEEP ABOVE IS NOT ENOUGH ───────────────────────────────
//
// reconcile's loop is over ORDERS, so every path into it needs a live order in
// `staged` or `delivered` pointing at the node. That is one assumption, and the
// lane-stress rig broke it in a way nothing recovered from.
//
// The unloader FGN_001 took delivery of a full ASSY bin through Core's
// "delivered fallback: bound bin N to node FGN_001 via delivery-node resolution"
// path, which binds the bin WITHOUT emitting EventOrderDelivered. So onDelivered
// never scheduled the clear, and by the time reconcile next ran the order was
// terminal — invisible to a loop that only looks at active ones. FGN_001 sat
// holding a full bin for the rest of the run.
//
// That one node is 41% of the plant's empty-carrier generation. With it dead the
// carrier pool drained, and every producer in the plant — presses, welds, and the
// loaders themselves — eventually had nothing to produce into. 65 orders in, the
// rig stopped completely and stayed stopped for two and a half hours.
//
// ── SO THE SWEEP ASKS THE NODE, NOT THE ORDER ─────────────────────────────
//
// A manual_swap node's actionability is a fact about what is SITTING ON IT:
//
//	produce (loader)   holding an empty carrier  -> LOAD it
//	consume (unloader) holding a full bin        -> CLEAR it
//
// Neither reading needs an order to exist, ever existed, or still exist. That is
// what makes this self-healing rather than one more path that can be missed: any
// way a bin arrives at one of these nodes — an order, a fallback binding, an
// operator's hand, a restart mid-swap — converges here within a reconcile tick.
// It also makes the 8-attempt give-up in run() harmless, because giving up is no
// longer permanent.
//
// IT READS BINS BEFORE IT SCHEDULES, rather than scheduling everything and
// letting classify sort it out. Scheduling a node with nothing to do costs eight
// retries at four seconds apiece, per node, per tick — noise that would bury the
// signal this exists to produce. One FetchNodeBins call answers it for the whole
// plant.
func (op *simOperator) sweepManualSwapNodes() {
	if !op.e.coreClient.Available() {
		return
	}
	nodes, err := op.e.db.ListProcessNodes()
	if err != nil {
		op.e.debugFn("[sim] node sweep: list process nodes: %v", err)
		return
	}
	// wantsEmpty distinguishes the two readings: a loader is waiting for an empty
	// carrier to fill, an unloader for a full bin to drain.
	type target struct {
		id         int64
		wantsEmpty bool
	}
	byName := make(map[string]target, len(nodes))
	names := make([]string, 0, len(nodes))
	for i := range nodes {
		n := nodes[i]
		if n.CoreNodeName == "" {
			continue
		}
		// THE ENGINE METHOD, not the package function — see classifyFromClaim for
		// why that distinction is load-bearing for Core-owned loaders.
		_, runtime, claim, lErr := op.e.loadActiveNode(n.ID)
		if lErr != nil || !claim.IsLoaderNode() {
			continue
		}
		// Same A/B rule the classifier applies: a bin parked at the inactive side
		// is not the operator's to act on.
		if claim.PairedCoreNode != "" && runtime != nil && !runtime.ActivePull {
			continue
		}
		switch claim.Role {
		case protocol.ClaimRoleProduce:
			byName[n.CoreNodeName] = target{id: n.ID, wantsEmpty: true}
		case protocol.ClaimRoleConsume:
			byName[n.CoreNodeName] = target{id: n.ID, wantsEmpty: false}
		default:
			continue
		}
		names = append(names, n.CoreNodeName)
	}
	if len(names) == 0 {
		return
	}
	bins, _, err := op.e.coreClient.FetchNodeBins(names)
	if err != nil {
		return
	}
	swept := 0
	for i := range bins {
		t, ok := byName[bins[i].NodeName]
		if !ok {
			continue
		}
		if !operatorHasWorkAt(t.wantsEmpty, bins[i].BinID, bins[i].UOPRemaining) {
			continue // nothing for the operator to do at this node right now
		}
		op.schedule(t.id) // deduped against anything already pending
		swept++
	}
	if swept > 0 {
		op.e.debugFn("[sim] node sweep: %d manual_swap node(s) holding an actionable bin", swept)
	}
}

// operatorHasWorkAt is the sweep's whole decision: does the bin sitting at this
// node need the operator, given what the node is waiting for.
//
// AN EMPTY SLOT IS NOT AN EMPTY CARRIER, and that distinction is the first thing
// the first live run caught. FetchNodeBins returns a row for every node it was
// ASKED about, not only the ones holding something, so a loader window standing
// empty comes back with BinID 0 and UOPRemaining 0 — which, on UOP alone, reads
// as "an empty carrier waiting to be filled". Every idle loader in the plant got
// scheduled, failed eight times with "no bin at node PLK_X1 — request an empty
// bin first", and did it again on the next tick. The bin id is what separates
// "there is a carrier here and it is empty" from "there is nothing here".
//
// A LOADER wants an empty carrier to fill; an UNLOADER wants a full bin to drain.
// The two are exact opposites, which is what makes one predicate right for both
// and makes getting it backwards produce a plant that looks busy and moves
// nothing — the operator loading full bins and clearing empty ones.
//
// UOP <= 0 IS EMPTY, not UOP == 0, and that boundary matters too: an
// over-consumed carrier is negative, and a negative bin at a loader window is
// still a carrier waiting to be filled. Treating it as "not empty" would leave
// exactly the bins the negative sweep exists to rescue sitting where they are.
func operatorHasWorkAt(wantsEmpty bool, binID int64, uopRemaining int) bool {
	if binID == 0 {
		return false // the node is standing empty; there is nothing to act on
	}
	return (uopRemaining <= 0) == wantsEmpty
}

// negBinMarket IS DELETED. It was `const negBinMarket = "SYN_MARKET"`, carrying
// its own warning: "hardcoded to the demo's combined market name. If the plant
// renames/splits its market group, update this... Untested — no sim run this
// session."
//
// The lane-stress plant does not have a SYN_MARKET. It has SYN_STAMP and
// SYN_COMP, so FetchNodeChildren("SYN_MARKET") returned nothing, marketSlots was
// empty, and the sweep returned at its length check on every single tick. Over a
// two-and-a-half-hour soak it cleared ZERO bins and logged nothing, because it
// only logs when it clears something — a sweep that cannot find its own subject
// looks exactly like a sweep with nothing to do.
//
// Six carriers were stranded negative by the end of that run: half the plant's
// twelve-bin pool, permanently out of circulation, on a rig where the empty
// carrier is the binding resource.
//
// The replacement is not a better constant or a config key. It is a DERIVATION —
// see collectMarketSlots — because a name written down in a second place is a
// name that can disagree with the plant, and this one did, silently, for as long
// as it existed.

// clearNegativeBins resets any negative-UOP bin sitting in the combined market to a
// clean empty (payload cleared, uop 0). Rationale (SB, 2026-07-12): the combined market
// tolerates PARTIAL bins, but a NEGATIVE bin (an over-consumed carrier, e.g. a weld
// overpack of -1/-2) must not re-enter circulation as supply or foul the empty pool.
// This is the (deleted) consume-clear helper's valid goal at a robust seam: poll
// observable market state instead of the fragile EventOrderStatusChanged trigger that
// fired 0. Runs from reconcile() (single goroutine) so marketSlots needs no lock.
func (op *simOperator) clearNegativeBins() {
	if !op.e.coreClient.Available() {
		return
	}
	if op.marketSlots == nil {
		op.marketSlots = op.collectMarketSlots()
	}
	if len(op.marketSlots) == 0 {
		return
	}
	bins, _, err := op.e.coreClient.FetchNodeBins(op.marketSlots)
	if err != nil || len(bins) == 0 {
		return
	}
	cleared := 0
	for i := range bins {
		if bins[i].UOPRemaining < 0 {
			// Core hands back the new generation stamp, and the operator paths at
			// the LINE write it to their runtime row so the station keeps counting
			// under the current one. There is no runtime row here: this sweeps
			// supermarket slots, not a claim's window. So the stamp is dropped on
			// purpose rather than by omission.
			if _, err := op.e.coreClient.ClearBin(bins[i].NodeName, ""); err == nil {
				cleared++
			}
		}
	}
	if cleared > 0 {
		op.e.logFn("[sim] operator cleared %d negative bin(s) across %d market slot(s) (reset to clean empties)", cleared, len(op.marketSlots))
	}
}

// collectMarketSlots enumerates the leaf storage-slot node names under the combined
// market group. FetchNodeChildren(market) returns the lanes (and any direct slots);
// each lane's children are its slots. A child with no children of its own is treated
// as a direct slot, so this is robust without depending on the node-type string.
// It DERIVES the markets from the plant rather than being told their names.
//
// Every manual_swap and swap claim names where its material comes from and where
// it goes — InboundSource and OutboundDestination — and those names ARE the
// plant's markets, by construction: they are what the cells actually draw from
// and push into. A plant with one combined market yields one name; lane-stress
// yields SYN_STAMP and SYN_COMP; a plant nobody has written yet yields whatever
// it uses. Nothing has to be kept in step with anything.
//
// A name that turns out not to be a group (a staging node, a line node named
// directly) has no children and is treated as a single slot, which is the same
// fallback the second level already used for a group's direct slot children.
func (op *simOperator) collectMarketSlots() []string {
	nodes, err := op.e.db.ListProcessNodes()
	if err != nil {
		op.e.debugFn("[sim] market slots: list process nodes: %v", err)
		return nil
	}
	markets := map[string]bool{}
	for i := range nodes {
		_, _, claim, lErr := op.e.loadActiveNode(nodes[i].ID)
		if lErr != nil || claim == nil {
			continue
		}
		for _, name := range []string{claim.InboundSource, claim.OutboundDestination} {
			if name != "" {
				markets[name] = true
			}
		}
	}

	seen := map[string]bool{}
	var slots []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			slots = append(slots, name)
		}
	}
	for market := range markets {
		lanes, _ := op.e.coreClient.FetchNodeChildren(market, true)
		if len(lanes) == 0 {
			add(market) // not a group — a node named directly
			continue
		}
		for _, lane := range lanes {
			laneSlots, _ := op.e.coreClient.FetchNodeChildren(lane.Name, false)
			if len(laneSlots) == 0 {
				add(lane.Name) // direct slot child of the group
				continue
			}
			for _, s := range laneSlots {
				add(s.Name)
			}
		}
	}
	return slots
}

// runRelease dwells for the operator-reaction delay, then pushes the release.
func (op *simOperator) runRelease(orderID int64) {
	defer func() {
		op.mu.Lock()
		delete(op.releasing, orderID)
		op.mu.Unlock()
	}()
	if !op.dwell(op.swapReleaseDelay()) {
		return
	}
	// AND IT MUST STILL HAVE AN ENGINE TO ACT ON. See dwell's note: the sim
	// clock can make the wait above arbitrarily long, and this is the only
	// worker whose first act on waking is to reach into the store.
	if !op.hasStore() {
		return
	}
	// A leg parked at a lane wait is Core's to release (G2), and Core says so
	// in OrderStaged's wait_kind. It used to be invisible from here, which is
	// what the retired release cap stood in for (240 refusals in five minutes
	// on the 2026-08-10 lane-stress rig); Core's next OrderStaged, at the
	// station wait, schedules this again.
	if o, err := op.e.db.GetOrder(orderID); err == nil && o.WaitKind == protocol.WaitKindLane {
		return
	}
	if op.ops.PairRelease {
		op.releaseAsPair(orderID)
		return
	}
	// Two-robot swaps (two_robot, two_robot_press_index) go through the
	// per-NODE release (releaseAsPair -> ReleaseStagedOrders), the same door a
	// real operator's RELEASE BUTTON uses. Both doors now finalize the
	// departing produce bin (finalizeDepartingProduce); the per-node door is
	// kept because it is the one a real operator presses. (Before S1b only
	// that door stamped the manifest, and a two-robot press bin released per
	// leg landed in the supermarket manifest_confirmed=false -- observed:
	// PRESS-1 PANEL-A -> SYN_MARKET, WELD-1 queued forever on "no bin of
	// requested payload".)
	// Sequential / single-robot modes stay on the per-leg path: ReleaseStagedOrders
	// rejects non-two-robot modes, and forcing it would wedge A/B nodes (the
	// pair_release trap documented in the sim-traps memory).
	// A CONSUME NODE GETS ITS FORM FILLED IN ON THIS PATH TOO. The two-robot check
	// above routes the PAIR MECHANISM, which is genuinely two-robot-only; the
	// disposition is not, and used to be reachable only through it. See
	// consumeDisposition for the run that cost — a single_robot consume node whose
	// carriers left unsettled and never came back as empties.
	//
	// Produce keeps the blank disposition it has always sent here; the note above
	// on why two-robot produce is routed to the pair path applies unchanged.
	disp := ReleaseDisposition{}
	if order, err := op.e.db.GetOrder(orderID); err == nil && order != nil && order.ProcessNodeID != nil {
		if _, _, claim, lerr := loadActiveNode(op.e.db, *order.ProcessNodeID); lerr == nil && claim != nil &&
			claim.SwapMode.IsTwoRobot() {
			op.releaseAsPair(orderID)
			return
		}
		if d, ok := op.consumeDisposition(*order.ProcessNodeID); ok {
			disp = d
		}
	}
	// Tolerated failure (order already advanced/cancelled): log at debug.
	if err := op.e.ReleaseOrderWithLineside(orderID, disp); err != nil {
		op.e.debugFn("[sim] operator auto-release order %d rejected: %v", orderID, err)
		return
	}
	op.e.logFn("[sim] operator auto-release order %d (swap-ready)", orderID)
}

// consumeDisposition returns the disposition a real operator would declare for
// the carrier leaving a CONSUME node, and whether this node is a consume node at
// all. It lives outside releaseAsPair because the ANSWER IS NOT ABOUT ROBOTS: it
// is a property of what is physically left in the bin, and both release paths owe
// it.
//
// ── WHY THIS IS SHARED, AND WHAT IT COST TO BE UNSHARED ───────────────────
//
// Core settles a count at RELEASE and nowhere else (SyncOrClearForReleased writes
// uop 0 and the released_empty / released_underpack audit row). A release that
// sends a blank disposition leaves the departing carrier's count unsettled and its
// manifest untouched, so the carrier rides into the pool still labelled — and a
// labelled carrier never satisfies EmptyCarrierWhere's blank-payload_code test
// again. It is empty, and invisible.
//
// This logic used to sit inside releaseAsPair, which is reached only when
// claim.SwapMode.IsTwoRobot(). That gate is correct FOR THE PAIR MECHANISM —
// ReleaseStagedOrders rejects non-two-robot modes and forcing it wedges A/B nodes
// — but the disposition rode along with it, so every single-robot and sequential
// consume node kept sending a blank form.
//
// MEASURED, demo.yaml 2026-08-31. ALN_003 (WELD-2, PANEL-B) is the plant's only
// single_robot CONSUME node, and PANEL-B is the payload that rides STANDARD-SM.
// Eleven SM carriers ended a run all labelled, ZERO manifest-clear operations in
// the whole run, and PRESS-2 dead waiting for an empty that could not exist. The
// fixture's answer was to bolt an unloader onto PANEL-B purely to wipe labels —
// which, because a drain window must be handed a FULL carrier, then destroyed 450
// UOP of PANEL-B in 81 sim-minutes while WELD-2 got none.
//
// A real operator declares what is PHYSICALLY there, so the sim reads the carrier
// and declares the same thing:
//
//	tracked <= 0  -> RELEASE UNDERPACK. The carrier is empty; the tracked count
//	                 disagreeing (negative, from an over-count) is exactly what
//	                 this disposition is for — same wire shape as release-empty,
//	                 and the tag carries the "physical inventory was less than
//	                 tracked" signal so Core records released_underpack.
//	tracked >  0  -> SEND PARTIAL BACK. Stock is left; the bin returns to the pool
//	                 with its count and manifest intact, which is what makes it
//	                 re-sourceable oldest-first.
//
// PRODUCE is deliberately NOT handled here. The per-leg path's produce-role branch
// skips manifest sync (see the caller's note on why two-robot produce is routed to
// the pair path instead), and sequential A/B produce nodes stay per-leg on purpose.
// Declaring capture_lineside for them is a separate change with separate evidence.
func (op *simOperator) consumeDisposition(nodeID int64) (ReleaseDisposition, bool) {
	node, _, claim, err := loadActiveNode(op.e.db, nodeID)
	if err != nil || claim == nil || claim.Role != protocol.ClaimRoleConsume {
		return ReleaseDisposition{}, false
	}
	disp := ReleaseDisposition{CalledBy: "sim-operator", Mode: DispositionSendPartialBack}
	bins, reachable, ferr := op.e.coreClient.FetchNodeBins([]string{node.CoreNodeName})
	switch {
	case ferr == nil && len(bins) > 0 && bins[0].Occupied && bins[0].UOPRemaining <= 0:
		disp.Mode = DispositionReleaseUnderpack
	case !reachable:
		// A HARNESS-FIDELITY HAZARD, NOT A PRODUCTION ONE, and it is worth a line
		// because of what it turns into. The default is SendPartialBack, which
		// returns the carrier LABELLED — and a labelled carrier never satisfies
		// EmptyCarrierWhere's blank-payload test again, so it is empty and
		// invisible. A telemetry blip during a soak run therefore shrinks the
		// empty pool by one, permanently, and reads afterwards exactly like the
		// carrier famine somebody would be running the soak to diagnose.
		op.e.debugFn("sim-operator: node %s — occupancy=%s, defaulting to send-partial-back; "+
			"the carrier returns labelled and leaves the empty pool",
			node.CoreNodeName, OccupancyOutcome(reachable, ferr))
	}
	return disp, true
}

// releaseAsPair pushes the operator's RELEASE BUTTON for the node this order
// belongs to, rather than releasing this one leg.
//
// ── WHY THIS IS A SEPARATE PATH AND NOT A TIDIER SPELLING ────────────────
//
// ReleaseOrderWithLineside is the per-ORDER API door; ReleaseStagedOrders is
// the per-NODE one, and it is the only thing an operator can actually press.
// Everything the pair path owns has no sim coverage while the per-leg path is
// the only one that runs: the lift hold on a placing leg while its sibling is
// still coming, the press paperwork, the intent a not-yet-moving leg is held
// with, and the disposition split that gives the evac leg the operator's
// choice and the supply leg a bare one.
//
// THE DISPOSITION IS COMPUTED, not blank. A blank disposition is what the
// per-leg path sends, and it is exactly what makes the U1 side-cycle trigger
// dormant — the trigger fires on capture_lineside, so a sim that always sends
// "" can never observe it. A produce node's operator is declaring a full bin,
// which is capture_lineside; anything else releases without saying what
// happened to the material.
func (op *simOperator) releaseAsPair(orderID int64) {
	order, err := op.e.db.GetOrder(orderID)
	if err != nil || order == nil || order.ProcessNodeID == nil {
		op.e.debugFn("[sim] operator pair-release: order %d has no process node: %v", orderID, err)
		return
	}
	nodeID := *order.ProcessNodeID
	disp, isConsume := op.consumeDisposition(nodeID)
	if !isConsume {
		disp = ReleaseDisposition{CalledBy: "sim-operator"}
		if _, _, claim, lerr := loadActiveNode(op.e.db, nodeID); lerr == nil && claim != nil &&
			claim.Role == protocol.ClaimRoleProduce {
			disp.Mode = DispositionCaptureLineside
		}
	}
	if err := op.e.ReleaseStagedOrders(nodeID, disp); err != nil {
		// A HELD release is the gate working, not a failure, and it must read
		// that way in the log — a sim run that reports every hold as a rejection
		// is a run nobody can tell a wedge from.
		var held *release.HeldError
		if errors.As(err, &held) {
			op.e.logFn("[sim] operator pair-release node %d HELD: %v — will retry", nodeID, err)
			return
		}
		op.e.debugFn("[sim] operator pair-release node %d rejected: %v", nodeID, err)
		return
	}
	op.e.logFn("[sim] operator pair-release node %d (order %d was swap-ready)", nodeID, orderID)
}

// classifyFromClaim inspects the node's active claim and returns the
// loader/unloader action + delay, or ok=false when the node isn't an
// active-pull manual_swap loader/unloader the sim operator should drive.
func (op *simOperator) classifyFromClaim(nodeID int64) (time.Duration, string, func() error, bool) {
	// THE ENGINE METHOD, NOT THE PACKAGE FUNCTION, and the difference is a whole
	// class of loader.
	//
	// This called the package-level loadActiveNode, which returns claim == nil for
	// any node without a per-style style_node_claim — and a CORE-OWNED loader
	// window has none by design. That is the entire point of the Core-owned loader
	// refactor: Core owns the loader, and the edge operates its windows without a
	// per-style claim. The Engine method exists precisely to synthesize a
	// manual_swap claim for those nodes (operator_helpers.go synthLoaderClaim).
	//
	// So every Core-owned loader window was invisible to the sim operator: a bin
	// delivered to one was never LOADed, and nothing else was going to do it. On
	// lane-stress that is PLK_W1 and PLK_W2, the two windows of the shared BRKT
	// loader — a whole payload's supply, with no operator behind it.
	//
	// A human at the HMI was never affected, because the HMI's load path already
	// goes through the Engine method. Only the headless operator, which is the
	// only operator a soak has.
	node, runtime, claim, err := op.e.loadActiveNode(nodeID)
	if err != nil || node == nil || claim == nil {
		return 0, "", nil, false
	}
	if !claim.IsLoaderNode() {
		return 0, "", nil, false // only operator-driven manual_swap nodes
	}
	// A/B pair: only the active-pull side is the live window — a bin parked at
	// the inactive side is not the operator's to act on (review I4).
	if claim.PairedCoreNode != "" && runtime != nil && !runtime.ActivePull {
		op.e.debugFn("[sim] operator: skip inactive A/B side %s", node.CoreNodeName)
		return 0, "", nil, false
	}
	switch claim.Role {
	case protocol.ClaimRoleProduce: // loader: empty bin arrived → LOAD it
		c := claim
		return op.loaderDelay(), "load", func() error { return op.loadBin(nodeID, c) }, true
	case protocol.ClaimRoleConsume: // unloader: full bin arrived → CLEAR it
		return op.unloaderDelay(), "clear", func() error { return op.e.ClearBin(nodeID, "") }, true
	}
	return 0, "", nil, false
}

// loadBin synthesizes a single-item manifest from the claim's payload + capacity
// (a human operator scans a card; the sim just fills the configured payload).
//
// BOTH FIELDS FALL BACK, because a Core-owned loader's claim is SYNTHESIZED and
// carries neither. SynthClaim supplies the six facts Core owns — role, swap
// mode, allowed payloads, inbound source, outbound destination, auto-confirm —
// and PayloadCode and UOPCapacity are not among them, so a loader window with no
// stored style_node_claim reads payload "" and capacity 0. LoadBin refuses a
// blank payload outright ("no payload code specified"), which would have stopped
// the sim's loaders the moment their stored claims were quarantined.
//
// The fallbacks are the right authorities rather than convenient ones. The
// payload comes from the claim's ALLOWED set, which SynthClaim scopes to this
// node — a dedicated home carries only its own pinned part, a shared window the
// whole set — so it is the same list the load gate accepts and the same card an
// operator would be shown. The capacity comes from payload_catalog, which is the
// Edge's mirror of Core's payload definitions and where a part's standard pack
// actually lives; claim.UOPCapacity is a per-cell policy number that a loader
// window has no business owning. A stored claim that carries its own values
// still wins, so nothing about a robot-served cell changes.
func (op *simOperator) loadBin(nodeID int64, claim *processes.NodeClaim) error {
	payload := claim.PayloadCode
	if payload == "" {
		if codes := claim.AllowedPayloads(); len(codes) > 0 {
			payload = codes[0]
		}
	}
	capacity := int64(claim.UOPCapacity)
	if capacity <= 0 && payload != "" {
		if entry, err := op.e.db.GetPayloadCatalogByCode(payload); err == nil && entry != nil && entry.UOPCapacity > 0 {
			capacity = int64(entry.UOPCapacity)
		}
	}
	if capacity <= 0 {
		capacity = 1
	}
	manifest := []protocol.IngestManifestItem{{PartNumber: payload, Quantity: capacity}}
	// The sim declares a count, so it travels as a value rather than as the
	// absence that asks Core for the standard pack.
	return op.e.LoadBin(nodeID, payload, &capacity, manifest)
}
