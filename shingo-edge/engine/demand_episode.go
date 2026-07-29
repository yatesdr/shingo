package engine

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// demand_episode.go — the Edge half of the demand grain.
//
// A DEMAND EPISODE is a continuous period during which a specific place needs
// material. It opens on a falling edge, closes on a rising edge, and every
// order created to serve it is its child. That unit is what makes orders
// navigable: without it, 2026-07-21 is 484 unrelated orders, and with it it is
// ONE demand that cost 484 orders and delivered nothing.
//
// THE GRAIN IS THE PROCESS, NOT THE NODE. A press-index cell is one process
// spanning several nodes and its swap is one demand served by a multi-node
// dance; an A/B pair is two claims on one process, and the process needs the
// payload regardless of which half is currently pulling. A/B cycling,
// press-index priming, paired positions and prime fills are CHOREOGRAPHY, not
// demand: anything created within one entry-point call belongs to that call's
// episode. See protocol.CellEpisodeKey.
//
// WHAT IS DURABLE AND WHAT IS NOT. The open episode lives in
// demand_origins_open, keyed on episode_key; the falling edge lives on the
// claim in below_reorder_since. Both are written ON TRANSITION ONLY — the level
// is evaluated on every PLC consume tick and neither value changes more than
// twice per episode. They are durable at all because Edge restarts more often
// than anything else here, and a lost open episode means the next tick mints a
// duplicate while the first never closes.

// openCellEpisode opens — or JOINS — the demand episode for a cell.
//
// Returns the origin id to stamp on every order this call creates, and whether
// an existing episode was joined rather than a new one minted.
//
// JOINING IS THE COMMON CASE FOR AN OPERATOR PUSH. A button pressed while an
// episode is open is not a new demand; it is the same one expressed
// impatiently. It increments rerequest_count and re-sends origin.opened, which
// is an upsert on Core — so the count is visible while the episode is OPEN,
// rather than arriving at the moment it stops being useful.
//
// expectedOrders is stamped ONCE, at open, and never recomputed or accumulated.
// Accumulating per re-fire would render 2026-07-21 as ratio 1.0 — normal, and
// invisible. A join therefore does NOT touch it.
func (e *Engine) openCellEpisode(
	processID int64,
	claim *processes.NodeClaim,
	direction, trigger string,
	expectedOrders int,
	openedTotal int,
	discretionary bool,
) (string, bool, error) {
	if claim == nil {
		return "", false, nil
	}
	key := protocol.CellEpisodeKey(e.cfg.StationID(), processID, string(claim.PayloadCode), direction)

	if open, err := e.db.GetOpenDemandOrigin(key); err == nil && open != nil {
		joined, jerr := e.db.JoinDemandOrigin(key)
		if jerr != nil {
			return open.OriginID, true, jerr
		}
		e.logFn("demand_episode: JOINED origin=%s key=%s trigger=%s rerequests=%d rev=%d — same demand, expressed again",
			joined.OriginID, key, trigger, joined.RerequestCount, joined.Revision)
		// Re-send the WHOLE state at the new revision, so the count is current
		// on Core while the episode is still open — which is the only time
		// knowing an operator pushed six times is any use.
		//
		// Ignored deliberately: the row is still on disk, so a failed enqueue
		// costs a stale rerequest_count on Core until the next change, and the
		// close re-sends everything regardless.
		_ = e.emitOriginState(joined, nil, "", "")
		return joined.OriginID, true, nil
	} else if err != nil && !errors.Is(err, store.ErrOriginNotOpen) {
		return "", false, err
	}

	expected := expectedOrders
	row := &store.OpenOrigin{
		EpisodeKey: key,
		OriginID:   uuid.NewString(),
		Kind:       protocol.EpisodeKindCell,
		Direction:  direction,
		// TriggerKind, not Trigger: SQLite reserves TRIGGER as a keyword.
		TriggerKind:    trigger,
		TriggerRef:     claimTriggerRef(claim),
		ProcessID:      processID,
		CoreNodeName:   claim.CoreNodeName,
		PayloadCode:    string(claim.PayloadCode),
		OpenedTotal:    openedTotal,
		Threshold:      claim.ReorderPoint,
		ExpectedOrders: &expected,
		Discretionary:  discretionary,
		OpenedAt:       time.Now().UTC(),
	}
	if err := e.db.OpenDemandOrigin(row); err != nil {
		return "", false, err
	}
	e.logFn("demand_episode: OPENED origin=%s key=%s trigger=%s expected_orders=%d opened_total=%d discretionary=%v",
		row.OriginID, key, trigger, expected, openedTotal, discretionary)
	// Ignored deliberately — see emitOriginState. A lost OPEN self-heals: the
	// row is durable and the close carries the whole episode.
	_ = e.emitOriginState(row, nil, "", "")
	return row.OriginID, false, nil
}

