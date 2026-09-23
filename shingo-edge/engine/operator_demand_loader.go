package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"

	"shingoedge/domain"
	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
)

// manualSwapWindowSlots is how many bins a single manual_swap core node can
// physically stage at its window — one (one physical slot per window/position).
//
// The LOADER empty path no longer reads this constant: withLoaderBudget
// derives the budget from the delivery-node SET cardinality (one bin per node),
// so a multi-window loader's budget grows to N when delivery spreads
// without a magic number, and the per-payload dedup + capacity cap are unified in
// the seam. The unloader's full-in derives its budget the same way
// (decideLoaderBudget), so no production path reads the constant. It remains as
// the expected one-window cap in the capacity tests (loader_capacity_cap_test.go,
// try_create_l1_test.go), and it documents the one-bin-per-node physical model
// the operator-path anti-spam guard also encodes (operator_bin_ops.go).
const manualSwapWindowSlots = 1

// A produce loader's automatic replenishment is now decided entirely on Core.
// The Edge used to receive a below-threshold signal and work out how many
// carriers were needed and where they went; that half is gone, and Core creates
// the orders itself. What is left here is the OPERATOR side: the opportunistic
// window-free push (rePushOwnLoader, SweepPushLoaders), the operator's own requests, and the
// unloader path — all still through the withLoaderBudget seam.
//
// Two earlier retirements, kept as gravestones because their names still appear
// in incident records: the bin-count produce DemandSignal trigger
// (MaybeCreateLoaderEmptyIn + findLoaderForDemand + refillLoaderForPayload), and
// the threshold receiver that replaced it (HandleLoopBelowThreshold + its
// park/replay machinery). A third followed (2026-08): the DemandSignal wire
// subject itself was deleted — Core no longer emits it, so the unloader's U1
// full-in now fires from operator release alone (operator_release.go).
//
// A fourth retirement: the L1Source type (L1SideCycle, L1LoopThreshold,
// L1LoaderPush) and its operator-driven suppression policy. It existed to force
// a class decision whenever a second L1 source was added; with the threshold
// source moved to Core only the operator push is left, so the log prefix
// `loader_push:` is now a literal in stageOperatorEmpty.

