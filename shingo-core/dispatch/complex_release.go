package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/orders"
)

// HandleOrderRelease processes a release request for a staged (dwelling) order.
// Multi-wait support: the order's WaitIndex tracks how many wait points have
// been consumed. Each release emits only the next segment (steps between
// consecutive waits) and increments the index. The fleet order stays staged
// (complete=false) until the final segment is released.
func (d *Dispatcher) HandleOrderRelease(env *protocol.Envelope, p *protocol.OrderRelease) {
	d.dbg("order release: station=%s uuid=%s", env.Src.Station, p.OrderUUID)

	order, ok := d.getOwnedOrder(env, p.OrderUUID)
	if !ok {
		d.sendError(env, p.OrderUUID, "not_found", "order not found or access denied")
		return
	}

	// A faulted leg is mid-recovery (SEER FAILED → Core's non-terminal `faulted`),
	// not terminally dead — Core keeps the order alive and it re-stages once the
	// robot recovers. Edge's consolidated two-robot release fans a release out to
	// BOTH legs of a swap; when it reaches a faulted leg we must NOT reply with an
	// error, because Edge turns any non-`manifest_sync_failed` order-error into a
	// terminal StatusFailed — killing the Edge mirror while Core's order lives on
	// (the ALN_003 divergence, Springfield 2026-06-12). No-op the release: there
	// is nothing to dispatch on a faulted leg, and the post-recovery re-release
	// re-fires normally. Mirrors the in_transit duplicate-fan-out special-case below.
	if order.Status == StatusFaulted {
		d.dbg("complex release: order %d is faulted (recovering) — no-op release, will re-stage", order.ID)
		return
	}

	// Precondition: order must be staged or in_transit. InTransit is accepted
	// for duplicate fan-out from Edge's consolidated two-robot release and for
	// multi-wait re-release. The real duplicate gate is splitSegment returning nil.
	if order.Status != StatusStaged && order.Status != StatusInTransit {
		d.sendError(env, p.OrderUUID, "invalid_state",
			fmt.Sprintf("order must be staged or in_transit to release, got %s", order.Status))
		return
	}

	if err := d.syncManifestForRelease(env, order, p); err != nil {
		return
	}

	// ── The fence: one decider for every append ───────────────────────────
	// Everything above this line consumed the station's contribution — the whole
	// OrderRelease payload (RemainingUOP, Disposition, CalledBy) is read inside
	// syncManifestForRelease and nowhere else. Everything below reads only the
	// order row. So the cut between REPORTING a fact and DECIDING to append is
	// already here; this states it.
	//
	// A GATE WAIT's precondition is internal to Core — a lane claim, a robot
	// inside, a slot reachable — and nothing outside Core can know when it is
	// satisfied. Only the evaluator (lane_gate_release.go) may advance one. This
	// handler predates lane gating entirely; it was built for the case where a
	// station reports something Core genuinely cannot see, and nobody scoped it
	// when the gate arrived.
	//
	// Core is what makes this reachable, which is worth stating because it is not
	// a story about a stray click: the robot parks on its Wait block, RDS reports
	// WAITING, MapState turns that into staged, Core writes it and pushes
	// TypeOrderStaged to the station — so the board is INVITED to offer a release,
	// and the status precondition above passes because Core itself just wrote it.
	// Removing the invitation is a separate step; this is the fence that holds
	// whether or not a button exists.
	//
	// invalid_state, not a silent drop: Edge handles this code non-terminally on
	// purpose (edge_handler.go, the ALN_003 divergence — a release rejection must
	// never kill the Edge mirror). Any other code would terminalize the Edge row
	// while Core's order lived on.
	if IsGateStaged(order) {
		d.dbg("release refused: order %d is parked on a gate wait — only the lane evaluator advances one",
			order.ID)
		d.sendError(env, p.OrderUUID, "invalid_state",
			"this order is waiting on a lane, not on the station; Core releases it when the lane is safe")
		return
	}

	var steps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &steps); err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to parse stored steps")
		return
	}

	segment, moreWaits, blockOffset := splitSegment(steps, order.WaitIndex)
	if segment == nil {
		if order.Status == StatusInTransit {
			d.dbg("complex release: order %d already in_transit with wait_index %d past final wait — no-op",
				order.ID, order.WaitIndex)
			return
		}
		d.sendError(env, p.OrderUUID, "invalid_state",
			fmt.Sprintf("wait_index %d exceeds number of waits in order", order.WaitIndex))
		return
	}

	d.patchRedirectSegments(segment, order, moreWaits)
	d.dispatchFleetRelease(env, order, segment, moreWaits, blockOffset)
}