// closeCellEpisode ends the episode for a place, if one is open.
func (e *Engine) closeCellEpisode(processID int64, payload, direction, reason, closedBy string) {
	e.closeEpisode(protocol.CellEpisodeKey(e.cfg.StationID(), processID, payload, direction), reason, closedBy)
}

// closeEpisode is the ONE close path for every kind Edge owns.
//
// ENQUEUE FIRST, THEN DELETE. The reverse order can lose a close entirely — the
// row is gone and nothing will ever say so — while this order can at worst
// re-send one, and a re-send is a no-op under Core's revision guard. Once the
// message is on the durable outbox, delivery is the outbox's job: it retries
// and dead-letters on its own, which is what lets this table keep meaning
// "episodes that are open" rather than "episodes we are still responsible for".
//
// Closing something already closed is a NO-OP, not an error: the falling edge
// is evaluated from more than one site — a recovery tick, the state-change
// pokes that exist because a node which stops consuming produces no ticks, and
// the reconciling sweep — so two of them racing to close one episode is
// ordinary rather than exceptional.
func (e *Engine) closeEpisode(key, reason, closedBy string) {
	closed, err := e.db.CloseDemandOrigin(key)
	if errors.Is(err, store.ErrOriginNotOpen) {
		return
	}
	if err != nil {
		e.logFn("demand_episode: close %s: %v", key, err)
		return
	}
	closedAt := time.Now().UTC()
	e.logFn("demand_episode: CLOSED origin=%s key=%s reason=%s duration=%s rerequests=%d rev=%d",
		closed.OriginID, key, reason, closedAt.Sub(closed.OpenedAt).Round(time.Second),
		closed.RerequestCount, closed.Revision)

	if err := e.emitOriginState(closed, &closedAt, reason, closedBy); err != nil {
		// KEEP THE ROW. The close never reached the outbox, so deleting here
		// would lose it outright — nothing would ever tell Core the episode
		// ended, and no sweep could notice, because the sweep reads this very
		// table. That is the failure the enqueue-then-delete order exists to
		// prevent, and skipping the delete is what actually collects it: the
		// episode stays open, the reconciler closes it again at a higher
		// revision, and the re-send is a no-op under Core's guard.
		//
		// production.tick does the same thing in its own shape — it restores
		// the delta snapshot when the enqueue fails, rather than trusting the
		// outbox with something that never got there.
		e.logFn("demand_episode: close %s kept open — state not enqueued: %v", key, err)
		return
	}
	if err := e.db.DeleteDemandOrigin(key); err != nil {
		// The state IS enqueued, so Core converges regardless. A row left
		// behind reads as still-open until the reconciler sweeps it and sends
		// the close again — harmless under the revision guard.
		e.logFn("demand_episode: delete closed episode %s: %v", key, err)
	}
}

// evaluateCellLevel is the falling/rising edge for one claim.
//
// CALL IT ON STATE CHANGES, NOT ONLY ON TICKS. A level trigger evaluated only
// on its normal data path goes blind exactly when that path stops: a node that
// stops consuming produces no consume ticks, so its level is never
// re-evaluated and its episode never closes. Both services have hit this
// independently — FlipABNode exists on Edge for it, engagePayloads/Resync on
// Core.
//
// It records the falling edge and reports whether the episode should now CLOSE.
// Opening is left to the caller, because only the caller knows how many orders
// the plan it is about to run will create, and expected_orders is stamped from
// that.
func (e *Engine) evaluateCellLevel(claim *processes.NodeClaim, remainingUOP int) (below bool, shouldClose bool) {
	if claim == nil || claim.ReorderPoint <= 0 {
		return false, false
	}
	margin := e.cfg.HysteresisMargin(claim.ReorderPoint)

	switch {
	case remainingUOP <= claim.ReorderPoint:
		// Falling edge. Stamp it once; a claim already below stays below, and
		// re-stamping would make every episode look as if it had just started.
		if claim.BelowReorderSince == nil {
			now := time.Now().UTC()
			if err := e.db.SetClaimBelowReorderSince(claim.ID, &now); err != nil {
				e.logFn("demand_episode: stamp falling edge claim=%d: %v", claim.ID, err)
			}
			claim.BelowReorderSince = &now
		}
		return true, false

	case remainingUOP > claim.ReorderPoint+margin:
		// Rising edge, and only ABOVE THE MARGIN. Closing at exactly the
		// reorder point would mint a fresh episode every time a tick nudged the
		// count across it — thousands of 20-second noise episodes, worst at a
		// low-mix plant where each one matters most.
		if claim.BelowReorderSince != nil {
			if err := e.db.SetClaimBelowReorderSince(claim.ID, nil); err != nil {
				e.logFn("demand_episode: clear falling edge claim=%d: %v", claim.ID, err)
			}
			claim.BelowReorderSince = nil
			e.debugFn("demand_episode: claim=%d node=%s recovered to %d (> reorder %d + margin %d)",
				claim.ID, claim.CoreNodeName, remainingUOP, claim.ReorderPoint, margin)
			return false, true
		}
		return false, false

	default:
		// Inside the hysteresis band: above the reorder point but not yet clear
		// of the margin. Neither edge — deliberately, that is what the band is.
		return claim.BelowReorderSince != nil, false
	}
}