// loaderBudgetLock returns the per-loader reservation mutex, creating it on first
// use. Keyed by loader id so two loaders never
// block each other — a slow burst on loader X can't stall loader Y.
func (e *Engine) loaderBudgetLock(loaderID string) *sync.Mutex {
	m, _ := e.loaderResv.LoadOrStore(loaderID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// withLoaderBudget makes count→fire atomic for a loader. Under the loader's
// mutex it counts non-terminal retrieve orders across the delivery-node set in
// ONE snapshot, applies the per-payload dedup and the loader-capacity cap, and
// fires the remainder via the caller's `fire` closure — all without releasing
// the lock, so a concurrent operator request or push sweep cannot interleave
// between the count and the create.
//
// SCOPE — this is the never-2N guarantee only for the writers that route
// through here: stageOperatorEmpty (the opportunistic push, via
// maybeStageLoaderEmpty, from rePushOwnLoader and SweepPushLoaders), RequestEmptyBin's manual_swap branch
// and RequestFullBin (operator), and CreateRetrieveForAPI (the HTTP order API,
// since Deploy 1b). The automatic U1 (createUnloaderFullIns) takes the same
// per-loader mutex and the same count/cap/target step (decideLoaderBudget) but
// not this function: it decides every payload of an unloader from one read.
// Core's own threshold replenishment no longer passes through here at all: it
// does not run here any more. It is NOT a universal chokepoint.
// Per the 2026-07-31 census this creates loader-window retrieves WITHOUT passing
// through here: RequestEmptyBin's simple mode. The HTTP order API used to be
// named here too and no longer belongs — Deploy 1b routed it through the seam
// (api_retrieve.go). The changeover paths (changeover_applier.go,
// operator_node_changeover.go) also create retrieves outside this seam; whether
// either can target a loader window was NOT established and is an open question,
// not a cleared one.
//
// The press-index partial-empty prime creates outside this seam DELIBERATELY,
// from TWO sites now: applyProducePlan (REQUEST SWAP) and
// primeBarePressIndexPositions (REQUEST EMPTY BIN, added 2026-08-26 — the same
// guard was missing on that door and a bare paired position minted a swap whose
// index leg could never source). Both have the same delivery node: a press's
// bare paired POSITION, never a loader window, so this seam's budget — one bin
// per delivery node across a loader's window set — has nothing to say about
// either. Both carry the same count->decide->create lock for the same never-2N
// reason at their own grain: Engine.primeResv, keyed by the cell's core node,
// around pairedPositionsAlreadyPrimed — and because the key is the CORE node
// they serialise against each other, so an operator hitting one door while the
// other is mid-prime cannot double-fire. The InboundSource they pull FROM may
// well be a loader group; that is Core's resolver's business, not this budget's.
//
// An earlier version of this comment claimed EVERY empty-firing writer routed
// through here. It did not, and the claim was load-bearing in two review rounds
// before a census refuted it. TestCensus_RetrieveOrderCreatorSites now fails when
// the creator count changes, so this list has a tripwire instead of only good
// intentions. Re-run the census before relying on this for a system invariant.
//
// want is the desired TOTAL in-flight for this payload; toFire = want minus what
// is already in flight for the payload, capped to the loader's free capacity
// (budget = one bin per delivery node, minus all in-flight empties across the set).
//
// NO transaction, by design. The only operation that RAISES a loader's empty
// count is the create inside `fire`; every other mutation (completion,
// cancellation, failure) only lowers it, so serialising the up-writers with the
// mutex makes the count monotone-safe without DB isolation. And
// CreateRetrieveOrder is not transaction-pure — it enqueues to Core and fires a
// synchronous EmitOrderCreated mid-write — so a surrounding tx could only
// manufacture the Core/Edge divergence it was meant to prevent. See
// FINAL-ADJUDICATION Q1 (monotonicity + unsoundness arguments) —
// shingo-library/archive/bin-loader-multiwindow-reviews-2026-06-12/FINAL-ADJUDICATION.md.
//
// Fails CLOSED: a count read error fires nothing; the next signal retries.
//
// RE-ENTRANCY RULE (pinned, do not assume — it is enforced by a test): `fire`
// runs while the loader's mutex is held and calls CreateRetrieveOrder, which
// fires EmitOrderCreated SYNCHRONOUSLY on the in-process event bus. No
// order-event subscriber may call back into the reservation seam for the SAME
// loader — sync.Mutex is non-reentrant and would self-deadlock. If a subscriber
// ever needs to re-enter, split reserve-from-fire (end the lock after the DB
// insert; enqueue/emit after release). TestWithLoaderBudget_EmitDuringReservation
// guards that the live subscribers do not re-enter.
// It serves BOTH directions: a loader's empty-in (retrieveEmpty=true) and an
// unloader's full-in (retrieveEmpty=false). It began as an empty-only function;
// the body turned out to be role-agnostic apart from the in-flight filter, so the
// consume side shares it (RequestFullBin here; the automatic U1 through
// decideLoaderBudget) rather than re-implementing the count and cap — the
// loader/unloader drift this codebase keeps re-growing. retrieveEmpty selects
// which in-flight orders the budget counts; the caller's fire closure creates the
// matching order type.
//
// NAMING: it was called reserveLoaderEmpties, then reserveLoaderBins. Neither
// reserved anything. Nothing is held here — the budget is recomputed from the
// order table on every call, and a "reservation" that survives no longer than the
// mutex is not one. The current name says what it does: run the caller's fire
// closure with the loader's budget enforced around it.
func (e *Engine) withLoaderBudget(loader *domain.Loader, payload domain.PayloadCode, want int, member domain.NodeID, retrieveEmpty bool, fire func(deliveryNodes []string) (int, error)) (int, error) {
	if loader == nil || want <= 0 {
		return 0, nil
	}
	// The Loader owns the reservation shape: which nodes the count spans and the
	// budget. multiWindowFor gates whether THIS shared loader spreads across its
	// windows (budget = SlotCount) or takes one at a time (budget 1); member
	// routes a dedicated reservation to the position the signal named —
	// see domain.Loader.ReservationTarget.
	nodes, budget := loader.ReservationTarget(member, payload, e.multiWindowFor(loader))
	if len(nodes) == 0 || budget <= 0 {
		return 0, nil // loader doesn't serve this payload — no target
	}
	deliveryNodes := nodeIDStrings(nodes)
	loaderID := string(loader.ID())
	pay := string(payload)

	mu := e.loaderBudgetLock(loaderID)
	mu.Lock()
	defer mu.Unlock()

	snap, err := e.readLoaderSnapshot(deliveryNodes)
	if err != nil {
		// Fail closed — never fire into the dark when the order list is unavailable.
		return 0, fmt.Errorf("reserve loader=%s: in-flight count: %w", loaderID, err)
	}
	d := decideLoaderBudget(&snap, loaderID, pay, want, budget, deliveryNodes, retrieveEmpty)
	if !snap.reachable {
		d.logUnreachable(e, &snap)
		return 0, nil
	}
	if d.toFire <= 0 {
		d.logNoFire(e, &snap)
		return 0, nil
	}
	created, ferr := fire(d.targets)
	d.logFired(e, &snap, created, ferr)
	return created, ferr
}

// loaderSnapshot is what one reservation reads, under the loader's mutex: the
// active orders delivering to the read set and, when Core is configured, the
// occupancy of the same set. withLoaderBudget reads it for one payload's target
// nodes; the unloader pass (createUnloaderFullIns) reads it ONCE for the
// loader's whole DeliveryNodes set and decides every payload from it.
type loaderSnapshot struct {
	orders []storeorders.Order
	bins   []NodeBinInfo
	// reachable is false only when Core is configured and the occupancy read
	// failed. With no Core configured it is true and bins is empty.
	reachable bool
	occupancy string // OccupancyOutcome of the read, or "not_configured"
	readErr   error  // the occupancy read's error, for the decision line
}

// readLoaderSnapshot reads the orders first and fails on their error without
// asking Core; then the occupancy, if Core is configured.
//
// A bin already RESIDENT on a window occupies it just as an inbound order does.
// The order count sees inbound retrieve/full ORDERS only; a carrier physically
// standing on a window — an empty awaiting the operator's load, or a full not
// yet pulled — is invisible to it. Without the occupancy read the seam re-fires
// an empty onto an occupied window every time the previous order terminalises:
// the SLN_002 resident-bin blindness (live at Springfield 2026-07-23 — SMN_014
// held a 0-UOP empty, system UOP read 0 < threshold, and the monitor re-issued a
// retrieve_empty each cycle; the empty is already there for the loader operator
// to LOAD, another carrier does nothing).
//
// Occupancy is Core-authoritative (FetchNodeBins), which marks a window Occupied
// for ANY resident bin, including a 0-UOP empty. It is read inside the loader
// mutex so it is part of the same atomic count→fire snapshot as the orders.
//
// `occupancy` records what the read actually did. It is the field that was
// missing on 2026-07-31: the log said resident=0, which reads as "the windows
// are empty" and in fact meant "nobody answered".
//
// AND IF NOBODY ANSWERED, NOTHING FIRES (reachable=false; the callers refuse).
// The seam's whole job is to not put a second carrier on a window that already
// has one, and it cannot do that job without knowing what is on the windows.
// Firing anyway was never a decision — it was the old read being unable to say
// it had failed. Core re-signals within about a minute, so the cost of waiting
// is one cycle; the cost of guessing is a robot delivering an empty to an
// occupied window, which is what happened.
//
// A loader with no Core configured is the exception and keeps firing: there is
// nothing to be out of touch WITH, the deployment simply has no Core telemetry,
// and refusing there would take the seam permanently offline rather than pause
// it. That arm has its own characterization test.
func (e *Engine) readLoaderSnapshot(nodes []string) (loaderSnapshot, error) {
	orderList, err := e.db.ListActiveOrdersByDeliveryNodeSet(nodes)
	if err != nil {
		return loaderSnapshot{}, err
	}
	snap := loaderSnapshot{orders: orderList, reachable: true, occupancy: "not_configured"}
	if e.coreClient.Available() {
		bins, reachable, rerr := e.coreClient.FetchNodeBins(nodes)
		snap.bins, snap.reachable, snap.readErr = bins, reachable, rerr
		snap.occupancy = OccupancyOutcome(reachable, rerr)
	}
	return snap, nil
}

// loaderBudgetDecision is the count→cap→target step for one payload over a
// snapshot, and the one structured loader_budget line it writes.
type loaderBudgetDecision struct {
	loaderID, pay                            string
	want, budget                             int
	inFlightPayload, inFlightTotal, resident int
	toFire                                   int
	targets                                  []string
}

// decideLoaderBudget counts, over the payload's target nodes only, this
// direction's in-flight orders and — where no order already accounts for a
// window — its resident bins, then caps want to the free budget and picks the
// free windows in target order.
//
// Restricting to the target nodes matters when the snapshot spans more: a
// funnelled loader's target is its first window alone, so an order or bin on
// another window must not spend its budget
// (TestUnloaderSweep_FunnelCountsOnlyItsTargetWindow). A row that names no node
// is still charged, as it always was; Core names every row.
func decideLoaderBudget(snap *loaderSnapshot, loaderID, pay string, want, budget int, nodes []string, retrieveEmpty bool) loaderBudgetDecision {
	d := loaderBudgetDecision{loaderID: loaderID, pay: pay, want: want, budget: budget}
	inTarget := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		inTarget[n] = true
	}
	perNode := make(map[string]int, len(nodes))
	for _, o := range snap.orders {
		if o.RetrieveEmpty != retrieveEmpty || !inTarget[o.DeliveryNode] {
			continue // count only this direction's in-flight (empties for a loader, fulls for an unloader)
		}
		d.inFlightTotal++
		perNode[o.DeliveryNode]++
		if o.PayloadCode == pay {
			d.inFlightPayload++
		}
	}
	if !snap.reachable {
		return d
	}
	// Count a resident bin toward window occupancy only where no inbound order
	// already accounts for that window, so an order that just delivered its bin
	// isn't double-counted.
	for i := range snap.bins {
		nb := snap.bins[i]
		if nb.NodeName != "" && !inTarget[nb.NodeName] {
			continue
		}
		if nb.Occupied && perNode[nb.NodeName] == 0 {
			d.inFlightTotal++
			perNode[nb.NodeName] = 1
			d.resident++
		}
	}
	d.toFire = want - d.inFlightPayload
	if headroom := budget - d.inFlightTotal; d.toFire > headroom {
		d.toFire = headroom
	}
	if d.toFire <= 0 {
		d.toFire = 0
		return d
	}
	// Assign each new bin to a FREE window (none in flight) — one physical bin
	// per window. budget = window count and toFire ≤ headroom = the free-window
	// count, so there are always enough; a single-node set degrades to [that node].
	d.targets = make([]string, 0, d.toFire)
	for _, node := range nodes {
		if len(d.targets) >= d.toFire {
			break
		}
		if perNode[node] == 0 {
			d.targets = append(d.targets, node)
		}
	}
	return d
}

// The three loader_budget line shapes: the structured decision record, one
// machine-parseable line per reservation so an over-ordering incident is
// reconstructable from logs alone (the SLN_002 bar).

func (d *loaderBudgetDecision) logUnreachable(e *Engine, snap *loaderSnapshot) {
	e.logFn("loader_budget loader=%s payload=%q want=%d in_flight_payload=%d in_flight_total=%d resident=0 occupancy=%s budget=%d to_fire=0 created=0 err=%v",
		d.loaderID, d.pay, d.want, d.inFlightPayload, d.inFlightTotal, snap.occupancy, d.budget, snap.readErr)
}

func (d *loaderBudgetDecision) logNoFire(e *Engine, snap *loaderSnapshot) {
	e.logFn("loader_budget loader=%s payload=%q want=%d in_flight_payload=%d in_flight_total=%d resident=%d occupancy=%s budget=%d to_fire=0 created=0",
		d.loaderID, d.pay, d.want, d.inFlightPayload, d.inFlightTotal, d.resident, snap.occupancy, d.budget)
}

func (d *loaderBudgetDecision) logFired(e *Engine, snap *loaderSnapshot, created int, ferr error) {
	e.logFn("loader_budget loader=%s payload=%q want=%d in_flight_payload=%d in_flight_total=%d resident=%d occupancy=%s budget=%d to_fire=%d targets=%v created=%d err=%v",
		d.loaderID, d.pay, d.want, d.inFlightPayload, d.inFlightTotal, d.resident, snap.occupancy, d.budget, d.toFire, d.targets, created, ferr)
}

// multiWindowFor reports whether THIS loader spreads its empties across its
// windows (one bin per window, budget = window count) or takes one window at a
// time (budget 1, all to the first window).
//
// Core owns the answer, on the loader's own row. It used to be one plant-wide
// Edge config key, which could only answer for every loader at once; a plant
// that needed the funnel for one loader imposed it on all of them.
//
// THE CONFIG KEY IS DEPRECATED AND SURVIVES ONLY AS A PLANT-WIDE OFF SWITCH.
// An explicit `loaders_multi_window: false` still funnels every loader,
// because deleting the key outright would silently switch such a plant to
// spreading — a live behaviour change nobody asked for, delivered by a
// deployment that only changed a default. It cannot turn spreading ON against a
// loader that is configured to funnel: the loader is the authority, and the key
// is a brake, never an accelerator. Deploy 9 removes it once no config sets it.
func (e *Engine) multiWindowFor(l *domain.Loader) bool {
	if e.cfg != nil && e.cfg.LoadersMultiWindow != nil && !*e.cfg.LoadersMultiWindow {
		return false
	}
	return l == nil || !l.FunnelWindows()
}

// nodeIDStrings projects typed NodeIDs to the plain strings the order-query layer
// keys on (the boundary where typed IDs meet the legacy string columns).
func nodeIDStrings(ns []domain.NodeID) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = string(n)
	}
	return out
}

