package engine

import (
	"fmt"
	"log"
	"sync"

	"shingoedge/domain"
	"shingoedge/orders"
)

// manualSwapWindowSlots is how many bins a single manual_swap core node can
// physically stage at its window — one (one physical slot per window/position).
//
// The LOADER empty path no longer reads this constant: withLoaderBudget
// derives the budget from the delivery-node SET cardinality (one bin per node),
// so a multi-window loader's budget grows to N when delivery spreads
// without a magic number, and the per-payload dedup + capacity cap are unified in
// the seam. The constant remains for the UNLOADER full-in cap
// (operator_demand_unloader.go), which has not yet moved to a reservation seam,
// and still documents the one-bin-per-node physical model the operator-path
// anti-spam guard also encodes (operator_bin_ops.go).
const manualSwapWindowSlots = 1

// A produce loader's automatic replenishment is now decided entirely on Core.
// The Edge used to receive a below-threshold signal and work out how many
// carriers were needed and where they went; that half is gone, and Core creates
// the orders itself. What is left here is the OPERATOR side: the opportunistic
// window-free push (MaybePushLoader), the operator's own requests, and the
// unloader path — all still through the withLoaderBudget seam.
//
// Two earlier retirements, kept as gravestones because their names still appear
// in incident records: the bin-count produce DemandSignal trigger
// (MaybeCreateLoaderEmptyIn + findLoaderForDemand + refillLoaderForPayload), and
// the threshold receiver that replaced it (HandleLoopBelowThreshold + its
// park/replay machinery). A third followed (2026-08): the DemandSignal wire
// subject itself was deleted — Core no longer emits it, so the unloader's U1
// full-in now fires from operator release alone (operator_release.go).

// L1Source identifies which path is creating a loader empty-in (L1)
// retrieve_empty order. Two sources are retired: the legacy bin-count one
// (L1SideCycle) and the UOP-threshold C-push (L1LoopThreshold), whose decision
// moved to Core. One source is left on the Edge — the operator-driven
// opportunistic push. It also carries the operator-driven-suppression policy,
// so adding a source forces a decision about its class rather than defaulting
// silently.
type L1Source string

const (
	L1LoaderPush L1Source = "loader_push" // operator-driven opportunistic empty staging
)

// logTag is the stable, greppable prefix this source uses in log lines.
func (s L1Source) logTag() string { return string(s) }