// evaluateProduceLevel is evaluateCellLevel's mirror for the evacuate
// direction.
//
// THE LEVEL RUNS THE OTHER WAY. A produce node FILLS toward UOPCapacity rather
// than draining toward a reorder point, so the state that needs attention is a
// HIGH reading, and recovery is the count dropping back down after the full bin
// leaves. Everything else is identical — one crossing stamps the edge, the
// margin absorbs the wobble, and the episode between the edges is one demand.
//
// It shares below_reorder_since with the consume side, and that is not a
// shortcut: a claim has exactly one role, so a single claim is only ever one
// direction. The column means "since this claim's level was breached".
func (e *Engine) evaluateProduceLevel(claim *processes.NodeClaim, remainingUOP int) (breached bool, shouldClose bool) {
	if claim == nil || claim.UOPCapacity <= 0 {
		return false, false
	}
	margin := e.cfg.HysteresisMargin(claim.UOPCapacity)

	switch {
	case remainingUOP >= claim.UOPCapacity:
		if claim.BelowReorderSince == nil {
			now := time.Now().UTC()
			if err := e.db.SetClaimBelowReorderSince(claim.ID, &now); err != nil {
				e.logFn("demand_episode: stamp produce edge claim=%d: %v", claim.ID, err)
			}
			claim.BelowReorderSince = &now
		}
		return true, false

	case remainingUOP < claim.UOPCapacity-margin:
		if claim.BelowReorderSince != nil {
			if err := e.db.SetClaimBelowReorderSince(claim.ID, nil); err != nil {
				e.logFn("demand_episode: clear produce edge claim=%d: %v", claim.ID, err)
			}
			claim.BelowReorderSince = nil
			e.debugFn("demand_episode: claim=%d node=%s relieved to %d (< capacity %d - margin %d)",
				claim.ID, claim.CoreNodeName, remainingUOP, claim.UOPCapacity, margin)
			return false, true
		}
		return false, false

	default:
		// Inside the band: below capacity but not yet clear of the margin.
		return claim.BelowReorderSince != nil, false
	}
}

// claimTriggerRef names the claim behind a mint. Forensic, not identity: the
// identity is the episode key, which is per process.
func claimTriggerRef(claim *processes.NodeClaim) string {
	if claim == nil {
		return ""
	}
	return "claim:" + itoa64(claim.ID)
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// emitOriginState puts the WHOLE episode row on the DURABLE OUTBOX.
//
// The outbox, not a direct send, because a zero-order episode has no order
// message to ride on and is the most valuable row on the surface — a cell that
// asked for four hours and got nothing. If Core is down when it changes, the
// episode still arrives.
//
// STATE, NOT AN EVENT. Core upserts on origin_id guarded by the revision, so a
// duplicate is a no-op, a reordered pair resolves by comparison rather than by
// a parking queue, and the LAST message is sufficient on its own — lose
// everything except the close and Core still converges. Rebuilding episode
// state on Core by replaying opened/closed events would be a second copy
// maintained by replay, which is the uopCache mistake in a new place.
//
// ONE emitter, deliberately: the message shape cannot drift between two call
// paths if there is only one.
//
// Best-effort by design: a failure to record an episode must never fail the
// material request it describes. Observability does not get to stop the plant.
//
// IT RETURNS THE ERROR ANYWAY, because one caller must act on it. On OPEN and
// JOIN a failed enqueue self-heals — the row stays on disk and the close
// carries the whole episode, so losing an open costs nothing once the last
// message lands. On CLOSE it does not: the row is about to be deleted, and
// deleting it after a failed enqueue loses the close outright, with no sweep
// able to notice because the sweep reads that table. So close checks this and
// keeps the row; the open paths deliberately ignore it.
func (e *Engine) emitOriginState(o *store.OpenOrigin, closedAt *time.Time, closeReason, closedBy string) error {
	if o == nil {
		return nil
	}
	msg := protocol.DemandOriginState{
		OriginID: o.OriginID, Revision: o.Revision, EpisodeKey: o.EpisodeKey,
		Kind: o.Kind, Direction: o.Direction, Trigger: o.TriggerKind,
		TriggerRef: o.TriggerRef, ProcessID: o.ProcessID,
		CoreNodeName: o.CoreNodeName, PayloadCode: o.PayloadCode,
		OpenedAt: o.OpenedAt, OpenedTotal: o.OpenedTotal, Threshold: o.Threshold,
		ExpectedOrders: o.ExpectedOrders, ExpectedUnknownReason: o.ExpectedUnknownReason,
		RerequestCount: o.RerequestCount, Discretionary: o.Discretionary,
		ClosedAt: closedAt, CloseReason: closeReason, ClosedBy: closedBy,
	}
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectDemandOrigin,
		protocol.Address{Role: protocol.RoleEdge, Station: e.cfg.StationID()},
		protocol.Address{Role: protocol.RoleCore},
		&msg,
	)
	if err != nil {
		log.Printf("demand_episode: build envelope origin=%s: %v", o.OriginID, err)
		return fmt.Errorf("build envelope origin=%s: %w", o.OriginID, err)
	}
	data, err := env.Encode()
	if err != nil {
		log.Printf("demand_episode: encode origin=%s: %v", o.OriginID, err)
		return fmt.Errorf("encode origin=%s: %w", o.OriginID, err)
	}
	if _, err := e.db.EnqueueOutbox(data, protocol.SubjectDemandOrigin); err != nil {
		log.Printf("demand_episode: enqueue origin=%s: %v", o.OriginID, err)
		return fmt.Errorf("enqueue origin=%s: %w", o.OriginID, err)
	}
	return nil
}