// resolveOwnWindow is the window-resolve step both fire closures (L1 and U1)
// share: it maps a delivery window to this Edge's process_node id.
//
// One plant, several Edges: Core broadcasts the loader config to every Edge,
// and each Edge holds process_node rows only for its own windows. A sweep on
// this Edge routinely walks windows served by another Edge, and sql.ErrNoRows
// is the store's ordinary "we do not own this destination" answer — so that
// arm logs skipFmt at debug and returns ok=false with no error, and the caller
// skips the window rather than failing the sweep. Any other read error comes
// back wrapped in errFmt, and the caller aborts its fire loop on it
// (TestStageOperatorEmpty_ResolveReadErrorAbortsLoop).
//
// skipFmt takes the window (%s); errFmt takes the window (%s) and the read
// error (%w). Each caller keeps its own wording.
func (e *Engine) resolveOwnWindow(deliveryNode, skipFmt, errFmt string) (nodeID int64, ok bool, err error) {
	node, nerr := e.db.GetProcessNodeByCoreNodeName(deliveryNode)
	if errors.Is(nerr, sql.ErrNoRows) {
		e.debugFn(skipFmt, deliveryNode)
		return 0, false, nil
	}
	if nerr != nil || node == nil {
		return 0, false, fmt.Errorf(errFmt, deliveryNode, nerr)
	}
	return node.ID, true, nil
}

