package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// HandleComplexOrderRequest processes a multi-step transport order from
// edge. Phase 4b of bin-transit-state moved this from "dispatch
// synchronously" to "queue-then-let-scanner-dispatch" so that the
// dropoff-capacity gate in fulfillment.Scanner.tryFulfill is the single
// sync point for capacity decisions across fresh-intake AND queue-
// replay paths. See dispatch/capacity.go for the rationale (race
// between two concurrent fresh intakes + scanner targeting the same
// dropoff would otherwise have a TOCTOU window).
//
// Flow:
//  1. Validate + resolve steps.
//  2. Create order with status=queued (was: pending + immediate dispatch).
//  3. Ack to edge.
//  4. Emit EventOrderQueued — scanner subscribes and runs immediately.
//     Scanner.tryFulfill calls Dispatcher.DispatchPreparedComplex when
//     capacity is green; leaves it queued otherwise.
//
// The latency cost on the happy path is ~milliseconds (event-driven
// scanner trigger, runs synchronously on the emitter goroutine).
// Complex orders briefly transition through `queued` status even when
// capacity is fine; consumers that only watch terminal states are
// unaffected.
func (d *Dispatcher) HandleComplexOrderRequest(env *protocol.Envelope, p *protocol.ComplexOrderRequest) {
	stationID := env.Src.Station
	d.dbg("complex order request: station=%s uuid=%s steps=%d", stationID, p.OrderUUID, len(p.Steps))

	if len(p.Steps) == 0 {
		d.sendError(env, p.OrderUUID, "invalid_steps", "complex order requires at least one step")
		return
	}

	payloadCode := p.PayloadCode

	// Resolve steps up-front so the scanner doesn't have to re-resolve
	// on the happy path (NGRP children may shift between intake and
	// dispatch — locking the choice at intake is the original
	// optimization).
	//
	// Round-3 follow-up: capacity-shaped resolution failures
	// ("no available slot in node group X", "no bin of requested
	// payload in node group X") used to terminal-reject the order at
	// intake — Edge got an error, no Core-side row created, no retry.
	// Now they create the order as queued with the resolver message as
	// queue_reason, and DispatchPreparedComplex re-resolves on each
	// scanner tick. Structural / unknown-action / unknown-node errors
	// still reject synchronously — those aren't fixable by waiting.
	resolvedSteps, err := d.resolveComplexSteps(p.Steps, payloadCode)
	var queueReason string
	if err != nil {
		class, payload := classifyResolutionError(err)
		switch class {
		case ResolutionBuried:
			// Route to the reshuffle path: create the parent at
			// Queued, pivot to Reshuffling, plan + dispatch an
			// unbury (or unbury+retrieve) compound. When the
			// compound completes the parent resumes back to Queued
			// and the fulfillment scanner runs the original first
			// pickup against the now-accessible slot.
			d.handleComplexBuriedAtIntake(env, p, payloadCode, payload.(*BuriedError))
			return
		case ResolutionCapacity:
			// Capacity-shaped — preserve the original step shape (NGRP
			// names intact) so the replay path has the input it needs
			// to re-attempt.
			resolvedSteps = stepsAsResolved(p.Steps)
			queueReason = err.Error()
		default:
			// Structural / transient / fatal — terminal at intake.
			d.sendError(env, p.OrderUUID, "resolution_failed", err.Error())
			return
		}
	}

	stepsJSON, err := json.Marshal(resolvedSteps)
	if err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to marshal steps")
		return
	}

	sourceNode, deliveryNode := extractEndpoints(resolvedSteps)

	order := &orders.Order{
		EdgeUUID:     p.OrderUUID,
		StationID:    stationID,
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued, // status-first queueing — scanner picks it up
		Quantity:     p.Quantity,
		Priority:     p.Priority,
		PayloadCode:  payloadCode,
		PayloadDesc:  p.PayloadDesc,
		SourceNode:   sourceNode,
		DeliveryNode: deliveryNode,
		ProcessNode:  p.ProcessNode,
		StepsJSON:    string(stepsJSON),
		QueueReason:  queueReason,
	}

	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create complex order: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	if queueReason != "" {
		// CreateOrder may not persist QueueReason depending on the store
		// helper's INSERT column list — set it explicitly so the field is
		// visible to the scanner's queue-reason check and to the HMI.
		if err := d.db.SetOrderQueueReason(order.ID, queueReason); err != nil {
			log.Printf("dispatch: set initial queue_reason for complex order %d: %v", order.ID, err)
		}
		log.Printf("dispatch: complex order %d queued at intake — %s", order.ID, queueReason)
	}

	// Two-robot swap pairing: the removal (evac) leg carries its supply
	// sibling's UUID. Link both rows now — before EmitOrderQueued triggers
	// the synchronous scanner — so the dispatch hold sees the pairing at the
	// removal leg's intake, ahead of the line-bin claim. The sibling row
	// already exists (supply is created first). Best-effort: a link failure
	// degrades to "no hold", it never blocks intake.
	if p.SiblingOrderUUID != "" {
		if _, err := d.db.LinkOrderSiblingsByEdgeUUID(order.EdgeUUID, p.SiblingOrderUUID); err != nil {
			log.Printf("dispatch: link complex order %d sibling %s: %v", order.ID, p.SiblingOrderUUID, err)
		}
	}
	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeComplex, payloadCode, deliveryNode)

	// Ack to edge before triggering the scanner so the edge's order-table
	// row exists when the dispatched-event fires (if scanner dispatches
	// synchronously, the edge needs to have already recorded the order ID).
	d.sendAck(env, order.EdgeUUID, order.ID, sourceNode)

	// EventOrderQueued is the scanner trigger — wired in engine/wiring.go.
	// Scanner.RunOnce is invoked synchronously on this goroutine via the
	// EventBus; if capacity is green and bins claimable, dispatch happens
	// before this function returns. Otherwise the order sits queued with
	// queue_reason set to the blocking signal.
	d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
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