// syncManifestForRelease performs the late-bind bin manifest sync at release
// time. p.RemainingUOP carries the operator's intent: nil = no change
// (legacy/Order-A path), 0 = bin empty (NOTHING PULLED), >0 = partial
// (SEND PARTIAL BACK). Must run before backend.ReleaseOrder so the fleet
// doesn't proceed against an inconsistent manifest.
//
// When order.BinID is nil (the complex claim missed), falls back to locating
// the bin at order.ProcessNode/SourceNode — the ALN_002 incident path.
func (d *Dispatcher) syncManifestForRelease(env *protocol.Envelope, order *orders.Order, p *protocol.OrderRelease) error {
	if p.Disposition != nil {
		var binIDForAudit int64
		if order.BinID != nil {
			binIDForAudit = *order.BinID
		} else if id, ok := d.findFallbackBinAtSource(order); ok {
			binIDForAudit = id
		}
		if binIDForAudit != 0 {
			if err := d.binManifest.AuditReleaseOverride(binIDForAudit, order.ID, p.Disposition, p.CalledBy); err != nil {
				log.Printf("dispatch: release override audit for order %d (bin %d): %v",
					order.ID, binIDForAudit, err)
			}
		}
	}

	if p.RemainingUOP == nil {
		return nil
	}

	var kind protocol.UOPDispositionKind
	if p.Disposition != nil {
		kind = p.Disposition.Kind
	}

	if order.BinID != nil {
		if err := d.binManifest.SyncOrClearForReleased(*order.BinID, order.ID, p.RemainingUOP, kind, p.CalledBy); err != nil {
			log.Printf("dispatch: manifest sync on release for order %d: %v", order.ID, err)
			d.sendError(env, p.OrderUUID, "manifest_sync_failed", err.Error())
			return err
		}
		return nil
	}

	if order.ProcessNode == "" && order.SourceNode == "" {
		log.Printf("dispatch: release for order %d had nil BinID and no ProcessNode/SourceNode — manifest will not clear",
			order.ID)
		return nil
	}

	fallbackLookup := order.ProcessNode
	if fallbackLookup == "" {
		fallbackLookup = order.SourceNode
	}

	binID, ok := d.findFallbackBinAtSource(order)
	if !ok {
		log.Printf("dispatch: release for order %d had nil BinID and no fallback bin at %s — manifest will not clear",
			order.ID, fallbackLookup)
		return nil
	}

	log.Printf("dispatch: release for order %d had nil BinID; fallback located bin %d at %s",
		order.ID, binID, fallbackLookup)

	if err := d.binManifest.SyncOrClearForReleasedNoOwner(binID, order.ID, p.RemainingUOP, p.CalledBy); err != nil {
		log.Printf("dispatch: fallback manifest sync on release for order %d (bin %d): %v", order.ID, binID, err)
		d.sendError(env, p.OrderUUID, "manifest_sync_failed", err.Error())
		return err
	}
	return nil
}

// patchRedirectSegments replaces the last dropoff node in the segment with
// the order's current DeliveryNode when the order was redirected while staged.
// Only patches the final segment — intermediate segments have legitimate
// dropoffs that differ from the final destination.
func (d *Dispatcher) patchRedirectSegments(segment []resolvedStep, order *orders.Order, moreWaits bool) {
	if order.DeliveryNode == "" || moreWaits {
		return
	}
	for i := len(segment) - 1; i >= 0; i-- {
		if segment[i].Action == protocol.ActionDropoff {
			if segment[i].Node != order.DeliveryNode {
				d.dbg("complex release: patching segment dropoff %s -> %s (redirect)", segment[i].Node, order.DeliveryNode)
				segment[i].Node = order.DeliveryNode
			}
			break
		}
	}
}