// stageOperatorEmpty creates loader empties opportunistically when a window
// frees up on an operator-driven loader. THIS PATH STAYS on the Edge: it is
// driven by what the operator physically did, which Core does not observe.
//
// It takes the resolved *domain.Loader (the Loader is the unit of resolution);
// the count→fire atomicity, the per-payload dedup, the capacity cap, and the
// decision record all live in withLoaderBudget. count is the desired total
// in-flight for the payload. No operator-driven gate applies: this IS the
// operator-driven supply path.
//
// origin attributes the L1s this call creates and comes from the CALLER, not a
// lookup here. The opportunistic push serves no demand episode and passes
// orders.NoDemand().
func (e *Engine) stageOperatorEmpty(loader *domain.Loader, payload domain.PayloadCode, count int, member domain.NodeID, origin orders.Origin) (int, error) {
	if loader == nil {
		return 0, nil
	}
	coreNode := string(loader.ID())
	if loader.InboundSource() == "" {
		// No inbound source to pull empties from — a forklift/press-fed loader is
		// supplied directly (operator stages empties at the window). Skip auto-L1; nothing
		// to queue. Symmetric to the unloader's no-inbound gate in createUnloaderFullIns.
		e.debugFn("loader_push: loader=%s payload=%s skipped — no inbound source (fed directly)",
			coreNode, payload)
		return 0, nil
	}
	created, err := e.withLoaderBudget(loader, payload, count, member, true, func(deliveryNodes []string) (int, error) {
		made := 0
		for i, deliveryNode := range deliveryNodes {
			nodeID, ok, rerr := e.resolveOwnWindow(deliveryNode,
				"loader_push: window %s has no process_node here — another Edge's window; skipped",
				"loader_push: no process_node for delivery target %s: %w")
			if rerr != nil {
				return made, rerr
			}
			if !ok {
				continue // another Edge's window
			}
			order, cerr := e.orderMgr.CreateRetrieveOrder(
				&nodeID, true, 1, deliveryNode, loader.InboundSource(), "",
				"standard", string(payload), false, true, origin,
			)
			if cerr != nil {
				return made, fmt.Errorf("loader_push: create L1 %d/%d loader=%s payload=%s: %w",
					i+1, len(deliveryNodes), coreNode, payload, cerr)
			}
			made++
			// Burst tripwire stays DELIVERY-NODE-keyed (orthogonal to loader identity):
			// one empty per physical window, so a flood at a single node trips it even
			// when the loader identity is now an opaque token.
			e.recordL1Burst(deliveryNode, 1)
			e.debugFn("loader_push: L1 order %d (%d/%d) loader=%s payload=%s window=%s",
				order.ID, i+1, len(deliveryNodes), coreNode, payload, deliveryNode)
		}
		return made, nil
	})
	if err != nil {
		e.logFn("loader_push: loader=%s payload=%s reservation failed after %d created: %v",
			coreNode, payload, created, err)
		return created, err
	}
	return created, nil
}

