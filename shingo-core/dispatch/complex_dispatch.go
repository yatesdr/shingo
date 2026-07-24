package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store"
	"shingocore/store/orders"
)

// isConcreteStorageDropoff reports whether a delivery node is a concrete
// (non-synthetic) STORAGE/STAGING slot — a direct child of a LANE or NGRP.
// This is the role gate for the complex dropoff-capacity check (#1): such a
// slot must queue-on-full, whereas a LINE/production dropoff must NOT be
// gated (a two-robot supply leg delivers to a line a sibling evac clears, and
// gating it deadlocks). Mirrors engine.isStorageSlot's parent-type rule minus
// the synthetic-root cases — NGRP/LANE dropoffs are handled by step
// re-resolution / ResolutionCapacity before this point.
// (Free function — shared by the Dispatcher's dropoff-capacity gate and the
// Allocator's slotNeeds; it needs only the store handle.)
func isConcreteStorageDropoff(db *store.DB, deliveryNode string) bool {
	if deliveryNode == "" {
		return false
	}
	node, err := db.GetNodeByDotName(deliveryNode)
	if err != nil || node == nil || node.IsSynthetic || node.ParentID == nil {
		return false
	}
	parent, err := db.GetNode(*node.ParentID)
	if err != nil || parent == nil {
		return false
	}
	return parent.NodeTypeCode == protocol.NodeClassLANE || parent.NodeTypeCode == protocol.NodeClassNGRP
}

