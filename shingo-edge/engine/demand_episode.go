package engine

import (
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
	station := e.cfg.StationID()
	key := protocol.CellEpisodeKey(station, processID, string(claim.PayloadCode), direction)

	if open, err := e.db.GetOpenDemandOrigin(key); err == nil && open != nil {
		count, jerr := e.db.JoinDemandOrigin(key)
		if jerr != nil {
			return open.OriginID, true, jerr
		}
		e.logFn("demand_episode: JOINED origin=%s key=%s trigger=%s rerequests=%d — same demand, expressed again",
			open.OriginID, key, trigger, count)
		// Re-send as an upsert so the open episode's count is current on Core.
		e.emitOriginOpened(protocol.DemandOriginOpened{
			OriginID: open.OriginID, EpisodeKey: key,
			Kind: protocol.EpisodeKindCell, Direction: direction, Trigger: trigger,
			TriggerRef: claimTriggerRef(claim), ProcessID: processID,
			CoreNodeName: claim.CoreNodeName, PayloadCode: string(claim.PayloadCode),
			OpenedAt: open.OpenedAt, RerequestCount: count,
		})
		return open.OriginID, true, nil
	} else if err != nil && err != store.ErrOriginNotOpen {
		return "", false, err
	}

	originID := uuid.NewString()
	openedAt := time.Now().UTC()
	if err := e.db.OpenDemandOrigin(key, originID, openedAt); err != nil {
		return "", false, err
	}
	e.logFn("demand_episode: OPENED origin=%s key=%s trigger=%s expected_orders=%d opened_total=%d discretionary=%v",
		originID, key, trigger, expectedOrders, openedTotal, discretionary)

	e.emitOriginOpened(protocol.DemandOriginOpened{
		OriginID: originID, EpisodeKey: key,
		Kind: protocol.EpisodeKindCell, Direction: direction, Trigger: trigger,
		TriggerRef: claimTriggerRef(claim), ProcessID: processID,
		CoreNodeName: claim.CoreNodeName, PayloadCode: string(claim.PayloadCode),
		OpenedAt: openedAt, OpenedTotal: openedTotal, Threshold: claim.ReorderPoint,
		ExpectedOrders: expectedOrders, Discretionary: discretionary,
	})
	return originID, false, nil
}

// closeCellEpisode ends the episode for a place, if one is open.
//
// Closing something already closed is a NO-OP, not an error: the falling edge
// is evaluated from more than one site (a recovery tick, and the state-change
// pokes that exist because a node which stops consuming produces no ticks at
// all), so two of them racing to close one episode is ordinary.
func (e *Engine) closeCellEpisode(processID int64, payload, direction, reason string) {
	key := protocol.CellEpisodeKey(e.cfg.StationID(), processID, payload, direction)
	closed, err := e.db.CloseDemandOrigin(key)
	if err == store.ErrOriginNotOpen {
		return
	}
	if err != nil {
		e.logFn("demand_episode: close %s: %v", key, err)
		return
	}
	closedAt := time.Now().UTC()
	e.logFn("demand_episode: CLOSED origin=%s key=%s reason=%s duration=%s rerequests=%d",
		closed.OriginID, key, reason, closedAt.Sub(closed.OpenedAt).Round(time.Second), closed.RerequestCount)
	e.emitOriginClosed(protocol.DemandOriginClosed{
		OriginID: closed.OriginID, ClosedAt: closedAt,
		CloseReason: reason, RerequestCount: closed.RerequestCount,
	})
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

// emitOriginOpened / emitOriginClosed put an episode on the DURABLE OUTBOX.
//
// The outbox, not a direct send, because a zero-order episode has no order
// message to ride on and is the most valuable row on the surface — a cell that
// asked for four hours and got nothing. If Core is down when it opens, the
// episode still arrives.
//
// Best-effort by design: a failure to record the episode must never fail the
// material request it describes. Observability does not get to stop the plant.
func (e *Engine) emitOriginOpened(p protocol.DemandOriginOpened) {
	e.enqueueOrigin(protocol.SubjectDemandOriginOpened, &p, p.OriginID)
}

func (e *Engine) emitOriginClosed(p protocol.DemandOriginClosed) {
	e.enqueueOrigin(protocol.SubjectDemandOriginClosed, &p, p.OriginID)
}

func (e *Engine) enqueueOrigin(subject string, payload any, originID string) {
	station := e.cfg.StationID()
	env, err := protocol.NewDataEnvelope(
		subject,
		protocol.Address{Role: protocol.RoleEdge, Station: station},
		protocol.Address{Role: protocol.RoleCore},
		payload,
	)
	if err != nil {
		log.Printf("demand_episode: build %s envelope origin=%s: %v", subject, originID, err)
		return
	}
	data, err := env.Encode()
	if err != nil {
		log.Printf("demand_episode: encode %s origin=%s: %v", subject, originID, err)
		return
	}
	if _, err := e.db.EnqueueOutbox(data, subject); err != nil {
		log.Printf("demand_episode: enqueue %s origin=%s: %v", subject, originID, err)
	}
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
// The origin id lands on process_changeovers.origin_id, which makes it durable
// across an Edge restart for free.
func (e *Engine) openChangeoverEpisode(co *processes.Changeover, expectedOrders int) string {
	if co == nil {
		return ""
	}
	station := e.cfg.StationID()
	key := protocol.ChangeoverEpisodeKey(station, co.ID)
	originID := uuid.NewString()
	openedAt := time.Now().UTC()

	if err := e.db.SetChangeoverOriginID(co.ID, originID); err != nil {
		e.logFn("demand_episode: stamp changeover origin co=%d: %v", co.ID, err)
		return ""
	}
	e.logFn("demand_episode: OPENED origin=%s key=%s kind=changeover process=%d expected_orders=%d",
		originID, key, co.ProcessID, expectedOrders)

	e.emitOriginOpened(protocol.DemandOriginOpened{
		OriginID: originID, EpisodeKey: key,
		Kind: protocol.EpisodeKindChangeover,
		// TriggerRef is the changeover row, which is also the identity here —
		// unlike the cell kind, where the claim is forensic and the process is
		// the identity.
		TriggerRef: "changeover:" + itoa64(co.ID),
		ProcessID:  co.ProcessID,
		OpenedAt:   openedAt, ExpectedOrders: expectedOrders,
	})
	return originID
}

// closeChangeoverEpisode ends a changeover's episode.
//
// reason distinguishes the two endings the design names: changeover_complete
// and cancelled. They are not the same outcome and a surface that merged them
// would hide every abandoned changeover behind a wall of successful ones.
//
// Idempotent: a changeover whose origin is already cleared closes to nothing.
func (e *Engine) closeChangeoverEpisode(changeoverID int64, reason string) {
	originID, err := e.db.GetChangeoverOriginID(changeoverID)
	if err != nil || originID == "" {
		return
	}
	closedAt := time.Now().UTC()
	e.logFn("demand_episode: CLOSED origin=%s kind=changeover co=%d reason=%s", originID, changeoverID, reason)
	e.emitOriginClosed(protocol.DemandOriginClosed{
		OriginID: originID, ClosedAt: closedAt, CloseReason: reason,
	})
	// The row keeps its origin_id as a durable back-pointer — it is the link
	// from a changeover to its demand, and clearing it would throw that away to
	// save nothing. Emitting twice is prevented by the state guard on
	// UpdateChangeoverStateWithTrigger: a terminal row cannot re-enter a
	// non-terminal state, so a changeover terminalises once.
}