// dispatchFleetRelease converts the segment to fleet blocks, submits them to
// the fleet backend, advances the wait index, and transitions the order
// lifecycle. Called after manifest sync and segment extraction have succeeded.
//
// The fleet half is appendSegmentAndAdvance (shared with the lane-gate path);
// this wrapper only adds the operator-facing error reply, which is the one thing
// the two callers genuinely differ on.
// HardReleaseStagedOrder advances a dwelling order's next segment REGARDLESS of
// who owns its wait. It is the Core operator's escape hatch (W3).
//
// ── IT BYPASSES THE FENCE ON PURPOSE, WHICH IS THE WHOLE AFFORDANCE ───────
//
// HandleOrderRelease refuses a gate-staged order because only the lane evaluator
// can know a lane is safe, and the station HMI must not offer a button for a
// wait it does not own. Both are right, and together they leave a hole: when the
// mechanism that WOULD advance a wait is itself wedged, nobody can advance it.
// This stream met that hole three times — a resume that never reached the Edge,
// a mirror stuck behind the authority, a heal that could never take its lane —
// and each time the only recovery was a person with database access.
//
// So the hatch is explicit, audited, and Core-side. It is NOT a new privilege
// class: it sits behind the same protected route group as order termination and
// the robot force-complete, which are the comparable "an engineer has decided"
// verbs. The audit row names the actor and the wait's owner, because a hard
// release of a STATION wait is a person overriding a cell that may still be
// occupied, and that is worth being able to find afterwards.
//
// It does NOT skip the fleet append or the ordering discipline: it goes through
// appendSegmentAndAdvance like both ordinary releasers, so the vehicle pin, the
// success-before-advance rule and the retry semantics are identical. The only
// thing it skips is the question of whose turn it is.
func (d *Dispatcher) HardReleaseStagedOrder(orderID int64, actor string) error {
	order, err := d.db.GetOrder(orderID)
	if err != nil || order == nil {
		return fmt.Errorf("hard release: order %d not found: %v", orderID, err)
	}
	if order.Status != StatusStaged && order.Status != StatusInTransit {
		return fmt.Errorf("hard release: order %d is %s — only a staged or in_transit order has a "+
			"segment left to append", orderID, order.Status)
	}
	var steps []resolvedStep
	if uErr := json.Unmarshal([]byte(order.StepsJSON), &steps); uErr != nil {
		return fmt.Errorf("hard release: order %d has an unreadable plan: %w", orderID, uErr)
	}
	segment, moreWaits, blockOffset := splitSegment(steps, order.WaitIndex)
	if segment == nil {
		return fmt.Errorf("hard release: order %d has no segment at wait_index %d — it is past its "+
			"final wait and there is nothing left to release", orderID, order.WaitIndex)
	}

	// ── CORE-OWNED WAITS ONLY, ENFORCED HERE AND NOT JUST IN THE UI ───────
	//
	// A STATION-owned wait is released from the station's board, by the person
	// who can see whether the cell is clear. A Core-side override for one would
	// let an engineer advance a robot into an occupied cell from a screen that
	// cannot show them the cell — and would do it in the one case where the
	// ordinary path is not broken at all. The hatch exists for the waits CORE is
	// responsible for advancing, whose mechanism can wedge with nobody else able
	// to clear it.
	//
	// An untagged wait (pre-ruling plan, still draining) reads as the station's
	// and is refused, which is the conservative direction.
	w, ok := waitAt(steps, order.WaitIndex)
	if !ok {
		return fmt.Errorf("hard release: order %d is not parked at a wait", orderID)
	}
	if IsStationWait(w.WaitKind) {
		return fmt.Errorf("hard release: order %d is parked at a STATION-owned wait at %q — release "+
			"it from the station's board, where the operator can see whether the cell is clear. "+
			"Core's hard release is for waits Core is responsible for advancing", orderID, w.Node)
	}
	owner := w.WaitKind

	// ── WHAT IT IS OVERRIDING, MEASURED AND RECORDED — NOT ASSUMED SAFE ───
	//
	// Skipping the OWNERSHIP fence is the affordance. Skipping the PHYSICAL
	// questions would be a different thing entirely, and doing it silently would
	// be indefensible: a lane can be dig-locked or hold another robot, and this
	// appends a segment that drives into it.
	//
	// It still proceeds — refusing on a busy lane would remove the hatch in
	// exactly the case it is needed, which is a wedge in the lane machinery
	// itself. So the physical verdict is READ and carried into the audit and the
	// log instead: the operator is overriding something specific, and afterwards
	// anyone can see what it was. Informed override, not a blind one.
	physical := "not evaluated (no lane on this entry)"
	if v, vErr := d.hardReleasePhysicalVerdict(order, steps); vErr != nil {
		physical = "COULD NOT BE READ: " + vErr.Error()
	} else if v != "" {
		physical = "REFUSED BY: " + v
	} else if v == "" {
		physical = "clear"
	}

	d.db.AppendAudit("order", orderID, "hard_release", "",
		fmt.Sprintf("HARD RELEASE by %s: advanced past a %s-owned wait at wait_index %d. Core's "+
			"fence and the station's board were both bypassed. Physical state at the moment of "+
			"release: %s — if this was a station wait, a cell may still have been occupied",
			actor, owner, order.WaitIndex, physical), actor)
	log.Printf("HARD RELEASE: order %d advanced past a %s-owned wait by %s (physical: %s) — the "+
		"ordinary releaser for that wait did not run, which is itself worth investigating",
		orderID, owner, actor, physical)

	d.patchRedirectSegments(segment, order, moreWaits)
	return d.appendSegmentAndAdvance(order, segment, moreWaits, blockOffset, "hard release")
}

