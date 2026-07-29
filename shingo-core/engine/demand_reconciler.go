package engine

import (
	"time"

	"shingo/protocol"
	"shingocore/store"
)

// demand_reconciler.go — the correctness floor under Core's close paths.
//
// DON'T RELY SOLELY ON BEING TOLD — ALSO BE ABLE TO NOTICE. Every site that
// closes a demand episode today is a NOTIFICATION path: something happens, so
// something fires. That works right up until nothing fires, and on Core there
// is a site where nothing ever will. SyncRegistry replaces a station's whole
// demand_registry in one transaction and reports only the rows whose threshold
// VALUE moved, so a binding that vanishes and comes back unchanged emits no
// RegistryChange at all; and three of its call sites throw the change list away
// regardless, including the stale-edge reaper, which deletes every binding a
// station has. You cannot add a hook to a thing that does not happen.
//
// So this sweep closes any open episode whose PRECONDITION no longer holds,
// regardless of how it stopped holding. The notification sites keep their job
// and become latency optimisations — they close promptly. This is the
// guarantee: it closes eventually. A close path nobody thought of degrades to
// "closed one sweep late" instead of "stranded forever", and an episode that
// never closes is the loudest row on the demand surface — a permanent alarm for
// a demand that ended, which is worse than no surface at all.
//
// IT CANNOT BE ONE SWEEP ACROSS BOTH SERVICES, and that is why this is the Core
// half. Preconditions are owned by whoever holds the state behind them: a cell
// episode's level lives in Edge's claims and a changeover's terminal state in
// Edge's process_changeovers, neither of which Core has. Edge runs its own
// sweep for those. Core owns the threshold bindings, and it owns the two
// OWNERLESS cases — orphan aging and childless auto-close — which need order
// counts that exist only here.
//
// THE FAILURE MODE THIS FILE MUST NOT HAVE is a sweep whose query is subtly
// wrong, finds nothing, and reports green forever, which is indistinguishable
// from a healthy plant. Two things guard against it. Every pass counts and logs
// what it did, so "the sweep closed nothing this month" is a readable fact
// rather than an absence. And both directions are pinned by tests: that it
// closes what a broken notification path left behind, and that it does NOT
// close an episode whose precondition still holds — because a sweep that closes
// everything passes the first test too.

// startDemandReconciler launches the sweep loop. A non-positive interval
// disables it: that is the pre-sweep behaviour, deliberately reachable so a
// plant can take the floor away while bisecting a problem without a rebuild.
func (e *Engine) startDemandReconciler() {
	if e.demandReconcileInterval() <= 0 {
		e.logFn("demand_reconciler: DISABLED (demand.reconcile_interval <= 0) — episodes close only via the notification paths")
		return
	}
	go e.runDemandReconciler()
}

func (e *Engine) runDemandReconciler() {
	ticker := time.NewTicker(e.demandReconcileInterval())
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			e.reconcileDemandEpisodes()
		}
	}
}

func (e *Engine) demandReconcileInterval() time.Duration {
	if e.cfg == nil {
		return 0
	}
	return e.cfg.Demand.ReconcileInterval
}

// reconcileDemandEpisodes is one pass. Split from the loop so tests drive it
// directly rather than waiting on a ticker.
//
// ORDER MATTERS BETWEEN THE FIRST TWO PASSES. A threshold episode whose binding
// vanished is usually also childless, and both passes would close it — but
// `threshold_removed` says what happened and `unattributed` says only that we
// never heard anything. Running the binding pass first means the truer reason
// wins whenever it is available.
func (e *Engine) reconcileDemandEpisodes() {
	byBinding := 0
	if e.thresholdMonitor != nil {
		byBinding = e.thresholdMonitor.reconcileThresholdBindings()
	}
	childless, unreachable := e.reconcileChildlessEpisodes()
	orphans := e.reconcileOrphanOrders()

	if byBinding == 0 && childless == 0 && orphans == 0 {
		// Nothing to say, every minute, forever. The counts still exist — the
		// per-item lines above carry them when they are non-zero — and burning
		// a plant's journal on "did nothing" is how the lines that matter stop
		// being read.
		e.dbg("demand_reconciler: pass clean (%d open episode(s) on an unreachable Edge)", unreachable)
		return
	}
	e.logFn("demand_reconciler: closed %d threshold episode(s) whose binding is gone, %d childless episode(s) as %s, aged out %d orphan order(s); %d open episode(s) left alone on an unreachable Edge",
		byBinding, childless, protocol.CloseReasonUnattributed, orphans, unreachable)
}

