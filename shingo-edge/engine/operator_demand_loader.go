package engine

import (
	"log"

	"shingoedge/domain"
	"shingoedge/orders"
)

// A produce loader's automatic replenishment is now decided entirely on Core.
// The Edge used to receive a below-threshold signal and work out how many
// carriers were needed and where they went; that half is gone, and Core creates
// the orders itself. What is left here is the OPERATOR side: the opportunistic
// window-free push (rePushOwnLoader, SweepPushLoaders) and the operator's own
// REQUEST EMPTY, both asked of Core (loader.retrieve, askLoaderEmpties), which
// counts and orders them. Every carrier a loader or unloader ASKS for is
// Core's to count and order now (loader_retrieve_ask.go): the Edge's per-loader
// budget (withLoaderBudget, its lock and its snapshot) was the last of the
// Edge's own count, and it went when the unloader's U1, a person's full and the
// manual retrieve at a loader window started to ask. An order a claim sends
// straight to a window (a changeover's retrieve, a cell's move) is still the
// Edge's, and Core holds the window against the asks once it is there
// (plan.LoaderWindows: TestLoaderWindows_AClaimDrivenDeliveryIsSeenByItsArm).
//
// Two earlier retirements, kept as gravestones because their names still appear
// in incident records: the bin-count produce DemandSignal trigger
// (MaybeCreateLoaderEmptyIn + findLoaderForDemand + refillLoaderForPayload), and
// the threshold receiver that replaced it (HandleLoopBelowThreshold + its
// park/replay machinery). A third followed (2026-08): the DemandSignal wire
// subject itself was deleted — Core no longer emits it. The unloader's U1
// full-in now comes from the unloader's own auto_push gates and the startup
// sweep (offerUnloaderFulls, asked of Core), and from MaybeCreateUnloaderFullIn on a
// produce release carrying capture_lineside (the release commit,
// release_commit.go), which a station's HMI never posts on a produce node.
//
// A fourth retirement: the L1Source type (L1SideCycle, L1LoopThreshold,
// L1LoaderPush) and its operator-driven suppression policy. It existed to force
// a class decision whenever a second L1 source was added; with the threshold
// source moved to Core only the operator push is left, so the log prefix
// `loader_push:` is now a literal in stageOperatorEmpty.

// nodeIDStrings projects typed NodeIDs to the plain strings the order-query layer
// keys on (the boundary where typed IDs meet the legacy string columns).
func nodeIDStrings(ns []domain.NodeID) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = string(n)
	}
	return out
}

// stageOperatorEmpty asks Core for loader empties when a window frees up on an
// operator-driven loader: the push is driven by what the operator physically
// did, which Core does not observe, so the Edge asks; Core counts what is in
// flight, which windows are spoken for and what stands on them, and orders the
// rest (askLoaderEmpties). Returns how many Core made.
//
// count is the desired total in flight for the payload, as Core reads it: the
// asking episode's own outstanding orders are subtracted there. origin
// attributes what Core makes and comes from the CALLER; the opportunistic push
// serves no demand episode and passes orders.NoDemand().
func (e *Engine) stageOperatorEmpty(loader *domain.Loader, payload domain.PayloadCode, count int, member domain.NodeID, origin orders.Origin) (int, error) {
	if loader == nil || count <= 0 {
		return 0, nil
	}
	coreNode := string(loader.ID())
	if loader.InboundSource() == "" {
		// No inbound source to pull empties from — a forklift/press-fed loader is
		// supplied directly (operator stages empties at the window). Nothing to
		// ask for. Symmetric to the unloader's no-inbound gate in offerUnloaderFulls.
		e.debugFn("loader_push: loader=%s payload=%s skipped — no inbound source (fed directly)",
			coreNode, payload)
		return 0, nil
	}
	res, err := e.askLoaderEmpties(loader, payload, count, member, origin)
	if err != nil {
		e.logFn("loader_push: loader=%s payload=%s ask: %v", coreNode, payload, err)
		return 0, err
	}
	e.logFn("loader_push: loader=%s payload=%q want=%d created=%d held=%v skipped=%q",
		coreNode, payload, res.Want, len(res.Created), res.HeldBy, res.Skipped)
	return len(res.Created), nil
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
	// No separate advisory in-flight pre-check: Core counts what is in flight and
	// what stands on the windows, so a push for a loader that already has an empty
	// coming makes none (HeldBy names the window). The empty is staged
	// payload-AGNOSTIC (blank code) — the operator picks the payload at LoadBin.
	if _, err := e.stageOperatorEmpty(loader, "", 1, "", orders.NoDemand()); err != nil { // opportunistic push: payload-agnostic, no member
		e.logFn("loader-push: stage empty at loader=%s failed: %v", loader.ID(), err)
	}
}

// SweepPushLoaders walks every active operator-driven produce manual_swap loader,
// a stage 2 excepted (TakesBare: stage 1 brings its carts), and stages an
// empty if its window is free. Intended for Edge startup (after
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
		// A threshold loader is Core's to feed. Whether its thresholds are set
		// is Core's to say too (its loader and inventory pages: "no threshold
		// set"); the Edge holds no copy of the threshold and warns about none.
		if !l.UsesOperatorStaging() || l.TakesBare() {
			continue
		}
		e.maybeStageLoaderEmpty(l)
		swept++
	}
	if swept > 0 {
		log.Printf("loader-push: startup sweep covered %d operator-staged loader(s)", swept)
	}
}