// isConcreteStorageDropoff reports whether a delivery node is a concrete
// (non-synthetic) STORAGE/STAGING slot — a direct child of a LANE or NGRP.
// This is the role gate for the complex dropoff-capacity check (#1): such a
// slot must queue-on-full, whereas a LINE/production dropoff must NOT be
// gated (a two-robot supply leg delivers to a line a sibling evac clears, and
// gating it deadlocks). Mirrors engine.isStorageSlot's parent-type rule minus
// the synthetic-root cases — NGRP/LANE dropoffs are handled by step
// re-resolution / ResolutionCapacity before this point.
func (d *Dispatcher) isConcreteStorageDropoff(deliveryNode string) bool {
	if deliveryNode == "" {
		return false
	}
	node, err := d.db.GetNodeByDotName(deliveryNode)
	if err != nil || node == nil || node.IsSynthetic || node.ParentID == nil {
		return false
	}
	parent, err := d.db.GetNode(*node.ParentID)
	if err != nil || parent == nil {
		return false
	}
	return parent.NodeTypeCode == protocol.NodeClassLANE || parent.NodeTypeCode == protocol.NodeClassNGRP
}

// swapRemovalLegHeld reports whether `order` is the removal (evac) leg of a
// two-robot swap whose supply sibling has not yet claimed a replacement
// bin. While true the removal leg must not claim/pull the line bin — the
// line keeps its current bin until a replacement is secured (ALN_003
// swap-starvation, 2026-06-03). Returns (false, "") for non-swap orders,
// supply legs, and removal legs whose supply sibling already holds a claim.
// Fail-open on lookup errors: never freeze a robot on a transient failure.
func (d *Dispatcher) swapRemovalLegHeld(order *orders.Order) (bool, string) {
	sibUUID, err := d.db.OrderSiblingUUID(order.ID)
	if err != nil {
		log.Printf("dispatch: swap-hold sibling lookup for order %d: %v", order.ID, err)
		return false, ""
	}
	if sibUUID == "" {
		return false, "" // not a swap leg
	}
	// The removal leg delivers AWAY from its line node; the supply leg
	// delivers TO the line (DeliveryNode == ProcessNode). Only the removal
	// leg is gated; an empty ProcessNode (no distinct line node) is not.
	if order.ProcessNode == "" || order.DeliveryNode == order.ProcessNode {
		return false, ""
	}
	sib, err := d.db.GetOrderByUUID(sibUUID)
	if err != nil || sib == nil {
		// Supply row should exist (created first, linked at intake); hold
		// rather than strand the line if it is somehow missing.
		return true, "swap: awaiting supply sibling"
	}
	claimed, err := d.db.ListBinsByClaim(sib.ID)
	if err != nil {
		log.Printf("dispatch: swap-hold claim check for order %d sib %d: %v", order.ID, sib.ID, err)
		return false, ""
	}
	if len(claimed) > 0 {
		return false, "" // supply secured a replacement — release the hold
	}
	return true, "swap: holding removal leg until supply sibling claims a bin"
}