// DispatchPreparedComplex performs the side-effecting tail of complex-
// order dispatch: claim bins per pickup step, transition the order
// queued → sourcing, send blocks to the fleet, transition → dispatched.
//
// Idempotent prerequisites: the order must have StepsJSON populated
// (intake side stores it on creation) and be in StatusQueued. Caller
// is responsible for the capacity gate — this method assumes green-
// light and proceeds with the atomic claim + dispatch.
//
// Called from:
//   - fulfillment.Scanner.tryFulfill on EventOrderQueued (fresh intake
//     just called HandleComplexOrderRequest)
//   - fulfillment.Scanner.tryFulfill on EventBinUpdated /
//     EventBinEnteredTransit / EventOrderCompleted etc. (slot vacancy
//     unblocks a previously-blocked order)
//
// Errors land on lifecycle.Fail — the order moves to terminal `failed`
// rather than back to queued, since these are unrecoverable from the
// scanner's perspective (steps unparseable, bins unavailable, fleet
// rejects).
func (d *Dispatcher) DispatchPreparedComplex(order *orders.Order) error {
	// Defense-in-depth: the fulfillment scanner's tryFulfill already gates on
	// IsAcquiring ({queued, sourcing}) before calling here, so a parent in
	// Reshuffling (with a compound in flight), or one already dispatched or
	// terminal, won't reach us through the scanner. Anything calling this
	// directly (engine recovery, future call sites) must still respect the
	// invariant — proceeding on a non-acquiring order would re-dispatch a parent
	// mid-reshuffle or race a post-resume. The acquiring set was widened
	// from queued-only to {queued, sourcing} so a complex order that reached
	// `sourcing` but didn't finish dispatching is retried. Return nil so the caller treats a
	// non-acquiring order as a no-op, not an error.
	if !protocol.IsAcquiring(order.Status) {
		d.dbg("complex: DispatchPreparedComplex called with status=%s (want queued/sourcing); skipping", order.Status)
		return nil
	}

	var resolvedSteps []resolvedStep
	if err := json.Unmarshal([]byte(order.StepsJSON), &resolvedSteps); err != nil {
		d.failOrderInternal(order, "invalid_steps", fmt.Sprintf("parse stored steps: %v", err))
		return err
	}

	// Round-3 follow-up: re-resolve any step that still references an
	// NGRP. This happens on the deferred path — intake queued the order
	// because the NGRP was saturated; the scanner replays after slot
	// vacancy events, and we attempt resolution again here. On capacity
	// failure, set queue_reason to the current resolver message and
	// stay queued (don't fail). On other resolver errors, fail with
	// invalid_steps. On success, persist the locked-in concrete-child
	// names so subsequent ticks don't redo the work.
	newSteps, changed, rerr := d.reResolveComplexSteps(resolvedSteps, order.PayloadCode)
	if rerr != nil {
		class, payload := classifyResolutionError(rerr)
		switch class {
		case ResolutionBuried:
			// Multi-burial scenario: a second-or-later step in the
			// order hit a burial after the first compound completed.
			// Same planner the intake path uses.
			buriedErr := payload.(*BuriedError)
			d.dbg("complex: order %d buried at replay — bin %d in lane %d", order.ID, buriedErr.Bin.ID, buriedErr.LaneID)
			d.handleComplexBuriedOnReplay(order, buriedErr)
			return rerr
		case ResolutionCapacity:
			capDetail := capacityDetailFrom(payload)
			code := queueCodeForCapacity(capDetail.kindOf())
			d.setQueueReason(order, code, "ngrp-resolve",
				queueParamsForCapacity(capDetail, order.PayloadCode, order.DeliveryNode))
			d.dbg("complex: order %d still capacity-blocked at NGRP resolution: %s", order.ID, code)
			return rerr
		default:
			d.failOrderInternal(order, "invalid_steps", rerr.Error())
			return rerr
		}
	}

	// C(ii): supply-pickup widening. Every full-material pickup — never an
	// evac/removal leg (isRemovalPickup splits those off; they keep today's
	// reserve/moot path byte-unchanged) — is re-anchored through the
	// node-local finder each tick: material sitting on a sibling position in
	// the anchor's pool rewrites the step there; a dry anchor PARKS the order
	// as waiting_for_material instead of letting it fail terminal downstream.
	// The park is the disposition change: a dry supply need is escapable
	// (operator abandon) rather than a cancel→autoreorder loop.
	widened, wchanged, hold := d.widenSupplyPickups(order, newSteps)
	newSteps = widened
	changed = changed || wchanged

	if changed {
		stepsJSON, mErr := json.Marshal(newSteps)
		if mErr == nil {
			if uErr := d.db.UpdateOrderStepsJSON(order.ID, string(stepsJSON)); uErr != nil {
				log.Printf("dispatch: update steps_json for complex order %d: %v", order.ID, uErr)
			} else {
				order.StepsJSON = string(stepsJSON)
			}
		}
		// Endpoints may have shifted (NGRP→child). Re-extract and persist
		// so handler-side lookups (process_node lookup, source/delivery
		// rendering) reflect the resolved choice.
		newSource, newDelivery := extractEndpoints(newSteps)
		if newSource != order.SourceNode {
			if err := d.db.UpdateOrderSourceNode(order.ID, newSource); err != nil {
				log.Printf("dispatch: update source_node for complex order %d: %v", order.ID, err)
			} else {
				order.SourceNode = newSource
			}
		}
		if newDelivery != order.DeliveryNode {
			if err := d.db.UpdateOrderDeliveryNode(order.ID, newDelivery); err != nil {
				log.Printf("dispatch: update delivery_node for complex order %d: %v", order.ID, err)
			} else {
				order.DeliveryNode = newDelivery
			}
		}
	}
	resolvedSteps = newSteps

	// A blocked supply need surfaces AFTER the persist block so partial
	// rewrites (steps widened before the blocked one) are already durable;
	// the next tick re-derives every anchor from the persisted Group stamp.
	if hold != nil {
		switch MapFinderOutcome(*hold) {
		case OutcomeWait:
			d.setQueueReason(order, hold.QueueCode, hold.QueueCause, hold.QueueParams)
			d.dbg("complex: order %d supply pickup waiting for material (%s)", order.ID, hold.QueueCause)
			return fmt.Errorf("supply pickup waiting for material: %s", hold.QueueCause)
		case OutcomeReshuffle:
			d.dbg("complex: order %d supply pickup buried at widen — bin %d in lane %d",
				order.ID, hold.Buried.Bin.ID, hold.Buried.LaneID)
			d.handleComplexBuriedOnReplay(order, hold.Buried)
			return fmt.Errorf("supply pickup buried; reshuffle planned")
		case OutcomeFound:
			// Impossible: widenSupplyPickups only holds non-Found results.
			// Fail structurally rather than silently dispatching a plan whose
			// widening never finished.
			fallthrough
		default: // OutcomeStructural — TermCode/Err may be unset on the
			// loud-degrade path out of MapFinderOutcome.
			code := hold.TermCode
			if code == "" {
				code = codeNoBin
			}
			detail := "supply widening failed structurally"
			if hold.Err != nil {
				detail = hold.Err.Error()
			}
			d.failOrderInternal(order, code, detail)
			return fmt.Errorf("supply pickup structural: %s", detail)
		}
	}

	// Dedicated home loader PARK: when this is a changeover return from a
	// dedicated-loader home (order.SourceNode = the evac pickup), Core decides where
	// the bin lands — HOME if free, else a buffer slot, else drain — and rewrites
	// DeliveryNode. The Edge shipped DeliveryNode="" and named no target, so Core is
	// the single authority; the release-time redirect overlay (patchRedirectSegments)
	// carries the choice to the fleet. A non-dedicated / non-loader source is left
	// untouched (drains as today). NOT a dispatch gate (no isConcreteStorageDropoff
	// widening) — a resolution-time read, so the swap supply leg is never gated.
	d.placeForDedicatedLoader(order, resolvedSteps)

	// Close the swap peer-terminal RACE (SPR 2424/2425, 2026-07). HandleSwapPeerTerminal
	// unwinds a swap when one leg reaches a terminal state, but it fires from the
	// DEAD leg's side — so if this leg did not exist yet when its sibling died (a
	// supply created + skipped moot in the same tick, before its evac was created),
	// that unwind found no peer and no-op'd, leaving this leg to hold forever on a
	// dead sibling (swapLegHeld waits on a claim that will never come). Re-run the
	// unwind now, from the surviving side, so a leg linked to an already-terminal
	// sibling is resolved instead of wedged. Reuses the same handler and its
	// per-role resolution: a moot-evac sibling that legitimately lets this supply
	// proceed is a no-op there, so this leg falls through and dispatches.
	if sibUUID, serr := d.db.OrderSiblingUUID(order.ID); serr == nil && sibUUID != "" {
		if sib, gerr := d.db.GetOrderByUUID(sibUUID); gerr == nil && sib != nil && protocol.IsTerminal(sib.Status) {
			if kind := swapTerminalKind(sib.Status); kind != "" {
				// Heal the dead leg's back-link first (idempotent). The unwind
				// resolves the peer FROM the dead leg's side, and the race is
				// precisely that this link may not have existed when the dead leg
				// first went terminal — so ensure it does now.
				if sib.SiblingOrderUUID != order.EdgeUUID {
					if _, rerr := d.db.LinkOrderSiblingsByEdgeUUID(order.EdgeUUID, sibUUID); rerr != nil {
						log.Printf("dispatch: swap race back-link repair order %d sib %s: %v", order.ID, sibUUID, rerr)
					}
				}
				d.HandleSwapPeerTerminal(sib.ID, kind)
				if self, rerr := d.db.GetOrder(order.ID); rerr == nil && self != nil && protocol.IsTerminal(self.Status) {
					return fmt.Errorf("complex order %d resolved by swap peer-terminal unwind: sibling %d already %s", order.ID, sib.ID, sib.Status)
				}
			}
		}
	}

	// Two-robot swap removal-leg hold: don't let a removal (evac) leg that
	// cannot fetch its own replacement claim/pull the line bin until its supply
	// sibling has secured one. Stops a swap from stranding the line when the
	// supermarket is empty (ALN_003 swap-starvation, 2026-06-03). Stay
	// queued — the scanner replays on EventBinUpdated when the supply leg
	// claims, clearing the gate. The sibling pointer is set at intake (the
	// second leg carries it on its ComplexOrderRequest), so it is present
	// here even on the synchronous intake-dispatch path.
	//
	// Reads the RESOLVED steps, not the raw ones: NGRP names have been resolved
	// to concrete nodes by now, and the line node is concrete either way, so the
	// pickup/dropoff shape the gate depends on is stable across resolution.
	if held, reason := d.swapLegHeld(order, resolvedSteps); held {
		d.setQueueReason(order, protocol.QueueWaitingForPartner, "swap-hold", QueueParams{Sibling: order.SiblingOrderUUID})
		d.dbg("complex: order %d held — %s", order.ID, reason)
		return fmt.Errorf("swap hold: %s", reason)
	}

	// #1 (regression 2b05dce): restore the dropoff-capacity gate for complex
	// orders, but ONLY for concrete STORAGE/STAGING dropoffs. The scanner
	// dropped the gate for every complex order to unstick two-robot SUPPLY
	// legs — which deliver to a LINE node a sibling EVAC clears, and Core has
	// no SiblingOrderID to model that — but that also let a changeover
	// drop/evac to a FULL concrete storage slot dispatch into the occupied
	// slot. Gate by node role (storage slot = child of LANE/NGRP), NOT by
	// same-order pickup: gating the line case would re-create the deadlock
	// 2b05dce fixed. NGRP dropoffs are already covered above by
	// reResolveComplexSteps / ResolutionCapacity. Stay queued by returning an
	// error — the scanner keeps the order queued and replays it on the next
	// slot-vacancy tick (same contract as the claim_failed branch below).
	if isConcreteStorageDropoff(d.db, order.DeliveryNode) {
		if blocked, cap := CheckDropoffCapacity(d.db, order.DeliveryNode, order.ID); blocked {
			d.setQueueReason(order, protocol.QueueWaitingForSlot, "dropoff-capacity", cap.Params)
			d.dbg("complex: order %d queued — concrete storage dropoff %s blocked: %s", order.ID, order.DeliveryNode, cap.Cause)
			return fmt.Errorf("dropoff capacity: %s", cap.Cause)
		}
	}

	// Reserve each concrete storage drop-off SLOT (the destination dual of the bin
	// reserve) — the reservation-native replacement for the retired hard-claim slot
	// loop (the split-brain fix). An incomplete order now holds its slots as
	// revocable RESERVATIONS across ticks, NOT hard nodes.claimed_by. Runs BEFORE the
	// bin reserve (slots-before-bins + the relay rule: a slot must be held before
	// the bin leg reads its emptiness). A fungible NGRP slot conflict
	// reverts-and-re-resolves (the escape valve, preserved); a fixed-concrete
	// conflict holds (Wait) — both requeue in the order's entry status (queued
	// first pass, sourcing on retry).
	//
	// The canonical node-ID sort is gone WITH the loop: the ABBA class dissolves at
	// the soft-acquire layer, where a loser backs off holding only revocable slot
	// reservations, not a hard claim. Removing the loop and its insurance together
	// honors the rule that the slot-ordering must not be reverted without restoring
	// a sweep for slot-wedged orders.
	if slotOutcome, serr := d.allocator.reserveComplexSlots(order, resolvedSteps); serr != nil {
		log.Printf("dispatch: complex order %d slot reserve error: %v", order.ID, serr)
		return serr
	} else if slotOutcome != reserveComplete {
		d.setQueueReason(order, protocol.QueueWaitingForSlot, "slot-reserve", QueueParams{Destination: order.DeliveryNode})
		d.dbg("complex: order %d held — incomplete slot reserve, retrying next tick", order.ID)
		return fmt.Errorf("complex order %d slot reserve incomplete", order.ID)
	}

	// Reserve/confirm. MoveToSourcing at the START of the reserve attempt: the
	// order stays `sourcing` while it holds partials and the scanner retries it
	// (the acquiring-set widening, complex scope). Idempotent — a retried order
	// re-enters sourcing→sourcing every tick, which MoveToSourcing skips. The gates
	// above (swap-hold, capacity, slot-claim) run first and park a blocked order in
	// its entry status (queued first pass, sourcing on retry); both are retried by
	// the complex-scoped scanner, and each wrote queue_reason for the Edge push.
	if err := d.lifecycle.MoveToSourcing(order, "scanner", "reserving source bins"); err != nil {
		// Refused CAS = another actor terminalized or moved this order while we
		// held a stale snapshot. Everything below reserves bins and ends in a
		// fleet dispatch, so yield rather than commit robots for an order that
		// is no longer ours.
		if IsConcurrentTransition(err) {
			log.Printf("dispatch: complex order %d moved under us — another actor owns it now: %v", order.ID, err)
			return fmt.Errorf("complex order %d moved concurrently: %w", order.ID, err)
		}
		log.Printf("dispatch: complex order %d → sourcing: %v", order.ID, err)
	}

	// Plan = ordering + intent. RemainingUOP is nil at complex intake (Edge threads
	// it at release, not intake). The plan's predicted bins are advisory; reserve and
	// confirm select/claim against live state, keyed on the plan's distinct needs.
	processNode := order.ProcessNode
	if processNode == "" {
		processNode = order.SourceNode
	}
	plan := BuildComplexPlan(resolvedSteps, d.snapshotPickupBins(resolvedSteps), order.PayloadCode, processNode)

	// Reserve = reconcile held reservations against the distinct source needs and
	// soft-hold the gaps (reserveComplexPlan). Runs AFTER the slot-claim loop above,
	// never interleaved with it (slots-before-bins) — one claim class fully ordered
	// before the next is what prevents a slot↔bin cross-type deadlock cycle. Dispatch
	// is gated on a COMPLETE distinct-bin set (the relay rule): an incomplete order
	// holds its partials and stays `sourcing` for the scanner to retry — a robot never
	// starts a job it can't finish, and give-up is operator-driven, never a timer.
	// There is no orphaned-hold window now: the order is already `sourcing` before it
	// holds anything, so a crash leaves a `sourcing` order whose pending holds the
	// owner-liveness reaper reclaims — not a `queued` order stranded with claimed bins.
	assigned, outcome, rerr := d.allocator.reserveComplexPlan(order, plan)
	if rerr != nil {
		log.Printf("dispatch: complex order %d reserve error: %v", order.ID, rerr)
		return rerr
	}
	switch outcome {
	case reserveMoot:
		// Reserved nothing and every source node is empty — the work is void (e.g. a
		// swap evac whose line bin was removed to quality hold before dispatch). Skip
		// so Edge's HandleOrderSkipped advances the linked changeover task, rather
		// than hold forever: a moot evac is not demand (operator-driven hold-and-retry
		// does not apply).
		d.skipOrderInternal(order, codeNoSourceBin, fmt.Sprintf("complex order %d: no bin at any source node", order.ID))
		return fmt.Errorf("complex order %d moot — skipped", order.ID)
	case reserveHolding:
		// Only claim "partial set already held" when the order actually holds part
		// of its set. Holding NOTHING (zero reserved, blocked on every need) is a
		// different operator situation and must not render as a partial hold — that
		// was the SPR ALN_006 lie: "sourcing / partial set already held" while the
		// order held zero reservations and made no progress.
		holdingPartials := len(assigned) > 0
		d.setQueueReason(order, protocol.QueueWaitingForMaterial, "reserve-holding",
			QueueParams{Payload: order.PayloadCode, Partial: holdingPartials})
		d.dbg("complex: order %d incomplete reserve — holding %d partial(s), retrying next tick", order.ID, len(assigned))
		return fmt.Errorf("complex order %d reserve incomplete", order.ID)
	}

	// Confirm = commit the complete reserved set to hard claims (apply-as-confirm, no
	// live re-walk). A claim_failed (a pending hold reaped, or a bin claimed by
	// another order between reserve and confirm) requeues the attempt; a malformed
	// order (no source pickup) fails.
	if cerr := d.allocator.confirmComplexPlan(order, plan, assigned); cerr != nil {
		var pe *planningError
		if errors.As(cerr, &pe) && pe.Code == codeClaimFailed {
			d.setQueueReason(order, protocol.QueueWaitingForMaterial, "claim-failed",
				QueueParams{Payload: order.PayloadCode})
			d.dbg("complex: order %d held on claim_failed: %s", order.ID, pe.Detail)
			return cerr
		}
		d.failOrderInternal(order, codeNoBin, cerr.Error())
		return cerr
	}

	preWait, hasWait := splitAtWait(resolvedSteps)
	vendorOrderID := fmt.Sprintf("%s%d-%s", VendorIDPrefix, order.ID, uuid.New().String()[:8])
	// Complex orders are not load-sequence expanded (nil): the F4c advanced load
	// sequence is scoped to the simple transport path the child-cart delivery
	// uses. Complex is "every other order kind" — byte-identical to before.
	blocks := stepsToBlocks(vendorOrderID, preWait, 0, nil)
	if len(blocks) == 0 {
		d.failOrderInternal(order, "invalid_steps", "no actionable steps before wait")
		return fmt.Errorf("no actionable blocks")
	}

	req := fleet.CreateOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   order.Priority,
		RobotGroup: d.robotGroupForPayload(order.PayloadCode),
		Complete:   false, // staged: a multi-wait complex order dwells (Complete=false) until its final segment is released
	}
	d.dbg("complex: creating staged order %s with %d initial blocks (hasWait=%v)", vendorOrderID, len(blocks), hasWait)
	if _, err := d.backend.CreateOrder(req); err != nil {
		log.Printf("dispatch: fleet create staged order failed: %v", err)
		d.failOrderInternal(order, "fleet_failed", err.Error())
		return err
	}
	if !hasWait {
		// No wait — fleet can complete the order immediately.
		if err := d.backend.ReleaseOrder(vendorOrderID, nil, true); err != nil {
			log.Printf("dispatch: fleet mark complete failed: %v", err)
		}
	}

	log.Printf("dispatch: complex order %d dispatched as %s (%d steps)", order.ID, vendorOrderID, len(resolvedSteps))
	if err := d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", ""); err != nil {
		log.Printf("dispatch: update order %d vendor: %v", order.ID, err)
	}
	if err := d.lifecycle.Dispatch(order, vendorOrderID, "scanner"); err != nil {
		log.Printf("dispatch: complex order %d → dispatched: %v", order.ID, err)
	}
	// Successful dispatch — clear any stale queue_reason from a prior
	// blocked replay attempt.
	d.setQueueReason(order, "", "", QueueParams{})
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, order.SourceNode, order.DeliveryNode)
	return nil
}