// maybeStageLoaderEmpty stages one empty at an operator-driven loader if none is
// already in flight. The empty is a generic carrier staged payload-AGNOSTIC
// (blank code) rather than tagged with an arbitrary "representative" payload —
// there is no payload-specific demand behind an opportunistic stage, so naming
// one just fabricates a binding the operator routinely overrides at LoadBin.
// One-at-a-time keeps it opportunistic; stageOperatorEmpty applies no
// operator-driven suppression (it IS the operator-driven supply path).
//
// Single-carrier assumption — see RequestEmptyBin: a blank order sources any
// compatible empty, which is correct only when the loader uses one carrier type.
func (e *Engine) maybeStageLoaderEmpty(loader *domain.Loader) {
	if loader == nil {
		return
	}
	// Misconfig guard: a loader with nothing to stage against (no shared payloads
	// and no positions) isn't set up to load anything, so there's nothing to stage
	// for — even agnostically.
	if len(loader.PayloadSet()) == 0 && len(loader.Positions()) == 0 {
		return // misconfigured loader — nothing to stage against
	}
	// No separate advisory in-flight pre-check: the reservation seam owns the
	// never-2N dedup atomically across the loader's delivery nodes, so a push for a
	// loader that already has an empty in flight resolves to to_fire=0 and fires
	// nothing. The empty is staged payload-AGNOSTIC (blank code) — the operator
	// picks the payload at LoadBin.
	if _, err := e.stageOperatorEmpty(loader, "", 1, "", orders.NoDemand()); err != nil { // opportunistic push: payload-agnostic, no member
		e.logFn("loader-push: stage empty at loader=%s failed: %v", loader.ID(), err)
	}
}