func (d *Dispatcher) DispatchPreparedComplex(order *orders.Order) error {
	// Defense-in-depth: the fulfillment scanner's tryFulfill already gates on
	// IsAcquiring ({queued, sourcing}) before calling here, so a parent in
	// Reshuffling (with a compound in flight), or one already dispatched or
	// terminal, won't reach us through the scanner. Anything calling this
	// directly (engine recovery, future call sites) must still respect the
	// invariant — proceeding on a non-acquiring order would re-dispatch a parent
	// mid-reshuffle or race a post-resume. Commit 3b widened the accepted set
	// from queued to acquiring so a complex order that reached `sourcing` but
	// didn't finish dispatching is retried. Return nil so the caller treats a
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
			reason := rerr.Error()
			if order.QueueReason != reason {
				if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
					log.Printf("dispatch: set queue_reason for complex order %d: %v", order.ID, serr)
				}
			}
			d.dbg("complex: order %d still capacity-blocked at NGRP resolution: %s", order.ID, reason)
			return rerr
		default:
			d.failOrderInternal(order, "invalid_steps", rerr.Error())
			return rerr
		}
	}
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

	// Dedicated home loader PARK: when this is a changeover return from a
	// dedicated-loader home (order.SourceNode = the evac pickup), Core decides where
	// the bin lands — HOME if free, else a buffer slot, else drain — and rewrites
	// DeliveryNode. The Edge shipped DeliveryNode="" and named no target, so Core is
	// the single authority; the release-time redirect overlay (patchRedirectSegments)
	// carries the choice to the fleet. A non-dedicated / non-loader source is left
	// untouched (drains as today). NOT a dispatch gate (no isConcreteStorageDropoff
	// widening) — a resolution-time read, so the swap supply leg is never gated.
	d.placeForDedicatedLoader(order, resolvedSteps)

	// Two-robot swap removal-leg hold: don't let the removal (evac) leg
	// claim/pull the line bin until the supply sibling has secured a
	// replacement bin. Stops a swap from stranding the line when the
	// supermarket is empty (ALN_003 swap-starvation, 2026-06-03). Stay
	// queued — the scanner replays on EventBinUpdated when the supply leg
	// claims, clearing the gate. The sibling pointer is set at intake (the
	// removal leg carries it on its ComplexOrderRequest), so it is present
	// here even on the synchronous intake-dispatch path.
	if held, reason := d.swapRemovalLegHeld(order); held {
		if order.QueueReason != reason {
			if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
				log.Printf("dispatch: set queue_reason swap-hold for order %d: %v", order.ID, serr)
			}
		}
		d.dbg("complex: order %d held — %s", order.ID, reason)
		return fmt.Errorf("swap removal hold: %s", reason)
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
	if d.isConcreteStorageDropoff(order.DeliveryNode) {
		if blocked, reason := CheckDropoffCapacity(d.db, order.DeliveryNode, order.ID); blocked {
			if order.QueueReason != reason {
				if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
					log.Printf("dispatch: set queue_reason for complex order %d: %v", order.ID, serr)
				}
			}
			d.dbg("complex: order %d queued — concrete storage dropoff %s blocked: %s", order.ID, order.DeliveryNode, reason)
			return fmt.Errorf("dropoff capacity: %s", reason)
		}
	}

	// Claim each storage drop-off slot atomically — the store dual of the bin
	// claim, and the single sync point that stops two orders released
	// near-simultaneously from dispatching a bin into the same slot (the
	// Hopkinsville #115/#117 race). The CAS admits exactly one winner per slot;
	// reserve-at-dispatch by design, so a queued order holds no slot. On a lost
	// claim we revert the step to its NGRP origin so the next scanner tick
	// re-resolves to a free slot (selection skips claimed slots) and requeue; a
	// fixed concrete drop-off with no NGRP origin stays queued until the slot
	// frees. Claimed slots are released on terminal by UnclaimOrderSlots, riding
	// the same cleanup hooks as the bin claim. ClaimSlot is owner-idempotent, so
	// slots already held by this order survive a requeue/replay without
	// livelock. Only STORAGE/STAGING slots are claimed — LINE/production
	// drop-offs must not be (a two-robot supply leg delivers to a line a sibling
	// evac clears; gating it would deadlock), same role gate as the capacity
	// check above.
	//
	// CANONICAL ORDER (D18-Q5): claim the slots in ascending node-ID order, NOT step
	// order. Two orders whose FIXED-concrete drop-offs overlap in opposite step order
	// would otherwise cross-hold — A grabs S1 and waits on S2 while B grabs S2 and waits
	// on S1: a true ABBA slot deadlock, now UNBOUNDED since commit 5 stopped the stuck-
	// sweep from abandoning pre-dispatch orders. One global claim order makes the loser
	// fail its FIRST contended slot before holding anything the winner needs, so it backs
	// off cleanly (holding none) and retries. Fungible/NGRP slots re-resolve on contention
	// and don't strictly need this; sorting them too is harmless. SLOTS-BEFORE-BINS: this
	// whole loop runs to completion (or requeues) BEFORE reserveComplexPlan touches a bin
	// — do not interleave the two claim classes, or a slot↔bin cross-type cycle becomes
	// possible. Each class fully ordered before the next is what keeps that cycle closed.
	type slotClaim struct {
		stepIndex int
		node      *nodes.Node
	}
	var slotClaims []slotClaim
	for i := range resolvedSteps {
		s := resolvedSteps[i]
		if s.Action != protocol.ActionDropoff || s.Node == "" || !d.isConcreteStorageDropoff(s.Node) {
			continue
		}
		node, nerr := d.db.GetNodeByDotName(s.Node)
		if nerr != nil || node == nil {
			continue // claim/dispatch path below surfaces a clearer error
		}
		slotClaims = append(slotClaims, slotClaim{stepIndex: i, node: node})
	}
	sort.Slice(slotClaims, func(a, b int) bool { return slotClaims[a].node.ID < slotClaims[b].node.ID })
	for _, sc := range slotClaims {
		if cerr := d.db.ClaimSlot(sc.node.ID, order.ID); cerr != nil {
			reason := fmt.Sprintf("drop-off slot %s claimed by another order", resolvedSteps[sc.stepIndex].Node)
			if resolvedSteps[sc.stepIndex].Group != "" {
				// Revert to the NGRP so reResolveComplexSteps re-picks a free
				// slot on the next tick; persist so the choice isn't redone blind.
				resolvedSteps[sc.stepIndex].Node = resolvedSteps[sc.stepIndex].Group
				if j, mErr := json.Marshal(resolvedSteps); mErr == nil {
					if uErr := d.db.UpdateOrderStepsJSON(order.ID, string(j)); uErr != nil {
						log.Printf("dispatch: update steps_json after slot-claim loss for order %d: %v", order.ID, uErr)
					} else {
						order.StepsJSON = string(j)
					}
				}
			}
			if order.QueueReason != reason {
				if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
					log.Printf("dispatch: set queue_reason slot-claim for order %d: %v", order.ID, serr)
				}
			}
			d.dbg("complex: order %d held — %s", order.ID, reason)
			return fmt.Errorf("slot claim: %s", reason)
		}
	}

	// 1c reserve/confirm (commit 4, D39). MoveToSourcing at the START of the reserve
	// attempt: the order stays `sourcing` while it holds partials and the scanner
	// retries it (commit 3's widening, complex scope). Idempotent — a retried order
	// re-enters sourcing→sourcing every tick, which MoveToSourcing skips. The gates
	// above (swap-hold, capacity, slot-claim) run first and park a blocked order in
	// its entry status (queued first pass, sourcing on retry); both are retried by
	// the complex-scoped scanner, and each wrote queue_reason for the Edge push.
	if err := d.lifecycle.MoveToSourcing(order, "scanner", "reserving source bins"); err != nil {
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
	// soft-hold the gaps (reserveComplexPlan). Runs AFTER the canonical slot-claim loop
	// above, never interleaved with it (SLOTS-BEFORE-BINS, D18-Q5) — one claim class fully
	// ordered before the next is what prevents a slot↔bin cross-type deadlock cycle. GO is
	// gated on a COMPLETE distinct-bin set (D5): an incomplete order holds its partials and
	// stays `sourcing` for the
	// scanner to retry — a robot never starts a job it can't finish, and give-up is
	// operator-driven, never a timer (D18-Q4). There is no orphaned-hold window now:
	// the order is already `sourcing` before it holds anything, so a crash leaves a
	// `sourcing` order whose pending holds the reaper reclaims (TTL in commit 4,
	// owner-liveness in commit 5) — not a `queued` order stranded with claimed bins.
	assigned, outcome, rerr := d.reserveComplexPlan(order, plan)
	if rerr != nil {
		log.Printf("dispatch: complex order %d reserve error: %v", order.ID, rerr)
		return rerr
	}
	switch outcome {
	case reserveMoot:
		// Reserved nothing and every source node is empty — the work is void (e.g. a
		// swap evac whose line bin was removed to quality hold before dispatch). Skip
		// so Edge's HandleOrderSkipped advances the linked changeover task, rather
		// than hold forever: a moot evac is not demand (D18-Q4).
		d.skipOrderInternal(order, codeNoSourceBin, fmt.Sprintf("complex order %d: no bin at any source node", order.ID))
		return fmt.Errorf("complex order %d moot — skipped", order.ID)
	case reserveHolding:
		const reason = "awaiting source bins"
		if order.QueueReason != reason {
			if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
				log.Printf("dispatch: set queue_reason for complex order %d: %v", order.ID, serr)
			}
		}
		d.dbg("complex: order %d incomplete reserve — holding partials, retrying next tick", order.ID)
		return fmt.Errorf("complex order %d reserve incomplete", order.ID)
	}

	// Confirm = commit the complete reserved set to hard claims (apply-as-confirm, no
	// live re-walk). A claim_failed (a pending hold reaped, or a bin claimed by
	// another order between reserve and confirm) requeues the attempt; a malformed
	// order (no source pickup) fails.
	if cerr := d.confirmComplexPlan(order, plan, assigned); cerr != nil {
		var pe *planningError
		if errors.As(cerr, &pe) && pe.Code == codeClaimFailed {
			if serr := d.db.SetOrderQueueReason(order.ID, codeClaimFailed); serr != nil {
				log.Printf("dispatch: set queue_reason claim_failed for order %d: %v", order.ID, serr)
			}
			d.dbg("complex: order %d held on claim_failed: %s", order.ID, pe.Detail)
			return cerr
		}
		d.failOrderInternal(order, codeNoBin, cerr.Error())
		return cerr
	}

	preWait, hasWait := splitAtWait(resolvedSteps)
	vendorOrderID := fmt.Sprintf("%s%d-%s", VendorIDPrefix, order.ID, uuid.New().String()[:8])
	blocks := stepsToBlocks(vendorOrderID, preWait, 0)
	if len(blocks) == 0 {
		d.failOrderInternal(order, "invalid_steps", "no actionable steps before wait")
		return fmt.Errorf("no actionable blocks")
	}

	req := fleet.StagedOrderRequest{
		OrderID:    vendorOrderID,
		ExternalID: order.EdgeUUID,
		Blocks:     blocks,
		Priority:   order.Priority,
		RobotGroup: d.robotGroupForPayload(order.PayloadCode),
	}
	d.dbg("complex: creating staged order %s with %d initial blocks (hasWait=%v)", vendorOrderID, len(blocks), hasWait)
	if _, err := d.backend.CreateStagedOrder(req); err != nil {
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
	if order.QueueReason != "" {
		if err := d.db.SetOrderQueueReason(order.ID, ""); err != nil {
			log.Printf("dispatch: clear queue_reason for order %d: %v", order.ID, err)
		}
	}
	d.emitter.EmitOrderDispatched(order.ID, vendorOrderID, order.SourceNode, order.DeliveryNode)
	return nil
}