// setQueueReason is the dispatch side's one door onto the queue-reason columns.
// It generates the operator sentence from code+params (via the shared formatter),
// then writes sentence+code+cause together — so a wait parked here always records
// the structured code, never free text. No-ops when the sentence AND code are
// unchanged: the unchanged short-circuit is load-bearing (rewriting the same
// reason re-touches the row and can re-trigger the very scanner tick that just
// parked the order — an event loop). cause is the engineer-only call-site tag
// (the `where` of older callers); params carries the values the sentence is built
// from and is discarded after formatting. Best-effort: a failed write is logged
// and swallowed (queue_reason is advisory HMI/queue metadata, never a correctness
// gate), leaving the in-memory fields matching the persisted values.
func (d *Dispatcher) setQueueReason(order *orders.Order, code protocol.QueueCode, cause string, params QueueParams) {
	reason := FormatQueueSentence(code, params)
	if order.QueueReason == reason && order.QueueCode == string(code) {
		return
	}
	if err := d.db.SetOrderQueueDetail(order.ID, reason, code, cause); err != nil {
		log.Printf("dispatch: set queue_reason (%s) for order %d: %v", cause, order.ID, err)
		return
	}
	order.QueueReason = reason
	order.QueueCode = string(code)
	order.QueueCause = cause
}