// reconcileChildlessEpisodes closes open episodes that have never produced a
// single order, and reports how many it deliberately left alone because their
// Edge is unreachable.
//
// WHY THIS IS NOT OPTIONAL. Ship a new Core against an older Edge and every
// order comes back with no origin on it, so every episode Core opens has zero
// children — and by the design's own display rules a long-open episode is the
// loudest row on the page, which turns a deploy artifact into a plant-wide
// emergency. It is reachable at full parity too: Edge silently drops threshold
// signals it cannot resolve, and that demand produces nothing either.
//
// A CHECK MUST KNOW WHETHER IT HAD THE INPUT TO CHECK. Zero children is
// evidence only if Core could have received children. When the owning Edge is
// stale — or was never registered at all — it is not evidence, it is a missing
// input, and rendering a missing input as a finding is the exact failure this
// branch has now paid for several times over. So those episodes are DECORATED,
// NEVER CLOSED: "this episode's Edge has been unreachable since X" is an honest
// unknown. It is a log line today because the demand surface does not exist
// yet; the query behind it is the one that surface will render.
//
// The decoration is not stored on the row. It is derivable from
// edge_registry.last_heartbeat at read time, and this design's standing rule is
// that nothing computed is ever stored — a stored rollup starts drifting from
// what it summarises, which is the uopCache lesson.
func (e *Engine) reconcileChildlessEpisodes() (closed int, unreachable int) {
	states, err := e.db.ListOpenEpisodeStates()
	if err != nil {
		// Not evidence that anything is stale. Leaving every episode open is
		// the honest outcome; the next pass asks again.
		e.logFn("demand_reconciler: list open episodes: %v", err)
		return 0, 0
	}
	grace := e.cfg.Demand.ChildlessGrace
	if grace <= 0 {
		// A zero grace would close every episode the instant it opened, before
		// its first order could possibly have been created — so an unset value
		// must not be read as "no waiting required".
		grace = 15 * time.Minute
	}
	cutoff := time.Now().UTC().Add(-grace)

	for i := range states {
		s := &states[i]
		if !s.EdgeReachable {
			unreachable++
			e.dbg("demand_reconciler: episode %s (%s, station=%s) left open — %s",
				s.OriginID, s.Kind, s.StationID, describeEdgeSilence(s))
			continue
		}
		if s.Children > 0 {
			continue
		}
		if !s.OpenedAt.Before(cutoff) {
			continue
		}
		// UNATTRIBUTED, not `recovered` and not `cancelled`. The demand did not
		// get its material and nothing said it was called off; what actually
		// happened is that no order was ever attributed to it, and the word has
		// to keep saying that or the surface loses the only signal that
		// distinguishes a dead-lettered close from an Edge that never spoke.
		ok, err := e.db.CloseDemandOriginInferred(s.OriginID, protocol.CloseReasonUnattributed,
			protocol.ClosedBySweep, time.Now().UTC())
		if err != nil {
			e.logFn("demand_reconciler: close childless episode %s: %v", s.OriginID, err)
			continue
		}
		if !ok {
			continue
		}
		closed++
		e.logFn("demand_reconciler: CLOSED %s origin=%s key=%s kind=%s — open %s with zero orders against it",
			protocol.CloseReasonUnattributed, s.OriginID, s.EpisodeKey, s.Kind,
			time.Since(s.OpenedAt).Round(time.Second))
	}
	return closed, unreachable
}

// describeEdgeSilence renders the honest unknown for an episode Core cannot
// judge, naming WHICH unknown it is.
//
// "Never sent a heartbeat" and "stopped sending one at 09:14" are different
// facts — one is a station that has never been up since Core booted, the other
// is a station that went away mid-episode — and collapsing them into "edge
// unreachable" throws away the half of the message that tells an operator where
// to look.
func describeEdgeSilence(s *store.OpenEpisodeState) string {
	if s.EdgeLastSeen == nil {
		return "its Edge (" + s.StationID + ") has never sent a heartbeat, so a zero order count proves nothing"
	}
	return "its Edge (" + s.StationID + ") has been unreachable since " +
		s.EdgeLastSeen.UTC().Format(time.RFC3339) + ", so a zero order count proves nothing"
}

// reconcileOrphanOrders retires orphan findings older than the grace period.
//
// An orphan is an order that should have carried an origin and didn't, and it
// is the ONLY origin_class that is a finding. There is no deferred attach — an
// orphan that later matches an open episode stays orphaned and reconciles by a
// human — so the finding set only ever grows, and an alarm that never clears is
// indistinguishable from a broken one.
//
// It lives in this sweep rather than in a timer of its own because it is the
// same act: ending something that has stopped being live. A second timer would
// be a second cadence, a second config knob and a second place to look when the
// counts disagree.
func (e *Engine) reconcileOrphanOrders() int {
	grace := e.cfg.Demand.OrphanGrace
	if grace <= 0 {
		grace = 24 * time.Hour
	}
	n, err := e.db.AgeOutOrphanOrders(time.Now().UTC().Add(-grace))
	if err != nil {
		e.logFn("demand_reconciler: age out orphan orders: %v", err)
		return 0
	}
	if n > 0 {
		e.logFn("demand_reconciler: aged out %d orphan order(s) older than %s — they stay recorded as orphans, they just stop being asked about",
			n, grace)
	}
	return int(n)
}