// failOrderInternal is the scanner-path failure helper. Same as
// failOrder but doesn't take an envelope (no edge-bound reply — the
// edge already has the queued status from intake; it'll learn about
// the failure via EventOrderFailed → edge_handler.HandleOrderError).
func (d *Dispatcher) failOrderInternal(order *orders.Order, code, detail string) {
	if err := d.lifecycle.Fail(order, order.StationID, code, detail); err != nil {
		log.Printf("dispatch: fail order %d: %v", order.ID, err)
	}
	d.emitter.EmitOrderFailed(order.ID, order.EdgeUUID, order.StationID, code, detail)
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

// handleComplexBuriedAtIntake creates the complex parent at Queued,
// acks edge, then plans and dispatches a buried-bin reshuffle
// compound. Branches on the source group's reshuffle_target_nodes
// property:
//
//   - empty → expose mode (PlanReshuffleUnburyOnly). Parent resumes
//     and re-runs its original first pickup against the now-
//     accessible original slot.
//   - non-empty with at least one empty target → target-node mode
//     (PlanReshuffleToTarget). Compound moves the target bin to the
//     first empty configured target; parent re-resolves against the
//     group on resume and finds it at the target node.
//   - non-empty with all targets occupied → leave parent Queued with
//     queue_reason. Scanner replays on bin/order events; once a
//     target frees the next replay proceeds.
//
// Lane contention: if the buried lane is already locked or TryLock
// races, leave the parent Queued with queue_reason — same disposition
// as planning_service.planBuriedReshuffle.
func (d *Dispatcher) handleComplexBuriedAtIntake(env *protocol.Envelope, p *protocol.ComplexOrderRequest, payloadCode string, buried *BuriedError) {
	stationID := env.Src.Station
	d.dbg("complex: order %s buried at intake — bin %d in lane %d (slot %s)",
		p.OrderUUID, buried.Bin.ID, buried.LaneID, buried.Slot.Name)

	// Preserve the original NGRP-bearing step shape so the resume path
	// (parent → Queued → scanner → reResolveComplexSteps) has the input
	// it needs to re-resolve once the compound completes.
	resolvedSteps := stepsAsResolved(p.Steps)
	stepsJSON, err := json.Marshal(resolvedSteps)
	if err != nil {
		d.sendError(env, p.OrderUUID, "internal_error", "failed to marshal steps")
		return
	}
	sourceNode, deliveryNode := extractEndpoints(resolvedSteps)

	order := &orders.Order{
		EdgeUUID:     p.OrderUUID,
		StationID:    stationID,
		OrderType:    OrderTypeComplex,
		Status:       StatusQueued,
		Quantity:     p.Quantity,
		Priority:     p.Priority,
		PayloadCode:  payloadCode,
		PayloadDesc:  p.PayloadDesc,
		SourceNode:   sourceNode,
		DeliveryNode: deliveryNode,
		ProcessNode:  p.ProcessNode,
		StepsJSON:    string(stepsJSON),
	}
	if err := d.db.CreateOrder(order); err != nil {
		log.Printf("dispatch: create complex parent for buried reshuffle: %v", err)
		d.sendError(env, p.OrderUUID, "internal_error", err.Error())
		return
	}
	d.emitter.EmitOrderReceived(order.ID, order.EdgeUUID, stationID, OrderTypeComplex, payloadCode, deliveryNode)
	d.sendAck(env, order.EdgeUUID, order.ID, sourceNode)

	// Resolve the lane's parent group so the planner has the group ID
	// for shuffle-slot search and the target_nodes property read.
	lane, err := d.db.GetNode(buried.LaneID)
	if err != nil || lane == nil || lane.ParentID == nil {
		d.dbg("complex: buried lane %d lookup failed (%v) — failing parent %d", buried.LaneID, err, order.ID)
		d.failOrderInternal(order, "reshuffle_error", "cannot determine node group for buried lane")
		return
	}
	groupID := *lane.ParentID

	// Lane-contention: leave the parent Queued for scanner replay.
	if d.laneLock.IsLocked(buried.LaneID) {
		reason := fmt.Sprintf("lane %d locked by reshuffle for another order", buried.LaneID)
		if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
			log.Printf("dispatch: set queue_reason on lane contention: %v", serr)
		}
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
		return
	}

	// Mode selection: empty target_nodes → expose mode; non-empty →
	// target-node mode (or queue when all targets occupied).
	targetNodeNames := ReshuffleTargetNodes(d.db, lane.ID, groupID)
	var plan *ReshufflePlan
	if len(targetNodeNames) == 0 {
		plan, err = PlanReshuffleUnburyOnly(d.db, buried.Bin, buried.Slot, lane, groupID)
	} else {
		targetNode, allOccupied, terr := d.pickEmptyReshuffleTarget(groupID, targetNodeNames)
		if terr != nil {
			d.failOrderInternal(order, "reshuffle_error", terr.Error())
			return
		}
		if allOccupied {
			reason := "all reshuffle targets occupied"
			if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
				log.Printf("dispatch: set queue_reason on targets-occupied: %v", serr)
			}
			d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
			return
		}
		plan, err = PlanReshuffleToTarget(d.db, buried.Bin, buried.Slot, lane, groupID, targetNode)
	}
	if err != nil {
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot plan reshuffle: %v", err))
		return
	}

	// Race-safe lock acquisition.
	if !d.laneLock.TryLock(buried.LaneID, order.ID) {
		reason := fmt.Sprintf("lane %d locked by reshuffle for another order", buried.LaneID)
		if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
			log.Printf("dispatch: set queue_reason on TryLock race: %v", serr)
		}
		d.emitter.EmitOrderQueued(order.ID, order.EdgeUUID, stationID, payloadCode)
		return
	}

	if err := d.CreateCompoundOrder(order, plan); err != nil {
		d.laneLock.Unlock(buried.LaneID)
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot create compound order: %v", err))
		return
	}
	// Expose-mode only: persist the lane-extension entry NOW so the
	// listener at AdvanceCompoundOrder terminal can look up the
	// target bin ID directly instead of re-deriving from lane state.
	// Target-node mode releases the lane immediately at terminal —
	// no row needed.
	if len(targetNodeNames) == 0 {
		if _, err := d.db.InsertPendingLaneExtension(&store.PendingLaneExtension{
			ComplexParentID:    order.ID,
			LaneID:             buried.LaneID,
			TargetBinID:        buried.Bin.ID,
			ExpectedFromNodeID: buried.Slot.ID,
		}); err != nil {
			log.Printf("dispatch: persist pending_lane_extension at intake for complex %d: %v", order.ID, err)
			// Non-fatal: the at-terminal arming path will still
			// run; if the row is missing then, it falls back to
			// the unconditional unlock. Loss is crash resilience
			// only.
		}
	}
	d.dbg("complex: compound reshuffle created for order %d: %d steps", order.ID, len(plan.Steps))

	// Arm restore-blockers via scheduleRestoreIfEnabled (default-off per group).
	// The "expected from-node" the listener watches for depends on the reshuffle
	// mode: in expose mode the parent picks the bin up from its original lane
	// slot (buried.Slot.ID); in target-node mode it picks up from the target
	// node. Identify the mode by scanning the plan for a retrieve step
	// (protocol.StepRetrieve) — present in target-node mode, absent in expose
	// mode — and take its ToNode when found.
	expectedFromNode := buried.Slot.ID
	for _, s := range plan.Steps {
		if s.StepType == protocol.StepRetrieve && s.ToNode != nil {
			expectedFromNode = s.ToNode.ID
		}
	}
	d.scheduleRestoreIfEnabled(order, groupID, buried.LaneID, plan, expectedFromNode)
}