// failOrderInternal is the scanner-path failure helper. Same as
// failOrder but doesn't take an envelope (no edge-bound reply — the
// edge already has the queued status from intake; it'll learn about
// the failure via EventOrderFailed → edge_handler.HandleOrderError).
func (d *Dispatcher) failOrderInternal(order *orders.Order, code, detail string) {
	if err := d.lifecycle.Fail(order, order.StationID, code, detail); err != nil {
		log.Printf("dispatch: fail order %d: %v", order.ID, err)
		// The transition failed, so its fireFailed actionMap hook did NOT emit
		// EmitOrderFailed. Fall back to an explicit emit so the failure still
		// reaches the edge. On the success path fireFailed is the single
		// authoritative emit — emitting again here would double it (the defect
		// this dedup removed).
		d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, order.StationID, code, detail)
	}
}

// skipOrderInternal is the scanner-path "the work was never needed" helper.
// Parallel shape to failOrderInternal but routes through lifecycle.Skip
// (which writes status='skipped' via SkipOrderAtomic, no anomaly mark on
// any leaked claims) and emits EventOrderSkipped. Edge subscribes via
// HandleOrderSkipped and advances the linked changeover node task without
// surfacing a failure to the operator.
func (d *Dispatcher) skipOrderInternal(order *orders.Order, code, detail string) {
	if err := d.lifecycle.Skip(order, order.StationID, code, detail); err != nil {
		log.Printf("dispatch: skip order %d: %v", order.ID, err)
	}
	d.emitter.EmitOrderSkipped(order.ID, order.EdgeUUID, order.StationID, code, detail)
}
