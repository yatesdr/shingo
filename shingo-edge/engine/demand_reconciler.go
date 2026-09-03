package engine

import (
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// demand_reconciler.go — the correctness floor under Edge's close paths.
//
// DON'T RELY SOLELY ON BEING TOLD — ALSO BE ABLE TO NOTICE. Every site that
// closes an episode today is a NOTIFICATION path: something happened, so
// something fires. That works right up until nothing fires. A claim can be
// deleted, a style swapped, a changeover row cancelled by a path that never
// learned episodes exist — and an absence cannot be wired up. There is no hook
// to add to a thing that does not happen.
//
// So this sweep closes any open episode whose PRECONDITION no longer holds,
// regardless of how it stopped holding. The notification sites keep their job
// and become latency optimisations — they close promptly. This is the
// guarantee: it closes eventually. A close path nobody thought of degrades to
// "closed one sweep late" instead of "stranded forever", and an episode that
// never closes is the loudest row on the demand surface — a permanent alarm
// for a demand that ended, which is worse than no surface at all.
//
// IT CANNOT BE ONE SWEEP ACROSS BOTH SERVICES, and that is why this is the
// Edge half. Preconditions are owned by whoever holds the state behind them:
// a cell episode's level lives in Edge's claims and a changeover's terminal
// state lives in Edge's process_changeovers, neither of which Core has. Core
// runs its own sweep for threshold episodes plus the ownerless cases — orphan
// aging and childless auto-close — which need child counts only Core has.
//
// COST IS BOUNDED BY OPEN-EPISODE COUNT, which is small by construction: one
// per place that currently needs material. That is what makes a per-episode
// query affordable here where it would not be on a tick path.

// demandReconcileInterval is how long an episode can stay open after its
// precondition stops holding. Not a hot path — this is the floor under the
// notification sites, not a replacement for them, so a minute of latency on
// the rare miss costs nothing.
var demandReconcileInterval = 60 * time.Second

func (e *Engine) startDemandReconciler() {
	go e.runDemandReconciler()
}

func (e *Engine) runDemandReconciler() {
	ticker := time.NewTicker(demandReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C:
			e.reconcileDemand()
		}
	}
}

// reconcileDemand is one pass of both halves, in the order that makes the
// second half correct. sweepCellLevels refreshes below_reorder_since from the
// live count; reconcileDemandEpisodes reads that column to decide what to
// close. Run the other way round, every close would be deciding on a flag one
// period stale.
func (e *Engine) reconcileDemand() {
	e.sweepCellLevels()
	e.reconcileDemandEpisodes()
}

// ── THE LEVEL KEEPER ──────────────────────────────────────────────────────
//
// A LEVEL EVALUATED ONLY ON ITS NORMAL DATA PATH GOES BLIND EXACTLY WHEN THAT
// PATH STOPS, and for replenishment the two coincide precisely. A consume cell
// that has run out produces no consume ticks — the PLC gate stops the counter
// because a real machine cannot cycle an empty input — so the moment the demand
// becomes certain is the moment its only evaluator falls silent. The produce
// side is the same shape from the other end: a press at capacity stops
// pressing. Whichever ask happened to be taken at the crossing was the only ask
// that would ever be taken, and if it was refused the cell sat starved with
// nothing left to speak for it.
//
// So the level moves here, beside the close pass, for the reason in this file's
// header: an absence cannot be wired up. Both halves now walk state rather than
// wait to be told, and the tick goes back to being an accumulator.
//
// THE ASK IS DERIVED FROM THE COUNT, NEVER FROM below_reorder_since. That
// column is written on transition only, so it records where the level was when
// something last looked — which, off the tick, can be any length of line
// stoppage ago. Keying an ask on it orders parts for a cell holding a full bin,
// once per period, all weekend. remaining_uop_cached is written by the tick AND
// by the delivery handler, and needs no tick to read.
//
// COST IS ONE PASS OVER THE NODE TABLE per period — larger than the close
// pass's open-episode walk, and still nothing at a minute's cadence against a
// table the operator HMI reads on every page load.

