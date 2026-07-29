package engine

import (
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
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
			e.reconcileDemandEpisodes()
		}
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
	role := string(protocol.ClaimRoleConsume)
	if ep.Direction == protocol.EpisodeDirectionEvacuate {
		role = string(protocol.ClaimRoleProduce)
	}
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