// hardReleasePhysicalVerdict asks the ordinary admission question about the
// entry this release is about to make, purely so the override can be recorded
// against it. Returns the refusing cause (empty when the lane is clear), or an
// error when the answer could not be read.
//
// It is READ-ONLY and its answer never blocks: see the note at the call site for
// why a hatch that refuses on a busy lane is not a hatch. Deliberately reuses
// the gate's own classifier rather than a second opinion — the point is to
// record what the REAL fence would have said.
func (d *Dispatcher) hardReleasePhysicalVerdict(order *orders.Order, steps []resolvedStep) (string, error) {
	entry, _, isRetrieve, ok := laneEntryAfterWait(steps, order.WaitIndex)
	if !ok || entry.Node == "" {
		return "", nil // nothing lane-shaped after this wait
	}
	node, err := d.db.GetNodeByDotName(entry.Node)
	if err != nil || node == nil {
		return "", fmt.Errorf("entry node %q does not resolve", entry.Node)
	}
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil {
		return "", err
	}
	if lane == nil {
		return "", nil // not a lane entry; nothing physical to override
	}
	v, err := d.gateEntryVerdict(lane, order, node, isRetrieve)
	if err != nil {
		return "", err
	}
	if v.Admitted() {
		return "", nil
	}
	return fmt.Sprintf("%s at %s", v.Cause(), lane.Name), nil
}

// AppendLandedError marks a failure that happened AFTER the fleet took the
// blocks. The append is the one irreversible step in a release: past it the
// robot has the segment and is driving it, whatever Core failed to write next.
//
// It exists because "the release did not complete" is two facts with opposite
// rollbacks. When the append never landed, the robot got nothing, every row the
// call took must go back, and the same segment can be retried. When the append
// DID land, the robot is moving — dropping the occupancy row it drove into
// would declare an occupied corridor empty (§R.54's phantom row, inverted), and
// retrying the segment would append it twice.
//
// Both are errors. Neither is success. The rollback arms ask which one.
type AppendLandedError struct{ err error }

func (e AppendLandedError) Error() string { return e.err.Error() }
func (e AppendLandedError) Unwrap() error { return e.err }

// IsAppendLanded reports whether the fleet already has the blocks — i.e. the
// failure is downstream of the irreversible step and nothing may be rolled back.
func IsAppendLanded(err error) bool {
	var a AppendLandedError
	return errors.As(err, &a)
}

func appendLanded(err error) error { return AppendLandedError{err: err} }

func (d *Dispatcher) dispatchFleetRelease(env *protocol.Envelope, order *orders.Order, segment []resolvedStep, moreWaits bool, blockOffset int) {
	if err := d.appendSegmentAndAdvance(order, segment, moreWaits, blockOffset, "complex release"); err != nil {
		d.sendError(env, order.EdgeUUID, "fleet_failed", err.Error())
	}
}