// sweepCellLevels evaluates every cell's level and asks for the ones that are
// breached with nothing already coming.
func (e *Engine) sweepCellLevels() {
	procs, err := e.db.ListProcesses()
	if err != nil {
		e.logFn("demand_sweep: list processes: %v", err)
		return
	}
	for i := range procs {
		e.sweepProcessLevels(&procs[i])
	}
}

func (e *Engine) sweepProcessLevels(process *processes.Process) {
	// The same node population the tick sees: ListProcessNodesByProcess filters
	// retired rows, so a decommissioned node cannot be reordered for.
	nodes, err := e.db.ListProcessNodesByProcess(process.ID)
	if err != nil {
		e.logFn("demand_sweep: list nodes for process %s: %v", process.Name, err)
		return
	}
	for i := range nodes {
		node := &nodes[i]
		claim := activeClaimForProcess(e.db, process, node)
		if claim == nil {
			continue
		}
		runtime, err := e.db.GetProcessNodeRuntime(node.ID)
		if err != nil || runtime == nil {
			continue
		}
		e.sweepNodeLevel(node, runtime, claim)
	}
}

// sweepNodeLevel is the whole decision for one claim: record where the level
// is, then ask if it is breached and nothing is already on its way.
//
// EVERY ARM FAILS TO INACTION. A check that cannot see its input asks for
// nothing and closes nothing, and the next pass re-decides — the same posture
// the close pass takes above, for the same reason.
//
// PARKED A/B SIDES ARE NOT SKIPPED, and that is what makes the flip tail
// removable. The tick skips them because they are not consuming; the level does
// not care. A parked side drained before the flip is exactly the cell the old
// tail existed to reorder for, and it is asked for here on the same terms as
// every other node — with the reorder_point opt-out honoured, a bin already
// inbound seen, and the falling edge recorded, none of which the tail did.
func (e *Engine) sweepNodeLevel(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim) {
	if node == nil || runtime == nil || claim == nil {
		return
	}
	// manual_swap nodes (loader/unloader) are forklift-managed staging points,
	// not production cells. No PLC tag is tied to their contents, so the cached
	// count there is an operator declaration rather than a measurement, and a
	// level read off it would be ordering against a number nobody maintains.
	// Loader replenishment is Core-owned. The tick skips them for the same
	// reason. The guard sits with the decision rather than with the walk
	// because there are two callers of it now.
	if claim.SwapMode == protocol.SwapModeManualSwap {
		return
	}
	remaining := runtime.RemainingUOPCached

	// The evaluators are unmoved and unchanged; only the caller is new. They
	// stamp and clear below_reorder_since, and with the tick blocks gone they
	// are reached ONLY from here — so this pass is what keeps that column
	// alive, and cellPreconditionGone above reads it.
	//
	// EVALUATED REGARDLESS OF auto_reorder, which is a change and a deliberate
	// one. The whole evaluation used to sit inside `if claim.AutoReorder`, so a
	// plant that has not armed ordering yet never stamped a falling edge and
	// held no record that a cell had ever run short — precisely the evidence it
	// needs to decide whether it is ready to arm. The flag governs whether we
	// ACT, not whether it happened.
	var breached bool
	switch claim.Role {
	case protocol.ClaimRoleConsume:
		// reorder_point = 0 is the documented opt-out and the legacy default.
		if claim.ReorderPoint <= 0 {
			return
		}
		e.evaluateCellLevel(claim, remaining)
		breached = remaining <= claim.ReorderPoint
	case protocol.ClaimRoleProduce:
		if claim.UOPCapacity <= 0 {
			return
		}
		e.evaluateProduceLevel(claim, remaining)
		breached = remaining >= claim.UOPCapacity
	default:
		return
	}

	// RECOMPUTED, NOT TAKEN FROM THE EVALUATOR'S RETURN VALUE, and the
	// difference is the hysteresis band. Inside it the evaluator reports its
	// first return from the FLAG — correct for "is this episode still running",
	// wrong for "should we order". A cell at 26 against a level of 25 sits in
	// the band with the flag still set, and the tick never ordered there.
	if !breached || !claim.AutoReorder {
		return
	}

	// THE DEDUP IS THE NODE — not the episode, and not the origin.
	//
	// process_node_id is a column Edge writes itself at create time, so this
	// sees an order this same pass made a moment ago with no Core round trip,
	// no projection and no outbox drain. An origin count could not: on Edge,
	// origin_id is written only by Core's projection upsert, so it reads zero
	// for an order seconds old — the 2026-08-03 duplicate shape one level down.
	//
	// Node grain is the right question independent of that. It sees a bin
	// already inbound from a changeover, an operator push or a swap's return
	// leg, which is the two-bins-on-one-node family; and the episode key is
	// process-grain, so an episode-scoped count would let two nodes on one
	// process sharing a payload shadow each other — one served, the other
	// suppressed indefinitely.
	active, err := e.db.ListActiveOrdersByProcessNode(node.ID)
	if err != nil {
		e.logFn("demand_sweep: node %s is below its level but its in-flight orders cannot be read (%v) "+
			"— asking for nothing this pass; the next one re-decides", node.Name, err)
		return
	}
	if len(active) > 0 {
		o := active[0]
		e.debugFn("demand_sweep: node %s below its level, but order %d (%s, %s) already points here",
			node.Name, o.ID, o.OrderType, o.Status)
		return
	}
	if canAccept, reason := e.CanAcceptOrders(node.ID); !canAccept {
		e.debugFn("demand_sweep: node %s below its level but not accepting orders: %s", node.Name, reason)
		return
	}

	e.logFn("demand_sweep: node %s role=%s remaining=%d — below its level with nothing in flight; asking",
		node.Name, claim.Role, remaining)
	var askErr error
	switch claim.Role {
	case protocol.ClaimRoleConsume:
		_, askErr = e.requestNodeMaterialFor(node.ID, 1, protocol.EpisodeTriggerAutoreorder)
	case protocol.ClaimRoleProduce:
		_, askErr = e.requestProduceSwapFor(node.ID, protocol.EpisodeTriggerAutoreorder)
	}
	if askErr != nil {
		// Not an error state. The guards downstream refuse for good reasons and
		// say why; the level is still breached, so the next pass asks again.
		e.logFn("demand_sweep: ask for node %s refused: %v", node.Name, askErr)
	}
}