// openChangeoverEpisode mints the episode for a changeover.
//
// Keyed on ProcessChangeoverID because that row already has exactly the
// episode's lifetime: to_style_id is written only at INSERT and nothing
// re-targets a row, so one changeover is one episode. Cancel-and-redirect
// cancels this row and inserts a fresh one — correctly a new id and a new
// episode, not a continuation.
//
// It is an EVENT trigger, not a level: a changeover arms, it does not cross a
// threshold. So there is no hysteresis here and no falling edge to stamp — the
// arming IS the edge.
//
// IT LIVES IN THE SAME TABLE AS THE CELL KINDS, and process_changeovers.
// origin_id stays as the durable back-pointer from a changeover to its demand.
// Two emission paths for one subject is where the second one drifts — and the
// state message needs content, above all expected_orders, that the changeover
// row has nowhere to put.
func (e *Engine) openChangeoverEpisode(co *processes.Changeover, expectedOrders int) string {
	if co == nil {
		return ""
	}
	key := protocol.ChangeoverEpisodeKey(e.cfg.StationID(), co.ID)
	if open, err := e.db.GetOpenDemandOrigin(key); err == nil && open != nil {
		return open.OriginID // already minted — a changeover arms once
	}

	expected := expectedOrders
	row := &store.OpenOrigin{
		EpisodeKey: key,
		OriginID:   uuid.NewString(),
		Kind:       protocol.EpisodeKindChangeover,
		// TriggerRef is the changeover row, which is also the identity here —
		// unlike the cell kind, where the claim is forensic and the process is
		// the identity.
		TriggerRef:     "changeover:" + itoa64(co.ID),
		ProcessID:      co.ProcessID,
		ExpectedOrders: &expected,
		OpenedAt:       time.Now().UTC(),
	}
	if err := e.db.OpenDemandOrigin(row); err != nil {
		e.logFn("demand_episode: open changeover episode co=%d: %v", co.ID, err)
		return ""
	}
	if err := e.db.SetChangeoverOriginID(co.ID, row.OriginID); err != nil {
		// The back-pointer is a convenience for reading a changeover row; the
		// episode itself is already durable, so this logs and does not fail.
		e.logFn("demand_episode: stamp changeover back-pointer co=%d: %v", co.ID, err)
	}
	e.logFn("demand_episode: OPENED origin=%s key=%s kind=changeover process=%d expected_orders=%d",
		row.OriginID, key, co.ProcessID, expected)
	// Ignored deliberately — see emitOriginState.
	_ = e.emitOriginState(row, nil, "", "")
	return row.OriginID
}

// closeChangeoverEpisode ends a changeover's episode.
//
// reason distinguishes the two endings the design names: changeover_complete
// and cancelled. They are not the same outcome and a surface that merged them
// would hide every abandoned changeover behind a wall of successful ones.
//
// The changeover row keeps its origin_id as a durable back-pointer even after
// the episode closes — it is the link from a changeover to its demand, and
// clearing it would throw that away to save nothing.
func (e *Engine) closeChangeoverEpisode(changeoverID int64, reason, closedBy string) {
	e.closeEpisode(protocol.ChangeoverEpisodeKey(e.cfg.StationID(), changeoverID), reason, closedBy)
}