// suppressedByOperatorDriven reports whether an operator-driven loader silences
// this source. Allowlist semantics: only the automatic market-accounting source
// opts in — an operator-driven loader is fed by the operator,
// not the threshold monitor. L1LoaderPush (the operator-driven supply path itself)
// falls through to false, so it is NOT suppressed.
func (s L1Source) suppressedByOperatorDriven() bool {
	return false
}

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
// maybeStageLoaderEmpty/MaybePushLoader), RequestEmptyBin's manual_swap branch
// and RequestFullBin (operator), createUnloaderFullInViaSeam (automatic U1),
// and CreateRetrieveForAPI (the HTTP order API — added by Deploy 1b; the
// sentence below that still names it as a bypass is corrected with it).
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
// consume side shares it rather than re-implementing the count and cap — the
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

	orderList, err := e.db.ListActiveOrdersByDeliveryNodeSet(deliveryNodes)
	if err != nil {
		// Fail closed — never fire into the dark when the order list is unavailable.
		return 0, fmt.Errorf("reserve loader=%s: in-flight count: %w", loaderID, err)
	}
	inFlightPayload, inFlightTotal := 0, 0
	perNode := make(map[string]int, len(deliveryNodes))
	for _, o := range orderList {
		if o.RetrieveEmpty != retrieveEmpty {
			continue // count only this direction's in-flight (empties for a loader, fulls for an unloader)
		}
		inFlightTotal++
		perNode[o.DeliveryNode]++
		if o.PayloadCode == pay {
			inFlightPayload++
		}
	}
	// A bin already RESIDENT on a window occupies it just as an inbound order does.
	// The order count above sees inbound retrieve/full ORDERS only; a carrier
	// physically standing on a window — an empty awaiting the operator's load, or a
	// full not yet pulled — is invisible to it. Without this the seam re-fires an
	// empty onto an occupied window every time the previous order terminalises: the
	// SLN_002 resident-bin blindness (live at Springfield 2026-07-23 — SMN_014 held a
	// 0-UOP empty, system UOP read 0 < threshold, and the monitor re-issued a
	// retrieve_empty each cycle; the empty is already there for the loader operator to
	// LOAD, another carrier does nothing). Count a resident bin toward window
	// occupancy only where no inbound order already accounts for that window, so an
	// order that just delivered its bin isn't double-counted.
	//
	// Occupancy is Core-authoritative (FetchNodeBins), which marks a window Occupied
	// for ANY resident bin, including a 0-UOP empty. Kept inside the loader mutex so
	// the occupancy read is part of the same atomic count→fire snapshot as the orders.
	//
	// `occupancy` records what the read actually did. It is the field that was
	// missing on 2026-07-31: the log said resident=0, which reads as "the windows
	// are empty" and in fact meant "nobody answered".
	//
	// AND IF NOBODY ANSWERED, NOTHING FIRES. This seam's whole job is to not put a
	// second carrier on a window that already has one, and it cannot do that job
	// without knowing what is on the windows. Firing anyway was never a decision —
	// it was the old read being unable to say it had failed. Core re-signals within
	// about a minute, so the cost of waiting is one cycle; the cost of guessing is a
	// robot delivering an empty to an occupied window, which is what happened.
	//
	// A loader with no Core configured is the exception and keeps firing: there is
	// nothing to be out of touch WITH, the deployment simply has no Core telemetry,
	// and refusing there would take the seam permanently offline rather than pause
	// it. That arm has its own characterization test.
	resident := 0
	occupancy := "not_configured"
	if e.coreClient.Available() {
		residentBins, reachable, rerr := e.coreClient.FetchNodeBins(deliveryNodes)
		occupancy = OccupancyOutcome(reachable, rerr)
		if !reachable {
			e.logFn("loader_budget loader=%s payload=%q want=%d in_flight_payload=%d in_flight_total=%d resident=0 occupancy=%s budget=%d to_fire=0 created=0 err=%v",
				loaderID, pay, want, inFlightPayload, inFlightTotal, occupancy, budget, rerr)
			return 0, nil
		}
		for i := range residentBins {
			nb := residentBins[i]
			if nb.Occupied && perNode[nb.NodeName] == 0 {
				inFlightTotal++
				perNode[nb.NodeName] = 1
				resident++
			}
		}
	}
	toFire := want - inFlightPayload
	if headroom := budget - inFlightTotal; toFire > headroom {
		toFire = headroom
	}
	if toFire <= 0 {
		e.logFn("loader_budget loader=%s payload=%q want=%d in_flight_payload=%d in_flight_total=%d resident=%d occupancy=%s budget=%d to_fire=0 created=0",
			loaderID, pay, want, inFlightPayload, inFlightTotal, resident, occupancy, budget)
		return 0, nil
	}
	// Assign each new empty to a FREE window (none in flight) — one physical bin
	// per window. budget = window count and toFire ≤ headroom = the free-window
	// count, so there are always enough; a single-node set degrades to [that node].
	targets := make([]string, 0, toFire)
	for _, node := range deliveryNodes {
		if len(targets) >= toFire {
			break
		}
		if perNode[node] == 0 {
			targets = append(targets, node)
		}
	}
	created, ferr := fire(targets)
	// Structured decision record — one machine-parseable line per reservation so an
	// over-ordering incident is reconstructable from logs alone (the SLN_002 bar).
	e.logFn("loader_budget loader=%s payload=%q want=%d in_flight_payload=%d in_flight_total=%d resident=%d occupancy=%s budget=%d to_fire=%d targets=%v created=%d err=%v",
		loaderID, pay, want, inFlightPayload, inFlightTotal, resident, occupancy, budget, toFire, targets, created, ferr)
	return created, ferr
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

// stageOperatorEmpty creates loader empties opportunistically when a window
// frees up on an operator-driven loader. THIS PATH STAYS on the Edge: it is
// driven by what the operator physically did, which Core does not observe.
func (e *Engine) stageOperatorEmpty(loader *domain.Loader, payload domain.PayloadCode, count int, member domain.NodeID, origin orders.Origin) (int, error) {
	return e.createLoaderEmpties(loader, payload, L1LoaderPush, count, member, origin)
}