// reconcileDemandEpisodes is one pass. Split from the loop so tests drive it
// directly rather than waiting on a ticker.
func (e *Engine) reconcileDemandEpisodes() {
	open, err := e.db.ListOpenDemandOrigins()
	if err != nil {
		e.logFn("demand_reconciler: list open episodes: %v", err)
		return
	}
	for i := range open {
		ep := &open[i]
		reason, stale, err := e.episodePreconditionGone(ep)
		if err != nil {
			// A read failure is NOT evidence the precondition is gone. Leaving
			// the episode open is the honest outcome — the next pass asks
			// again, and a check that cannot see its input must never render
			// as a finding.
			e.logFn("demand_reconciler: evaluate %s: %v", ep.EpisodeKey, err)
			continue
		}
		if !stale {
			continue
		}
		e.logFn("demand_reconciler: closing %s (origin=%s kind=%s) — precondition no longer holds: %s",
			ep.EpisodeKey, ep.OriginID, ep.Kind, reason)
		e.closeEpisode(ep.EpisodeKey, reason, protocol.ClosedBySweep)
	}
}

// episodePreconditionGone asks whether one episode should still be open, and
// says WHY it should not.
//
// The reason is not decoration. "recovered" and "claim_removed" are different
// facts about the plant — one is a cell that got its material, the other is a
// need that stopped being asked because the configuration moved underneath it.
// A surface that merged them could not tell a healthy ending from a silent
// disappearance.
func (e *Engine) episodePreconditionGone(ep *store.OpenOrigin) (reason string, gone bool, err error) {
	switch ep.Kind {
	case protocol.EpisodeKindCell:
		return e.cellPreconditionGone(ep)
	case protocol.EpisodeKindChangeover:
		return e.changeoverPreconditionGone(ep)
	default:
		// Threshold episodes belong to Core's sweep and must never be closed
		// from here: Edge cannot read a plant-wide in-loop total, so it has no
		// basis to say the precondition failed. An unknown kind is left alone
		// for the same reason — silence beats a guess.
		return "", false, nil
	}
}