// SweepPushLoaders walks every active operator-driven produce manual_swap loader
// and stages an empty if its window is free. Intended for Edge startup (after
// registration ack, mirroring SweepPushUnloaders): catches loaders that were
// empty when Edge went down so the operator returns to a staged empty rather
// than an empty window.
func (e *Engine) SweepPushLoaders() {
	if !e.sweepingLoaders.CompareAndSwap(false, true) {
		return // a sweep is already running — a re-register storm must not stack them
	}
	defer e.sweepingLoaders.Store(false)
	loaders, err := e.loaders().Loaders(domain.RoleProduce)
	if err != nil {
		e.logFn("loader-push: startup sweep list produce loaders: %v", err)
		return
	}
	swept := 0
	for _, l := range loaders {
		// CHECKED BEFORE THE GATE, NOT INSIDE IT. A threshold loader with no
		// threshold is not operator-staged any more, so it is skipped below — and
		// if this warning sat after the skip, the one configuration that feeds a
		// loader from nothing would be the one nothing reports.
		// Two severities, and the second is the one that hides. NO threshold at
		// all means nothing feeds the loader — loud. SOME payloads without one
		// means the loader works and those parts are ordered by nobody, which
		// passes every check that only asks whether a threshold exists.
		if missing := l.PayloadsMissingThreshold(); len(missing) > 0 {
			if l.MisconfiguredThreshold() {
				log.Printf("WARN loader-push: loader=%s is switched to threshold replenishment but has NO threshold configured — "+
					"nothing will order carriers for it. Either set a UOP threshold, or switch it to operator so the window-free push feeds it.", l.ID())
			} else {
				log.Printf("WARN loader-push: loader=%s is on threshold replenishment and %d of its payloads have no threshold (%v) — "+
					"those parts are ordered by nobody. Set a UOP threshold for each on the inventory page.",
					l.ID(), len(missing), missing)
			}
		}
		if !l.UsesOperatorStaging() {
			continue
		}
		e.maybeStageLoaderEmpty(l)
		swept++
	}
	if swept > 0 {
		log.Printf("loader-push: startup sweep covered %d operator-staged loader(s)", swept)
	}
}