// appendSegmentAndAdvance is the ONE fleet-append path: convert a segment to
// blocks, append them to the order's live (unsealed) waybill, and — only on
// success — advance wait_index and take the staged→in_transit transition.
//
// Both releasers go through here: the OPERATOR release (dispatchFleetRelease,
// driven by protocol.OrderRelease from Edge) and the LANE GATE release (Core
// deciding a lane is safe to enter). They differ only in who pushes the button
// and how a failure is reported, so the append itself must not be written twice:
// the vehicle pin (seerrds Adapter.ReleaseOrder reads the assigned vehicle and
// pins it so RDS does not re-dispatch to a different robot), the
// success-before-advance ordering, and the IsIllegalTransition tolerance are
// three independently-learned behaviors, and a parallel implementation would
// lose one of them quietly.
//
// ORDERING IS LOAD-BEARING. wait_index and the lifecycle transition move ONLY
// after ReleaseOrder returns nil. On failure the order keeps wait_index and its
// staged status, so the caller can retry the same segment — the retry is what
// makes a transient fleet error survivable rather than a stranded robot.
//
// ── AND IT FAILS CLOSED ON BOTH SIDES OF THE APPEND (§R.98 stage A3) ──────
//
// wait_index is the DURABLE WITNESS every release path re-reads to decide
// whether a tail is still owed (IsGateStaged). Two ways it can lie, and neither
// one used to be reported:
//
//   - The append was never proven and the witness advanced anyway. That was the
//     backend's fault, not this function's: a fleet that returns nil for a
//     mission it never issued makes an unproven append indistinguishable from a
//     proven one. Fixed at both backends; this side only has to keep believing
//     the answer.
//   - The append landed and the witness did NOT advance, or the order could not
//     be put in transit afterwards. Both were logged and returned success. A
//     release that could not move the order out of staging has not completed,
//     and saying it did is how a vanished mission got recorded as a good final
//     append. They now return AppendLandedError — an error, carrying the fact
//     that the robot has the blocks so no caller rolls back a corridor the
//     robot is already inside.
//
// Segments are not load-sequence expanded (nil): F4c is scoped to the simple
// transport path's initial pickup, and an appended segment never carries one.
func (d *Dispatcher) appendSegmentAndAdvance(order *orders.Order, segment []resolvedStep, moreWaits bool, blockOffset int, what string) error {
	blocks := stepsToBlocks(order.VendorOrderID, segment, blockOffset, nil)
	complete := !moreWaits

	d.dbg("%s: order=%d vendor=%s wait_index=%d adding %d blocks complete=%v",
		what, order.ID, order.VendorOrderID, order.WaitIndex, len(blocks), complete)

	if err := d.backend.ReleaseOrder(order.VendorOrderID, blocks, complete); err != nil {
		log.Printf("dispatch: %s: fleet append for order %d failed: %v", what, order.ID, err)
		return err
	}

	// PAST THIS LINE THE ROBOT HAS THE SEGMENT. Nothing below can be undone.
	newWaitIndex := order.WaitIndex + 1
	if err := d.db.UpdateOrderWaitIndex(order.ID, newWaitIndex); err != nil {
		log.Printf("dispatch: update order %d wait_index to %d: %v", order.ID, newWaitIndex, err)
		return appendLanded(fmt.Errorf("%s: order %d: the fleet took the segment but wait_index did not advance to %d — the row still says a tail is owed: %w",
			what, order.ID, newWaitIndex, err))
	}

	// Release BEFORE the in-memory wait_index bump: its audit reason renders
	// ord.WaitIndex as "released from staging (wait N)", where N names the wait
	// being LEFT. Bumping first would silently re-number every such audit line.
	if err := d.lifecycle.Release(order, "dispatcher"); err != nil {
		// ── ALREADY THERE IS NOT A FAILURE, AND IT IS NOT THE SAME AS UN-RELEASABLE ──
		//
		// A second append on one order — a gated ENTRY followed by the dwell's own
		// release, the composed shape — finds the order already in transit, because
		// the entry put it there. The state machine has no self-edge, so a perfectly
		// healthy idempotent release surfaces as `in_transit → in_transit`.
		//
		// That is why this arm was tolerant, and the tolerance was right for this
		// case and wrong for every other one: cancelled, failed and delivered came
		// through the same branch and were reported as successful releases. The
		// discriminator is not the error class, it is whether the order is ALREADY
		// where the transition was going. When it is, the postcondition holds and
		// the release completed. When it is not, the order became un-releasable
		// mid-flight and this did not complete.
		var it IllegalTransition
		if !errors.As(err, &it) || it.From != it.To {
			// The witness advanced, so keep the caller's struct matching the row it
			// wrote even on the way out — the gate path re-reads it.
			order.WaitIndex = newWaitIndex
			if IsIllegalTransition(err) {
				log.Printf("dispatch: order %d became un-releasable mid-flight (status=%s): %v", order.ID, order.Status, err)
				return appendLanded(fmt.Errorf("%s: order %d: the fleet took the segment but the order was un-releasable (status=%s): %w",
					what, order.ID, order.Status, err))
			}
			log.Printf("dispatch: release order %d from staging: %v", order.ID, err)
			return appendLanded(fmt.Errorf("%s: order %d: the fleet took the segment but the order could not be released from staging: %w",
				what, order.ID, err))
		}
		d.dbg("%s: order %d was already %s when its segment was appended — the release is idempotent, not refused",
			what, order.ID, it.From)
	}
	log.Printf("dispatch: %s: order %d appended %d blocks (wait %d, complete=%v)",
		what, order.ID, len(blocks), order.WaitIndex, complete)
	// Keep the caller's struct consistent with the row it just wrote — the gate
	// path re-reads it to decide whether an order is still awaiting its tail.
	order.WaitIndex = newWaitIndex
	return nil
}