// cellPreconditionGone: is any claim on this process still below its level for
// this payload and direction?
func (e *Engine) cellPreconditionGone(ep *store.OpenOrigin) (string, bool, error) {
	// THE TRANSLATION TABLE IS GONE. This read
	//
	//	role := string(protocol.ClaimRoleConsume)
	//	if ep.Direction == protocol.EpisodeDirectionEvacuate { role = ... }
	//
	// — a hand-written 1:1 dictionary from the episode's vocabulary back into
	// the claim's, which is where the value came from in the first place. The
	// episode now stores the role itself, so there is nothing to translate and
	// nothing to get backwards. Defaulting to consume on an unrecognised word,
	// which is what the old shape did silently, was the hazard: a key written by
	// a site that picked the wrong spelling swept the wrong role's claims.
	role := string(ep.Direction)
	breached, err := e.db.CellLevelStillBreached(ep.ProcessID, ep.PayloadCode, role)
	if err != nil {
		return "", false, err
	}
	if breached {
		return "", false, nil
	}
	// The flag is not set anywhere that counts. Two ways that happens, and they
	// are different facts: the level came back up (the rising edge cleared it,
	// and the close simply did not land), or there is no such claim to be
	// below any more. Ask which.
	claimed, err := e.db.CellPayloadStillClaimed(ep.ProcessID, ep.PayloadCode, role)
	if err != nil {
		return "", false, err
	}
	if claimed {
		return protocol.CloseReasonRecovered, true, nil
	}
	return protocol.CloseReasonClaimRemoved, true, nil
}

// changeoverPreconditionGone: is the changeover row still running?
func (e *Engine) changeoverPreconditionGone(ep *store.OpenOrigin) (string, bool, error) {
	parsed, err := protocol.ParseEpisodeKey(ep.EpisodeKey)
	if err != nil {
		return "", false, err
	}
	co, err := e.db.GetProcessChangeoverByID(parsed.ChangeoverID)
	if err != nil {
		return "", false, err
	}
	if co == nil {
		// The row is gone. A changeover that was deleted rather than resolved
		// did not complete, and calling it complete would inflate every
		// changeover success figure that ever reads this column.
		return protocol.CloseReasonCancelled, true, nil
	}
	switch co.State {
	case domain.ChangeoverCompleted:
		return protocol.CloseReasonChangeoverComplete, true, nil
	case domain.ChangeoverCancelled:
		return protocol.CloseReasonCancelled, true, nil
	default:
		return "", false, nil
	}
}

// sweepNodeLevelNow runs the level decision for one node immediately, off the
// period.
//
// A LATENCY OPTIMISATION, IN THE SENSE THIS FILE'S HEADER ALREADY USES — the
// same relationship the notification close paths have to the close sweep. It
// makes nothing possible that the next pass would not do anyway, so a caller
// that forgets it costs a minute and never correctness.
func (e *Engine) sweepNodeLevelNow(nodeID int64) {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		e.logFn("demand_sweep: immediate level check for node %d: %v", nodeID, err)
		return
	}
	e.sweepNodeLevel(node, runtime, claim)
}