// handleComplexBuriedOnReplay handles a burial discovered by the
// scanner-path re-resolve (after the parent has resumed from a prior
// reshuffle). Pivots the parent Queued → Reshuffling and dispatches a
// fresh compound. Same dual-mode logic as the intake path but without
// the parent-creation step — the order already exists.
//
// Multi-burial loop: each successful resume → re-resolve cycle that
// discovers a new burial gets its own compound. v6's livelock cap was
// removed in v7 — the lane-lock extension closes the only realistic
// re-burial vector for expose mode, and sequential legitimate burials
// in a multi-pickup complex order shouldn't be punished with a
// terminal fail.
func (d *Dispatcher) handleComplexBuriedOnReplay(order *orders.Order, buried *BuriedError) {
	lane, err := d.db.GetNode(buried.LaneID)
	if err != nil || lane == nil || lane.ParentID == nil {
		d.failOrderInternal(order, "reshuffle_error", "cannot determine node group for buried lane")
		return
	}
	groupID := *lane.ParentID

	if d.laneLock.IsLocked(buried.LaneID) {
		reason := fmt.Sprintf("lane %d locked by reshuffle for another order", buried.LaneID)
		if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
			log.Printf("dispatch: set queue_reason on replay lane contention: %v", serr)
		}
		return
	}

	targetNodeNames := ReshuffleTargetNodes(d.db, lane.ID, groupID)
	var plan *ReshufflePlan
	if len(targetNodeNames) == 0 {
		plan, err = PlanReshuffleUnburyOnly(d.db, buried.Bin, buried.Slot, lane, groupID)
	} else {
		targetNode, allOccupied, terr := d.pickEmptyReshuffleTarget(groupID, targetNodeNames)
		if terr != nil {
			d.failOrderInternal(order, "reshuffle_error", terr.Error())
			return
		}
		if allOccupied {
			reason := "all reshuffle targets occupied"
			if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
				log.Printf("dispatch: set queue_reason on replay targets-occupied: %v", serr)
			}
			return
		}
		plan, err = PlanReshuffleToTarget(d.db, buried.Bin, buried.Slot, lane, groupID, targetNode)
	}
	if err != nil {
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot plan reshuffle: %v", err))
		return
	}

	if !d.laneLock.TryLock(buried.LaneID, order.ID) {
		reason := fmt.Sprintf("lane %d locked by reshuffle for another order", buried.LaneID)
		if serr := d.db.SetOrderQueueReason(order.ID, reason); serr != nil {
			log.Printf("dispatch: set queue_reason on replay TryLock race: %v", serr)
		}
		return
	}
	if err := d.CreateCompoundOrder(order, plan); err != nil {
		d.laneLock.Unlock(buried.LaneID)
		d.failOrderInternal(order, "reshuffle_error",
			fmt.Sprintf("cannot create compound order on replay: %v", err))
		return
	}
	// Same expose-mode-only persistence as the intake path. See the
	// comment in handleComplexBuriedAtIntake.
	if len(targetNodeNames) == 0 {
		if _, err := d.db.InsertPendingLaneExtension(&store.PendingLaneExtension{
			ComplexParentID:    order.ID,
			LaneID:             buried.LaneID,
			TargetBinID:        buried.Bin.ID,
			ExpectedFromNodeID: buried.Slot.ID,
		}); err != nil {
			log.Printf("dispatch: persist pending_lane_extension at replay for complex %d: %v", order.ID, err)
		}
	}
	d.dbg("complex: replay compound reshuffle created for order %d: %d steps", order.ID, len(plan.Steps))

	// Arm restore-blockers via scheduleRestoreIfEnabled (default-off per group).
	// The "expected from-node" the listener watches for depends on the reshuffle
	// mode: in expose mode the parent picks the bin up from its original lane
	// slot (buried.Slot.ID); in target-node mode it picks up from the target
	// node. Identify the mode by scanning the plan for a retrieve step
	// (protocol.StepRetrieve) — present in target-node mode, absent in expose
	// mode — and take its ToNode when found.
	expectedFromNode := buried.Slot.ID
	for _, s := range plan.Steps {
		if s.StepType == protocol.StepRetrieve && s.ToNode != nil {
			expectedFromNode = s.ToNode.ID
		}
	}
	d.scheduleRestoreIfEnabled(order, groupID, buried.LaneID, plan, expectedFromNode)
}