// findFallbackBinAtSource locates a bin to manifest-sync when the
// caller's order.BinID is nil at release time. Returns (binID, true)
// on success.
//
// Lookup order:
//
//  1. **Claim-first** (Phase 3 of bin-transit-state): query bins where
//     claimed_by = order.ID. The claim is the canonical "this order's
//     bin(s)" pointer, independent of where the bin physically sits.
//     Critical under transit semantics — a bin mid-flight has
//     node_id=_TRANSIT, not its original source, so a node-only lookup
//     would miss it. Multi-bin orders may return several rows; if
//     ProcessNode is set we prefer the bin currently at the line node
//     (the operator's release target), else the first by ID.
//
//  2. **Node fallback**: search bins physically at ProcessNode (the
//     line) or SourceNode (the first pickup) for orders without an
//     active claim — pre-existing behavior. Selects payload-matching
//     bin first, then any non-empty bin at the node.
//
// Pre-Phase-3 this was node-only and would silently miss bins that
// the complex claim HAD claimed but UpdateOrderBinID failed to persist
// (DB-write race), and miss any in-transit bin during the rare case
// where release fires after pickup has already happened.
func (d *Dispatcher) findFallbackBinAtSource(order *orders.Order) (int64, bool) {
	// 1) Claim-first.
	claimed, err := d.db.ListBinsByClaim(order.ID)
	if err == nil && len(claimed) > 0 {
		// Multi-bin orders: prefer the bin at ProcessNode (the line —
		// where the operator's release intent applies). Falls back to
		// the first by ID if no per-line preference resolves.
		if order.ProcessNode != "" && len(claimed) > 1 {
			if procNode, perr := d.db.GetNodeByDotName(order.ProcessNode); perr == nil && procNode != nil {
				for _, b := range claimed {
					if b.NodeID != nil && *b.NodeID == procNode.ID {
						return b.ID, true
					}
				}
			}
		}
		return claimed[0].ID, true
	}

	// 2) Node fallback — only reached when no bin is claimed by this
	// order at all (the complex claim missed entirely, or order is in
	// a partial-state we can't reason about from claims).
	lookupNode := order.ProcessNode
	if lookupNode == "" {
		lookupNode = order.SourceNode
	}
	srcNode, err := d.db.GetNodeByDotName(lookupNode)
	if err != nil || srcNode == nil {
		return 0, false
	}
	bins, err := d.db.ListBinsByNode(srcNode.ID)
	if err != nil || len(bins) == 0 {
		return 0, false
	}
	// Prefer a payload-matching bin (correct in the multi-bin storage case).
	if order.PayloadCode != "" {
		for _, b := range bins {
			if b.PayloadCode == order.PayloadCode {
				return b.ID, true
			}
		}
	}
	// No payload match — fall back to the first bin with a non-empty
	// manifest. Skip already-cleared bins to avoid double-clearing a
	// stale empty.
	for _, b := range bins {
		if b.PayloadCode != "" {
			return b.ID, true
		}
	}
	return 0, false
}
