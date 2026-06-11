//go:build sim

package engine

import (
	"context"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/shared/clock"
	"shingoedge/config"
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
}

// StartSimOperator wires the sim operator to the EventBus. Sim builds only;
// called from the edge composition root's startSimSubsystems when
// sim.operators.enabled. The driver/fake run on their own clocks today; a
// shared clock for a manual-clock integration harness is deferred (J16).
func (e *Engine) StartSimOperator(ctx context.Context, simCfg config.SimConfig, clk clock.Clock) {
	op := &simOperator{
		e:         e,
		ops:       simCfg.Operators,
		clk:       clk,
		ctx:       ctx,
		pending:    make(map[int64]bool),
		releasing:  make(map[int64]bool),
		flipping:   make(map[int64]bool),
		confirming: make(map[int64]bool),
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
		op.loaderDelay(), op.unloaderDelay(), swapReleaseDelay)
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
	op.schedule(*d.ProcessNodeID)              // LOAD/CLEAR for manual_swap nodes
	op.scheduleConfirm(d.OrderID, *d.ProcessNodeID) // sign off swap legs delivered to a line node
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
//   - removal legs deliver to the supermarket, not the line node, so
//     DeliveryNode != CoreNodeName filters them out (they auto-confirm already).
//   - re-checks status==delivered after the dwell so a racing confirm is a no-op.
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
	if order.DeliveryNode != node.CoreNodeName {
		return // only legs that bind a bin AT this node (resupply / A/B backfill)
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

// schedule dedupes by node and spawns the delayed worker. Never blocks, so it's
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
	if err := action(); err != nil {
		// Tolerated: a precondition moved between delivery and now (bin already
		// loaded, order cancelled, …). Engine validation rejected it — log at
		// debug and move on.
		op.e.debugFn("[sim] operator auto-%s node %d rejected: %v", label, nodeID, err)
		return
	}
	op.e.logFn("[sim] operator auto-%s node %d", label, nodeID)
}

// swapReleaseDelay is the simulated operator reaction time between a swap
// reaching its swap-ready wait (status "staged") and the operator pushing
// Release. The sim clock's After scales it by live speed.
const swapReleaseDelay = 3 * time.Second

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
	if !ok || d.NewStatus != "staged" {
		return
	}
	op.mu.Lock()
	if op.releasing[d.OrderID] {
		op.mu.Unlock()
		return
	}
	op.releasing[d.OrderID] = true
	op.mu.Unlock()
	go op.runRelease(d.OrderID)
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
	case <-op.clk.After(swapReleaseDelay):
	}
	// Empty disposition: release the swap without touching the bin manifest. The
	// sim isn't modeling SEND PARTIAL / RELEASE EMPTY accounting — just the
	// "operator pushed Release" transition that lets the staged swap finish.
	// Tolerated failure (order already advanced/cancelled): log at debug.
	if err := op.e.ReleaseOrderWithLineside(orderID, ReleaseDisposition{}); err != nil {
		op.e.debugFn("[sim] operator auto-release order %d rejected: %v", orderID, err)
		return
	}
	op.e.logFn("[sim] operator auto-release order %d (swap-ready)", orderID)
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
	if err := op.e.FlipABNode(paired.ID); err != nil {
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
	node, runtime, claim, err := loadActiveNode(op.e.db, nodeID)
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
		return op.unloaderDelay(), "clear", func() error { return op.e.ClearBin(nodeID) }, true
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