// createLoaderEmpties is the shared body behind both entry points. It takes the
// resolved *domain.Loader (the Loader is the unit of resolution). The
// operator-driven gate is applied here; the count→fire atomicity, the per-payload
// dedup, the capacity cap, and the decision record all live in withLoaderBudget.
// count is the desired total in-flight for the payload.
//
// origin attributes the L1s this call creates and comes from the CALLER, not a
// lookup here: the threshold path serves a Core demand episode and the
// opportunistic push serves nothing at all, and only the caller knows which.
func (e *Engine) createLoaderEmpties(loader *domain.Loader, payload domain.PayloadCode, source L1Source, count int, member domain.NodeID, origin orders.Origin) (int, error) {
	if loader == nil {
		return 0, nil
	}
	coreNode := string(loader.ID())
	// IsOperatorDriven reads the aggregate directly — correct after loader.ID() became
	// the loader_key token (a cache lookup keyed on the old core_node_name would now
	// miss). The push source is exempt regardless (it IS the operator-driven supply path).
	if loader.IsOperatorDriven() && source.suppressedByOperatorDriven() {
		e.debugFn("%s: loader=%s payload=%s skipped — operator-driven",
			source.logTag(), coreNode, payload)
		return 0, nil
	}
	if loader.InboundSource() == "" {
		// No inbound source to pull empties from — a forklift/press-fed loader is
		// supplied directly (operator stages empties at the window). Skip auto-L1; nothing
		// to queue. Symmetric to the unloader's no-inbound gate in createUnloaderFullInViaSeam.
		e.debugFn("%s: loader=%s payload=%s skipped — no inbound source (fed directly)",
			source.logTag(), coreNode, payload)
		return 0, nil
	}
	created, err := e.withLoaderBudget(loader, payload, count, member, true, func(deliveryNodes []string) (int, error) {
		made := 0
		for i, deliveryNode := range deliveryNodes {
			node, nerr := e.db.GetProcessNodeByCoreNodeName(deliveryNode)
			if nerr != nil || node == nil {
				return made, fmt.Errorf("%s: no process_node for delivery target %s: %w", source.logTag(), deliveryNode, nerr)
			}
			nodeID := node.ID
			order, cerr := e.orderMgr.CreateRetrieveOrder(
				&nodeID, true, 1, deliveryNode, loader.InboundSource(), "",
				"standard", string(payload), false, true, origin,
			)
			if cerr != nil {
				return made, fmt.Errorf("%s: create L1 %d/%d loader=%s payload=%s: %w",
					source.logTag(), i+1, len(deliveryNodes), coreNode, payload, cerr)
			}
			made++
			// Burst tripwire stays DELIVERY-NODE-keyed (orthogonal to loader identity):
			// one empty per physical window, so a flood at a single node trips it even
			// when the loader identity is now an opaque token.
			e.recordL1Burst(deliveryNode, 1)
			e.debugFn("%s: L1 order %d (%d/%d) loader=%s payload=%s window=%s",
				source.logTag(), order.ID, i+1, len(deliveryNodes), coreNode, payload, deliveryNode)
		}
		return made, nil
	})
	if err != nil {
		e.logFn("%s: loader=%s payload=%s reservation failed after %d created: %v",
			source.logTag(), coreNode, payload, created, err)
		return created, err
	}
	return created, nil
}

// MaybePushLoader is the loader-side mirror of MaybePushUnloader: the
// opportunistic empty-staging push for OPERATOR-DRIVEN loaders. When an
// operator-driven loader's window is free it stages one empty so the operator
// always has a bin to fill. Threshold loaders are no-ops here — their
// empties come from the threshold path (which knows the payload and
// count). Opportunistic, one at a time: maybeStageLoaderEmpty fires only when
// no empty is already in flight, and Core's CheckDropoffCapacity queues the
// order if the window is still physically occupied, so it can't slam.
//
// Trigger sites mirror the unloader:
//   - applyManualSwap (L2 arrived at the market — window confirmed free).
//   - ClearBin (operator cleared the window).
//   - SweepPushLoaders on Edge startup / registration ack.
//
// The nodeID arg is now vestigial — the reservation seam's never-2N budget makes
// the sweep idempotent (already-staged loaders create nothing), so there is no need
// to filter to a specific loader. Mirrors MaybePushUnloader(_ int64).
func (e *Engine) MaybePushLoader(_ int64) {
	loaders, err := e.loaders().Loaders(domain.RoleProduce)
	if err != nil {
		e.logFn("loader-push: list produce loaders: %v", err)
		return
	}
	for _, l := range loaders {
		// Operator-driven loaders only. A threshold loader is Core's to feed, even
		// if it has no threshold configured — in which case it is fed by nothing,
		// and SweepPushLoaders is where that gets said out loud.
		if !l.UsesOperatorStaging() {
			continue
		}
		e.maybeStageLoaderEmpty(l)
	}
}

// maybeStageLoaderEmpty stages one empty at an operator-driven loader if none is
// already in flight. The empty is a generic carrier staged payload-AGNOSTIC
// (blank code) rather than tagged with an arbitrary "representative" payload —
// there is no payload-specific demand behind an opportunistic stage, so naming
// one just fabricates a binding the operator routinely overrides at LoadBin.
// One-at-a-time keeps it opportunistic; L1LoaderPush is exempt from the
// operator-driven suppression in createLoaderEmpties (it IS the operator-driven
// supply path).
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
	// picks the payload at LoadBin; L1LoaderPush is exempt from the operator-driven
	// suppression in createLoaderEmpties (it IS the operator-driven supply path).
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