// pickEmptyReshuffleTarget walks the configured target-node names in
// order and returns the first one with zero bins. Returns
// (nil, true, nil) when all configured targets are occupied — the
// caller queues the parent in that case rather than falling back to
// expose mode. Validation failures (target name doesn't resolve, or
// resolves to a synthetic / lane / non-direct-child) return a
// non-nil error.
func (d *Dispatcher) pickEmptyReshuffleTarget(groupID int64, names []string) (target *nodes.Node, allOccupied bool, err error) {
	if len(names) == 0 {
		return nil, false, nil
	}
	for _, name := range names {
		node, gErr := d.db.GetNodeByDotName(name)
		if gErr != nil || node == nil {
			return nil, false, fmt.Errorf("reshuffle target %s not found in group %d", name, groupID)
		}
		if node.ParentID == nil || *node.ParentID != groupID {
			return nil, false, fmt.Errorf("reshuffle target %s is not a direct child of group %d", name, groupID)
		}
		if node.IsSynthetic || node.NodeTypeCode == protocol.NodeClassLANE {
			return nil, false, fmt.Errorf("reshuffle target %s must be a non-synthetic, non-lane node", name)
		}
		cnt, _ := d.db.CountBinsByNode(node.ID)
		if cnt == 0 && node.ClaimedBy == nil {
			return node, false, nil
		}
	}
	return nil, true, nil
}
