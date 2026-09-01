//go:build sim

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/config"
	"shingoedge/orders"
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
// Deferred within T3.2 (noted in AGENT-REPORT): auto-cutover for changeover
// (operators.changeover_auto_cutover) and the EventCounterDelta→0 unloader
// trigger. The delivery-driven LOAD/CLEAR below is the core of the four loops.

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
	flipping   map[int64]bool // A/B active nodes with a cutover scheduled/in-flight
	confirming map[int64]bool // delivered swap legs with a confirm scheduled/in-flight
	// releaseTries counts consecutive Release pushes per order that did not stick.
	// See maxReleaseTries: the Edge cannot see a Core-owned lane wait, so the only
	// signal that a button is not ours to push is the order coming back `staged`.
	releaseTries map[int64]int
	// cappedAt records when an order hit maxReleaseTries, so the cap can be
	// re-armed instead of becoming a permanent give-up. See releaseCapReArm.
	cappedAt map[int64]time.Time

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
		e:            e,
		ops:          simCfg.Operators,
		clk:          clk,
		ctx:          ctx,
		pending:      make(map[int64]bool),
		releasing:    make(map[int64]bool),
		flipping:     make(map[int64]bool),
		confirming:   make(map[int64]bool),
		releaseTries: make(map[int64]int),
		cappedAt:     make(map[int64]time.Time),
	}
	op.classify = op.classifyFromClaim
	// The bus is synchronous (D4): handlers must not block — they dedupe and
	// spawn a delayed worker, then return. onDelivered drives the post-delivery
	// LOAD/CLEAR; onStatusChanged drives the swap-ready release; onOrderCreated
	// drives the A/B cutover (the PLC-bit stand-in).
	e.Events.SubscribeTypes(op.onDelivered, EventOrderDelivered)
	e.Events.SubscribeTypes(op.onStatusChanged, EventOrderStatusChanged)
	e.Events.SubscribeTypes(op.onOrderCreated, EventOrderCreated)
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
	if claim.SwapMode == protocol.SwapModeManualSwap {
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

	select {
	case <-op.ctx.Done():
		return
	case <-op.clk.After(confirmDelay):
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
	select {
	case <-op.ctx.Done():
		return
	case <-op.clk.After(delay):
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
		select {
		case <-op.ctx.Done():
			return
		case <-op.clk.After(retryDelay):
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
// maxReleaseTries is how many times the sim operator will push Release on one
// order before concluding the button is not its to push.
//
// ── WHY A CAP, AND WHY IT CANNOT BE AN ERROR CHECK ────────────────────────
//
// A LANE-waiting order is CORE's to release, not the station's, and Core enforces
// that: "release refused: order N is parked on a gate wait — only the lane
// evaluator advances one". But the Edge cannot tell the two kinds of wait apart.
// Core INSERTS the lane wait into its own plan (spliceLaneWait) and never sends
// it back, so the Edge's steps_json is the Edge-authored choreography with no
// lane wait in it. From here, a gate-staged order and a swap-ready one look
// identical.
//
// Worse, the refusal is INVISIBLE at the call site. ReleaseOrderWithLineside
// returns nil — the Edge transitions the order staged→in_transit locally and logs
// a successful release. Core's rejection arrives much later, asynchronously, as an
// inbound order.error that puts the row back to `staged`, which re-fires
// onStatusChanged, which releases again. There is no error to back off from,
// which is why this is a cap and not a retry policy.
//
// MEASURED on the lane-stress rig, 2026-08-10: 240 refusals in five minutes
// across four orders — one every 1.25 seconds, indefinitely, each round trip
// costing an outbox row and a Kafka publish. 1796 outbox messages on a plant that
// had completed 46 orders.
//
// THE COUNTER CLEARS WHEN THE ORDER ACTUALLY LEAVES `staged` (see reconcile), and
// that is what makes the cap self-correcting rather than a permanent give-up: a
// lane-waiting order stays `staged` and stays capped until Core lets it in, at
// which point it leaves and the count is dropped. Three is a human's patience,
// not a tuned number.
const maxReleaseTries = 3

// releaseCapReArm is how long a capped order is left alone before the sim
// operator gives it another round of tries.
//
// ── WHY THE CAP RE-ARMS, AND WHY IT USED NOT TO ───────────────────────────
//
// The cap's premise was that a capped order is CORE's to release: "a lane-waiting
// order stays staged and stays capped until Core lets it in, at which point it
// leaves and the count is dropped." That is true of a LANE wait and false of a
// STATION wait — and Core is explicit that it must never advance one of those
// ("the precondition is a fact only the station can observe", queue_releasers.go).
// The station, in the sim, IS this operator. So a station-waiting order that hit
// the cap had its only possible releaser go quiet for good.
//
// MEASURED, 12e run 2026-08-31. Order 91 took three pair-releases inside ONE
// SECOND (15:25:31, :32, :32), hit the cap, and was abandoned holding AMR-15 for
// the rest of the run under the message above — which told the reader it was
// "most likely parked on a LANE wait" when its own plan says
// `wait SLN_010 wait_kind=station`. 24 orders hit the cap in that run.
//
// A BOUNDED BACKOFF, NOT A RETRY LOOP. The original incident this cap was written
// for (lane-stress 2026-08-10: 240 refusals in five minutes, one every 1.25s,
// 1796 outbox rows for 46 completed orders) stays fixed: three tries per re-arm
// window is ~3/minute against ~48/minute measured then, a 16x reduction, and a
// genuinely lane-waiting order simply re-caps each window until Core lets it in.
// What it stops being is permanent.
const releaseCapReArm = time.Minute

func (op *simOperator) scheduleRelease(orderID int64) {
	op.mu.Lock()
	if op.releasing[orderID] {
		op.mu.Unlock()
		return
	}
	if op.releaseTries[orderID] >= maxReleaseTries {
		if op.releaseTries[orderID] == maxReleaseTries {
			op.releaseTries[orderID]++ // once past the cap, say so once and go quiet
			// UNDER THE LOCK. cappedAt is read and deleted by
			// reArmExpiredReleaseCaps from the reconcile goroutine, which holds
			// op.mu to do it; this write was outside the mutex when the re-arm
			// landed, which is a concurrent map write — a panic, not a stale
			// read. The race detector never saw it: gate.sh's race step runs
			// shingo-core only and passes no `sim` tag, so nothing in this file
			// is ever built under -race.
			op.cappedAt[orderID] = op.clk.Now()
			op.mu.Unlock()
			op.e.logFn("[sim] operator: order %d has refused release %d times — backing off for %s. %s",
				orderID, maxReleaseTries, releaseCapReArm, op.releaseCapDiagnosis(orderID))
			return
		}
		op.mu.Unlock()
		return
	}
	op.releaseTries[orderID]++
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
	// THE RELEASE CAP CLEARS HERE, and the condition is "the order actually got
	// somewhere" — NOT "the order is not staged right now".
	//
	// THE OBVIOUS VERSION OF THIS WAS WRONG AND THE RIG SAID SO. Clearing whenever
	// an order was not currently `staged` looked right and did nothing: a refused
	// order flaps staged → in_transit → order.error → staged about once a second,
	// so this ten-second sweep lands in the in_transit half often enough to reset
	// the count before it can ever reach three. Deployed, measured, still 48
	// refusals a minute and not one cap announcement.
	//
	// The flap is exactly the thing being capped, so the release from the cap
	// cannot be a state the flap passes through. `delivered` and terminal are not:
	// a release that STUCK carries the leg onward, and one that was refused never
	// gets there. An order that has gone quiet under the cap simply keeps its count
	// until it either lands or dies.
	progressed := func(o storeorders.Order) bool { return o.Status == protocol.StatusDelivered }
	live := make(map[int64]bool, len(active))
	for i := range active {
		if !progressed(active[i]) {
			live[active[i].ID] = true
		}
	}
	op.reArmExpiredReleaseCaps(live)

	pending := 0
	for i := range active {
		o := active[i]
		switch o.Status {
		case protocol.StatusStaged:
			op.scheduleRelease(o.ID)
			// ── AND THE A/B CUTOVER, WHICH THIS LOOP DID NOT DRIVE ────────
			//
			// scheduleFlip had exactly ONE caller — onOrderCreated — so the
			// cutover was LIVE-EVENT-ONLY. That is a hole shaped precisely like
			// a deadlock, and the fixture found it:
			//
			//	[sim] operator auto-release order 164 rejected: the line is
			//	      pulling from PLN_003; flip to PLN_004 first
			//
			// The evac at a sequential press cannot release until the line
			// flips to the paired side. Nothing flips unless an order is
			// CREATED against that node. No order can be created for a cell
			// whose swap is stuck. Measured 2026-08-30: 444 refusals of that one
			// message, 500 release-cap announcements, five robots pinned, and
			// PANEL-B production stopped for four sim-hours — with the operator
			// retrying an action that could not succeed until it took a
			// different one first.
			//
			// This loop exists to "re-derive pending operator actions from
			// current state" so a live-only trigger cannot strand the plant. It
			// re-derived three of the four. runFlip is already idempotent and
			// already short-circuits on !ActivePull and on a non-sequential
			// claim, so asking it once per staged order per sweep costs a read
			// and answers nothing new when there is nothing to flip.
			if o.ProcessNodeID != nil {
				op.scheduleFlip(*o.ProcessNodeID)
			}
			pending++
		case protocol.StatusDelivered:
			if o.ProcessNodeID != nil {
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
		if lErr != nil || claim == nil || claim.SwapMode != protocol.SwapModeManualSwap {
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
	select {
	case <-op.ctx.Done():
		return
	case <-op.clk.After(op.swapReleaseDelay()):
	}
	if op.ops.PairRelease {
		op.releaseAsPair(orderID)
		return
	}
	// Two-robot swaps (two_robot, two_robot_press_index) go through the
	// per-NODE release (releaseAsPair -> ReleaseStagedOrders), the same door a
	// real operator's RELEASE BUTTON uses. That path runs the deferred produce
	// paperwork (produceIngestAtRelease) that stamps + confirms the departing
	// bin's manifest. The per-LEG release below sends a blank disposition and
	// the produce-role branch skips manifest sync, so a two-robot press bin
	// would land in the supermarket manifest_confirmed=false -- invisible to
	// the retrieve resolver's manifest_confirmed gate -- and the downstream
	// consumer starves once the seeded stock drains (observed: PRESS-1 PANEL-A
	// -> SYN_MARKET, WELD-1 queued forever on "no bin of requested payload").
	// Sequential / single-robot modes stay on the per-leg path: ReleaseStagedOrders
	// rejects non-two-robot modes, and forcing it would wedge A/B nodes (the
	// pair_release trap documented in the sim-traps memory).
	if order, err := op.e.db.GetOrder(orderID); err == nil && order != nil && order.ProcessNodeID != nil {
		if _, _, claim, lerr := loadActiveNode(op.e.db, *order.ProcessNodeID); lerr == nil && claim != nil &&
			claim.SwapMode.IsTwoRobot() {
			op.releaseAsPair(orderID)
			return
		}
	}
	// Empty disposition: release the swap without touching the bin manifest. The
	// sim isn't modeling SEND PARTIAL / RELEASE EMPTY accounting -- just the
	// "operator pushed Release" transition that lets the staged swap finish.
	// Tolerated failure (order already advanced/cancelled): log at debug.
	if err := op.e.ReleaseOrderWithLineside(orderID, ReleaseDisposition{}); err != nil {
		op.e.debugFn("[sim] operator auto-release order %d rejected: %v", orderID, err)
		return
	}
	op.e.logFn("[sim] operator auto-release order %d (swap-ready)", orderID)
}

// releaseAsPair pushes the operator's RELEASE BUTTON for the node this order
// belongs to, rather than releasing this one leg.
//
// ── WHY THIS IS A SEPARATE PATH AND NOT A TIDIER SPELLING ────────────────
//
// ReleaseOrderWithLineside is the per-ORDER API door; ReleaseStagedOrders is
// the per-NODE one, and it is the only thing an operator can actually press.
// Everything the pair path owns has no sim coverage while the per-leg path is
// the only one that runs: the collision gate that holds a placing leg while its
// sibling is still coming, the produce paperwork and its ordering against that
// gate, the deferred-sibling re-fire when only one leg was releasable, and the
// disposition split that gives the evac leg the operator's choice and the
// supply leg a bare one.
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
	disp := ReleaseDisposition{CalledBy: "sim-operator"}
	if node, _, claim, lerr := loadActiveNode(op.e.db, nodeID); lerr == nil && claim != nil {
		switch claim.Role {
		case protocol.ClaimRoleProduce:
			disp.Mode = DispositionCaptureLineside
		case protocol.ClaimRoleConsume:
			// THE CONSUME MIRROR, and it was missing. A consume claim fell through
			// to the zero value, which means "release the swap without touching the
			// bin manifest" — so the departing carrier's count was never settled and
			// rode into the pool as-is. A carrier over-consumed to -6 stayed at -6 in
			// a buffer, and nothing downstream would ever zero it: Core settles a
			// count at RELEASE (SyncOrClearForReleased writes uop 0 and the
			// released_empty / released_underpack audit row), and release is the only
			// place it does.
			//
			// A real operator declares what is PHYSICALLY there, so the sim reads the
			// carrier and declares the same thing:
			//
			//   tracked <= 0  -> RELEASE UNDERPACK. The carrier is empty; the tracked
			//                    count disagreeing (negative, from an over-count) is
			//                    exactly what this disposition is for — same wire
			//                    shape as release-empty, and the tag carries the
			//                    "physical inventory was less than tracked" signal so
			//                    Core records released_underpack.
			//   tracked >  0  -> SEND PARTIAL BACK. Stock is left; the bin returns to
			//                    the pool with its count and manifest intact, which is
			//                    what makes it re-sourceable oldest-first.
			disp.Mode = DispositionSendPartialBack
			if bins, _, ferr := op.e.coreClient.FetchNodeBins([]string{node.CoreNodeName}); ferr == nil &&
				len(bins) > 0 && bins[0].Occupied && bins[0].UOPRemaining <= 0 {
				disp.Mode = DispositionReleaseUnderpack
			}
		}
	}
	if err := op.e.ReleaseStagedOrders(nodeID, disp); err != nil {
		// A HELD release is the gate working, not a failure, and it must read
		// that way in the log — a sim run that reports every hold as a rejection
		// is a run nobody can tell a wedge from.
		var notReady *SwapPairNotReadyError
		if errors.As(err, &notReady) {
			op.e.logFn("[sim] operator pair-release node %d HELD: %v — will retry", nodeID, err)
			return
		}
		op.e.debugFn("[sim] operator pair-release node %d rejected: %v", nodeID, err)
		return
	}
	op.e.logFn("[sim] operator pair-release node %d (order %d was swap-ready)", nodeID, orderID)
}

// onOrderCreated is the A/B cutover trigger — the headless stand-in for the PLC
// bit. A real plant's PLC flips active_pull to the partner bin when the active
// bin's swap fires; the sim has no PLC, so when a produce-side A/B (sequential)
// node dispatches its swap order, flip active_pull to its paired partner so the
// line keeps producing on the partner while this bin swaps out. Must not block
// (synchronous bus): dedupe and spawn, then return.
func (op *simOperator) onOrderCreated(ev Event) {
	d, ok := ev.Payload.(OrderCreatedEvent)
	if !ok || d.ProcessNodeID == nil {
		return
	}
	op.scheduleFlip(*d.ProcessNodeID)
}

// scheduleFlip dedupes by the active node and spawns the cutover worker.
func (op *simOperator) scheduleFlip(nodeID int64) {
	op.mu.Lock()
	if op.flipping[nodeID] {
		op.mu.Unlock()
		return
	}
	op.flipping[nodeID] = true
	op.mu.Unlock()
	go op.runFlip(nodeID)
}

func (op *simOperator) runFlip(nodeID int64) {
	defer func() {
		op.mu.Lock()
		delete(op.flipping, nodeID)
		op.mu.Unlock()
	}()

	node, runtime, claim, err := loadActiveNode(op.e.db, nodeID)
	if err != nil || node == nil || claim == nil || runtime == nil {
		return
	}
	// Only a produce-side A/B (sequential + paired) node that is CURRENTLY the
	// active-pull side. After we flip it inactive, the backfill order's
	// EventOrderCreated re-enters here and short-circuits on !ActivePull.
	if claim.SwapMode != protocol.SwapModeSequential || claim.PairedCoreNode == "" ||
		claim.Role != protocol.ClaimRoleProduce || !runtime.ActivePull {
		return
	}
	paired := op.pairedNode(node.ProcessID, claim.PairedCoreNode)
	if paired == nil {
		return
	}
	// FlipABNode(x) makes x active and its partner (this node) inactive.
	if err := op.e.FlipABNode(paired.ID, OperatorFlip("sim-operator")); err != nil {
		op.e.debugFn("[sim] A/B cutover %s→%s rejected: %v", node.CoreNodeName, claim.PairedCoreNode, err)
		return
	}
	op.e.logFn("[sim] A/B cutover: %s → %s (active bin swapping out)", node.CoreNodeName, claim.PairedCoreNode)
}

// pairedNode resolves the process node with the given core-node name in a process.
func (op *simOperator) pairedNode(processID int64, coreName string) *processes.Node {
	nodes, err := op.e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return nil
	}
	for i := range nodes {
		if nodes[i].CoreNodeName == coreName {
			return &nodes[i]
		}
	}
	return nil
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
	if claim.SwapMode != protocol.SwapModeManualSwap {
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
func (op *simOperator) loadBin(nodeID int64, claim *processes.NodeClaim) error {
	payload := claim.PayloadCode
	capacity := int64(claim.UOPCapacity)
	if capacity <= 0 {
		capacity = 1
	}
	manifest := []protocol.IngestManifestItem{{PartNumber: payload, Quantity: capacity}}
	return op.e.LoadBin(nodeID, payload, capacity, manifest)
}

// reArmExpiredReleaseCaps drops the release cap for orders that have progressed
// (or gone) and for those that have sat under it longer than releaseCapReArm.
//
// The first half is the original clearing rule and its reasoning is above: the
// release from the cap cannot be a state the refusal-flap passes through, so it
// keys on `delivered` rather than on "not currently staged".
//
// The second half is the re-arm, and it exists because the cap's premise is only
// half true. See releaseCapReArm.
func (op *simOperator) reArmExpiredReleaseCaps(live map[int64]bool) {
	now := op.clk.Now()
	op.mu.Lock()
	defer op.mu.Unlock()
	for id := range op.releaseTries {
		if !live[id] { // delivered, or gone from the active set entirely (terminal)
			delete(op.releaseTries, id)
			delete(op.cappedAt, id)
			continue
		}
		at, capped := op.cappedAt[id]
		if !capped || now.Sub(at) < releaseCapReArm {
			continue
		}
		delete(op.releaseTries, id)
		delete(op.cappedAt, id)
		op.e.logFn("[sim] operator: order %d has waited %s under the release cap — trying again in "+
			"case the button was mine to push all along", id, releaseCapReArm)
	}
}

// releaseCapDiagnosis is what the sim operator can HONESTLY say about an order
// it has just stopped pushing Release on.
//
// ── WHAT IT REPLACED, AND WHY THAT MATTERED ───────────────────────────────
//
// The cap's announcement used to assert the order was "most likely parked on a
// LANE wait, which only Core's lane evaluator can advance". Order 91 in the 12e
// run took three pair-releases inside one second, hit the cap, held AMR-15 for
// the rest of the run, and printed that sentence — while its own plan said
// `wait SLN_010 wait_kind=station`. The sentence sent every reader looking for a
// lane fence that was not refusing anything, and it was the reason the whole
// population was filed as a swap peer-terminal orphan for two rounds.
//
// ── WHAT THE EDGE ACTUALLY KNOWS, WHICH IS LESS THAN IT WANTED TO SAY ─────
//
// It knows its OWN plan, and every wait the Edge authors is station-owned by
// construction (material_orders.go stationWait). It knows the sibling's status.
// It does NOT know which wait the robot is standing at: Core splices its lane
// waits into ITS copy of the plan and never sends them back, and
// protocol.OrderStaged carries a uuid and a detail string — no wait index, no
// wait kind. So the honest report is the two facts plus the missing one, and
// naming the gap is the part that stops the next reader repeating order 91's
// investigation.
func (op *simOperator) releaseCapDiagnosis(orderID int64) string {
	order, err := op.e.db.GetOrder(orderID)
	if err != nil {
		return "The Edge cannot read the order to say more about what it is waiting on."
	}

	partner := "It has no swap partner."
	if order.SiblingOrderID != nil {
		if sib, sErr := op.e.db.GetOrder(*order.SiblingOrderID); sErr == nil {
			partner = fmt.Sprintf("Its swap partner %d is %s.", sib.ID, sib.Status)
			if orders.IsTerminalSuccess(sib.Status) {
				partner += " That partner already ran its half, so nothing is coming for this leg" +
					" to make room for — see releaseSurvivorOfFinishedPartner, which releases this" +
					" population without a click."
			}
		} else {
			partner = fmt.Sprintf("Its swap partner %d could not be read.", *order.SiblingOrderID)
		}
	}

	plan := "Its plan declares no waits."
	if stepsJSON, sErr := op.e.db.GetOrderStepsJSON(orderID); sErr == nil {
		if steps, dErr := decodeSteps(stepsJSON); dErr == nil {
			kinds := make([]string, 0, len(steps))
			for _, s := range steps {
				if s.Action != protocol.ActionWait {
					continue
				}
				kind := s.WaitKind
				if kind == "" {
					kind = "station (untagged)"
				}
				node := s.Node
				if node == "" {
					node = "(no node)"
				}
				kinds = append(kinds, node+"="+kind)
			}
			if len(kinds) > 0 {
				plan = "Its own plan's waits are " + strings.Join(kinds, ", ") + "."
			}
		}
	}

	return partner + " " + plan +
		" Which of them it is standing at is not knowable from the Edge: Core splices its lane" +
		" waits into its own copy of the plan and the staged notification carries no wait index."
}
