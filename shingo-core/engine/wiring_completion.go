// wiring_completion.go — Order delivery and completion handling.
//
// handleOrderDelivered runs when fleet reports FINISHED. It sends the
// delivered notification and moves the bin(s) to their destination
// immediately so telemetry is accurate. handleOrderCompleted runs when
// Edge confirms receipt and advances compound orders. Both paths use
// applyBinArrivalForOrder / applyMultiBinArrivalForOrder for the bin
// move; the completion path is idempotent (skips bins already at dest).

package engine

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/service"
	"shingocore/store/orders"
)

// handleOrderDelivered runs on fleet-reported FINISHED. Notifies Edge
// and moves the bin to its destination immediately so subsequent orders
// see accurate occupancy.
func (e *Engine) handleOrderDelivered(order *orders.Order) {
	// Resolve staged expiry for the delivered message. Only ship a countdown
	// when the bin will actually arrive `staged` — for storage destinations
	// (LANE/NGRP roots and their children) the bin lands `available`
	// and an expiry on the order envelope is misleading to the operator UI.
	var stagedExpireAt *time.Time
	if order.DeliveryNode != "" {
		if destNode, err := e.db.GetNodeByDotName(order.DeliveryNode); err == nil {
			if staged, ea := e.resolveNodeStaging(destNode); staged && ea != nil {
				stagedExpireAt = ea
			}
		}
	}

	// Apply bin arrival FIRST so telemetry is accurate immediately. The
	// previous order — sendToEdge then applyBinArrivalForOrder — let
	// AutoConfirm Edge orders auto-confirm before the bin-arrival
	// commit landed.
	//
	// ── FAIL LOUD, AND STOP ───────────────────────────────────────────────
	// A refusal here means the robot arrived carrying something the ledger says
	// belongs to someone else, and Core cannot tell what is actually on the deck.
	// That is an integrity fault, not congestion, so wait-not-fail does not cover
	// it: standing law 1's own carve-out is that genuine faults fail loud with a
	// named message.
	//
	// THE LICENCE IS THE EVIDENCE, not an argument. The instrument read 121, then
	// 2, then 1 as three successive extraction errors were corrected, and every
	// surviving specimen was explained benign — the last being a terminal order
	// whose bin had moved on, closed by the discriminator in arrival_guard.go. A
	// refusal that survives all four cuts is a state nothing in the claim
	// lifecycle should be able to produce, so the right response to seeing one is
	// to stop and say so.
	//
	// Parking was the alternative and it loses on a fact discovered while
	// building: Core does not know what the robot is holding. Parking keeps an
	// order alive whose payload is unidentifiable, still holding a runtime slot —
	// the dead-robot wedge — which is worse than a loud failure.
	//
	// IT RETURNS rather than falling through, because the rest of this function
	// tells Edge the order was DELIVERED. Failing the order and then announcing
	// its delivery in the same breath is the lie this whole thread has been
	// unwinding.
	//
	// failOrderAndEmit, not a bare FailOrderAtomic: it routes through
	// Lifecycle().Fail and fires EventOrderFailed, so the failure lands in the
	// audit trail and reaches the station like every other failure. It also
	// releases the order's bin claims, which is correct here — the bin it thought
	// it held is demonstrably not its own.
	if refusal := e.applyBinArrivalForOrder(order); refusal != nil {
		claimant := "nobody"
		if refusal.ClaimedBy != nil {
			claimant = fmt.Sprintf("order %d", *refusal.ClaimedBy)
		}
		detail := fmt.Sprintf("cargo does not match the ledger: bin %d is claimed by %s, not by this "+
			"order (%s). Core cannot identify what the robot is carrying, so the delivery is not "+
			"recorded and the order is failed rather than reported delivered.",
			refusal.BinID, claimant, refusal.Context())
		e.logFn("FAIL: order=%d refused at %s — %s", order.ID, refusal.Site, detail)
		e.failOrderAndEmit(order.ID, "cargo_ledger_mismatch", detail)
		return
	}

	// Ship the bin ID so Edge can attribute PLC tick deltas to the
	// right bin. Single-bin orders carry BinID; multi-tote (multi-bin)
	// orders select the ONE bin destined for the consuming process node
	// (F1b) — see selectConsumingBinForNode. Edge's bin-ownership flip
	// means active_bin_id at the runtime row is now sourced from this
	// envelope — without it, Edge can't track tick attribution for the
	// duration the bin sits at the slot.
	var binID *int64
	var binDestNode string
	multiBin := false
	if order.BinID != nil {
		orderBins, _ := e.db.ListOrderBins(order.ID)
		if len(orderBins) == 0 {
			v := *order.BinID
			binID = &v
		} else {
			// F1b — multi-tote adoption. order.BinID names the bin claimed AT
			// the process node (for a swap, the evac bin picked up there), not
			// the supply bin that stays and is consumed. Select the order_bin
			// whose dest_node is the consuming node (order.ProcessNode) and ship
			// THAT bin so Edge binds the right carrier. If none lands at the
			// process node, leave binID nil so the Edge multi-bin backstop alarm
			// (P2-C3) fires — never guess.
			multiBin = true
			if sel := selectConsumingBinForNode(orderBins, order.ProcessNode); sel != nil {
				v := sel.BinID
				binID = &v
				binDestNode = sel.DestNode
			} else {
				e.logFn("engine: order=%d multi-bin delivered: no order_bin destined for process node %q — Edge multi-bin backstop alarm will fire (F1b no-match)",
					order.ID, order.ProcessNode)
			}
		}
	}

	// Diagnostic: surface the missing-bin case so a future "Edge isn't
	// tracking ticks" investigation can grep for the cause. order.BinID
	// nil on a single-bin order means planMove never persisted the bin
	// reference (a known failure mode the bin-stuck-at-source log at
	// applyBinArrivalForOrder also tracks).
	if binID == nil && !multiBin {
		e.logFn("engine: order=%d type=%s shipped order.delivered without bin_id (order.BinID nil at delivery — Edge tick attribution will be silent until next order)",
			order.ID, order.OrderType)
	}

	// Snapshot the just-arrived bin's authoritative count + load-lifecycle
	// epoch onto the envelope so Edge seeds its runtime cache and stamps
	// outgoing BinUOPDeltas from these — no separate HTTP pull. Read AFTER
	// applyBinArrivalForOrder above so the bin row reflects the arrival.
	// Reads whichever bin was selected above: order.BinID for single-bin, or
	// the consuming-node bin for multi-tote (F1b). binID nil (unmatched
	// multi-bin) leaves the snapshot empty and Edge falls back to its default.
	var uopRemaining *int
	var deltaEpoch int64
	if binID != nil {
		if bin, binErr := e.db.GetBin(*binID); binErr == nil {
			u := bin.UOPRemaining
			uopRemaining = &u
			deltaEpoch = bin.DeltaEpoch
		} else {
			e.logFn("engine: order=%d delivered: bin %d uop/epoch lookup failed: %v (Edge falls back to role default)",
				order.ID, *binID, binErr)
		}
	}

	// Core-admin (manual move) orders have no station; broadcast to all edges so
	// each can attempt the bin binding via DeliveryNode fallback.
	stationID := order.StationID
	if stationID == "" {
		stationID = protocol.StationBroadcast
	}
	if err := e.sendToEdge(protocol.TypeOrderDelivered, stationID, &protocol.OrderDelivered{
		OrderUUID:      order.EdgeUUID,
		DeliveredAt:    clock.Now().UTC(),
		StagedExpireAt: stagedExpireAt,
		BinID:          binID,
		UOPRemaining:   uopRemaining,
		DeltaEpoch:     deltaEpoch,
		DeliveryNode:   order.DeliveryNode,
		BinDestNode:    binDestNode,
	}); err != nil {
		e.logFn("engine: delivered notification: %v", err)
	}
}

// selectConsumingBinForNode returns the order_bin destined for the consuming
// (process) node — the bin that stays at the line and receives consume ticks.
// For a two-robot swap this is the SUPPLY bin dropped at the line, NOT
// order.BinID, which names the evac bin picked up AT the line. dest_node is the
// per-bin landing node persisted by the allocator (resolvePerBinDestinations),
// so a plain dot-name compare against order.ProcessNode is unambiguous — the
// same string-compare the Edge delivery gate uses (dest == CoreNodeName).
//
// Returns nil when processNode is empty or no bin lands there, so the caller
// leaves BinID nil and the Edge multi-bin backstop alarm (P2-C3) fires rather
// than binding a guess.
func selectConsumingBinForNode(orderBins []*orders.OrderBin, processNode string) *orders.OrderBin {
	if processNode == "" {
		return nil
	}
	for _, ob := range orderBins {
		if ob.DestNode == processNode {
			return ob
		}
	}
	return nil
}

// applyBinArrivalForOrder moves the order's bin(s) to the delivery node.
// Called from handleOrderDelivered (on fleet FINISHED) so that telemetry
// is accurate immediately. handleOrderCompleted still runs on confirmation
// but is idempotent — it skips the bin move if already at the destination.
// It returns the refusal when the claim guard declines the placement, so the
// completion path can see that this order did not deliver what it says it did.
// What to DO about that is an open ruling (see arrival_guard.go) — today the
// caller records it and nothing more, which is exactly the previous behaviour
// plus the ability to know.
func (e *Engine) applyBinArrivalForOrder(order *orders.Order) *ArrivalRefusal {
	if order.SourceNode == "" || order.DeliveryNode == "" {
		// Bin-stuck-at-source diagnostic: previously a silent skip. Move-order
		// post-mortem 2026-04-28 traced "delivered but bin still at source"
		// scenarios that left no log line at all.
		e.logFn("delivery: order=%d type=%s bin=%v skipped arrival: missing source/delivery (source=%q delivery=%q)",
			order.ID, order.OrderType, order.BinID, order.SourceNode, order.DeliveryNode)
		return nil
	}

	// Release the order's destination-slot claims now that its bins have
	// arrived — the dispatch-time ClaimSlot has served its purpose. Without this
	// the happy path leaked slot claims until the terminal transition (the
	// TerminalizeOrder backstop at 'confirmed'), so a slot could stay
	// un-reclaimable in the Delivered→Confirmed window after its bin moved on.
	if err := e.db.UnclaimOrderSlots(order.ID); err != nil {
		e.logFn("engine: release slot claims for order %d on arrival: %v", order.ID, err)
	}

	// Multi-bin path
	orderBins, _ := e.db.ListOrderBins(order.ID)
	if len(orderBins) > 0 {
		e.logFn("delivery: order=%d type=%s taking multi-bin arrival path (%d junction rows)",
			order.ID, order.OrderType, len(orderBins))
		// A multi-bin order can be refused for some bins and place the rest. The
		// first refusal is enough to tell the caller this order did not deliver
		// everything it is about to claim it did; all of them are counted and
		// logged inside.
		if rs := e.applyMultiBinArrivalForOrder(order, orderBins); len(rs) > 0 {
			return rs[0]
		}
		return nil
	}

	// Single-bin path
	//
	// SHADOWED: the diagnostic below reads a NULL bin_id as "planMove may have
	// failed to persist BinID", which is true of a broken order AND true of a
	// coordinator, whose bin_id is NULL permanently and correctly. See
	// service.NoteFolderShadow.
	if order.BinID == nil {
		owns, oerr := e.db.OrderOwnsNoCargo(order.ID)
		service.NoteFolderShadow(service.FolderSiteDeliverySettle, order.ID, owns, oerr)
		// Bin-stuck-at-source diagnostic: this is the failure mode where
		// planMove's UpdateOrderBinID didn't persist (or was never called)
		// but the order still progressed to FINISHED. Without a log here,
		// the bin silently stays at source and the symptom shows up downstream.
		e.logFn("delivery: order=%d type=%s skipped arrival: order.BinID is nil (source=%s delivery=%s) — planMove may have failed to persist BinID",
			order.ID, order.OrderType, order.SourceNode, order.DeliveryNode)
		return nil
	}

	destNode, err := e.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		e.logFn("engine: dest node %s not found for delivery arrival: %v", order.DeliveryNode, err)
		return nil
	}

	sourceNode, _ := e.db.GetNodeByDotName(order.SourceNode)
	sourceNodeID := int64(0)
	if sourceNode != nil {
		sourceNodeID = sourceNode.ID
	}

	// Claim-based teleport guard (#7): a late-arriving FINISHED for an
	// order that was meanwhile failed/cancelled (releasing the claim)
	// or whose bin was reclaimed by a newer order would, without this
	// guard, move the bin to a stale destination — the same teleport
	// shape SMN_001 / SMN_002 produced in the completion path. The
	// completion-time guard at handleOrderCompleted already protects
	// the safety-net call; this matches the predicate at delivery time.
	//
	// Skip the guard for compound order children (ParentOrderID != nil):
	// a multi-step reshuffle plan intentionally overlaps bin claims —
	// when CreateCompoundChildren writes claims for all steps in one
	// transaction, the LAST step's UPDATE wins for any bin that appears
	// in multiple steps (e.g. an unbury followed by a restock both
	// touching the same blocker bin). Interim child orders need to move
	// the bin even though claimed_by points at a sibling child. The
	// compound dispatcher serializes children sequentially, so the
	// teleport class this guard prevents (concurrent reclaim) doesn't
	// apply within a compound family.
	//
	// "The LAST step's UPDATE wins" is no longer unconditional, and the
	// difference matters to this skip. That claim is now a sibling-scoped
	// compare-and-set (store/orders.go CreateCompoundChildren): last-write-wins
	// still holds for a bin held by this compound's parent or by one of its
	// children — which is every case this skip is about — but a bin held by an
	// order OUTSIDE the compound is refused and fails the whole transaction.
	// So a child reaching here can no longer be carrying a bin an unrelated
	// order claimed after the plan was built: the compound would never have been
	// created. The skip is therefore narrower than it was in what it lets
	// through, and unchanged in what it is FOR.
	guardBin, binErr := e.db.GetBin(*order.BinID)
	if binErr != nil {
		e.logFn("engine: get bin %d for delivery arrival guard: %v", *order.BinID, binErr)
		return nil
	}
	if r := e.recordArrivalRefusal(refuseArrival(order, guardBin, destNode.ID, arrivalSiteDelivery)); r != nil {
		return r
	}

	staged, expiresAt := e.resolveNodeStaging(destNode)

	// Note: previously this path forced staged=false for complex orders with
	// WaitIndex > 0 and for retrieve_empty deliveries. Both overrides removed
	// 2026-04-14 — they bypassed the FindSourceBinFIFO staged exclusion and
	// allowed unloader/loader auto-requests to poach lineside bins. With the
	// overrides gone, lineside deliveries arrive `staged` and stay protected
	// until the next claim or operator action.

	e.logFn("delivery: order=%d type=%s bin=%d arriving %s -> %s (staged=%v)",
		order.ID, order.OrderType, *order.BinID, order.SourceNode, order.DeliveryNode, staged)
	evicted, err := e.binService.ApplyArrival(*order.BinID, destNode.ID, staged, expiresAt, order.ID)
	if err != nil {
		e.logFn("engine: apply bin arrival on delivery for order %d bin %d: %v", order.ID, *order.BinID, err)
		return nil
	}
	if evicted {
		e.logFn("WARN: delivery of bin %d to %s evicted a stale bin record there — a delivery cannot physically complete onto an occupied slot, so the completed delivery proves the slot was empty; the stale bin is at _TRANSIT, recover via the anomalies page", *order.BinID, order.DeliveryNode)
	}

	// Re-read bin for the event payload (post-ApplyArrival state). The
	// guard's earlier read is pre-arrival; the event needs the new node
	// and any side-effects from ApplyArrival (e.g. anomaly_at clear).
	updatedBin, updatedErr := e.db.GetBin(*order.BinID)
	if updatedErr != nil {
		e.logFn("engine: get bin %d for delivery arrival event: %v", *order.BinID, updatedErr)
	}
	if updatedBin != nil {
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       updatedBin.ID,
			PayloadCode: updatedBin.PayloadCode,
			FromNodeID:  sourceNodeID,
			ToNodeID:    destNode.ID,
			NodeID:      destNode.ID,
		}})
	}
	return nil
}

// applyMultiBinArrivalForOrder handles the multi-bin case at delivery time.
//
// Note: previously this path forced staged=false for complex orders with
// WaitIndex > 0 ("operatorConfirmed"). Override removed 2026-04-14 — bins
// arriving at lineside via complex orders now stage like simple orders do.
// See applyBinArrivalForOrder for full context.
// It returns one refusal per bin the claim guard declined — see
// applyBinArrivalForOrder and arrival_guard.go. A multi-bin order can be refused
// for SOME of its bins and place the rest, so this is a slice rather than a
// single answer; that partial shape is exactly why the disposition ruling is
// still open.
func (e *Engine) applyMultiBinArrivalForOrder(order *orders.Order, orderBins []*orders.OrderBin) []*ArrivalRefusal {
	var refusals []*ArrivalRefusal
	var instructions []orders.BinArrivalInstruction
	// fromNodeIDs[i] is the source node of instructions[i]. Captured here
	// so the post-arrival BinUpdatedEvent can carry FromNodeID — without it
	// handleKanbanDemand cannot fire produce signals on storage-slot exit.
	var fromNodeIDs []int64

	// Measured before anything is placed, because the interesting number is how
	// often the record the settle is ABOUT TO USE disagrees with the plan.
	e.noteDestNodeDrift(order, orderBins, driftSiteDelivery)

	for _, ob := range orderBins {
		if ob.DestNode == "" {
			continue
		}
		// Claim-based teleport guard (#8): per-bin variant of the same
		// predicate applyBinArrivalForOrder uses for single-bin orders.
		// A late-arriving FINISHED on a stale order, or a bin reclaimed
		// between FINISHED and the engine's processing of it, must NOT
		// be teleported to the junction-table destination. The
		// completion-time path (handleMultiBinCompleted) has the same
		// guard; this matches it at delivery time.
		//
		// Compound children (ParentOrderID != nil) skip the guard for
		// the same overlapping-claim reason documented in
		// applyBinArrivalForOrder.
		guardBin, err := e.db.GetBin(ob.BinID)
		if err != nil {
			e.logFn("engine: order %d bin %d get for delivery guard: %v", order.ID, ob.BinID, err)
			continue
		}
		// Destination resolved BEFORE the guard so a refusal can say where the bin
		// was owed, not just who owns it — the diagnosable half.
		destNode, err := e.db.GetNodeByDotName(ob.DestNode)
		if err != nil {
			e.logFn("engine: order %d bin %d dest node %q not found on delivery: %v", order.ID, ob.BinID, ob.DestNode, err)
			continue
		}
		// ALREADY THERE IS NOT A PLACEMENT, and this site had no way to say so.
		//
		// Its completion-time sibling has asked this first since cb7ed41d; here the
		// settle re-placed every junction row unconditionally. The asymmetry matters
		// more at THIS site than at that one, because of where the destination comes
		// from: order_bins.dest_node is written once at allocation and updated by
		// nothing (D2), while the single-bin path next door reads order.DeliveryNode,
		// which the gate re-bind does maintain. So this is the loop that can re-place
		// a bin the fleet already reported somewhere.
		//
		// Asked BEFORE ownership, matching the sibling: a bin sitting at its
		// destination is a finished delivery whoever holds the claim by now, and
		// asking about ownership first is what counted 121 ordinary deliveries as
		// defects. On a repeat `delivered` event — the at-least-once shape — the
		// claim is already released, so this site would have logged a refusal for a
		// delivery that worked.
		//
		// SKIPPING DOES NOT STRAND THE CLAIM: TerminalizeOrderWithReason releases
		// every claim this order holds, unconditionally, and stamps anomaly_at only
		// on bins still at _TRANSIT — which a landed bin is not.
		if binAlreadyAt(guardBin, destNode.ID) {
			continue
		}
		if r := e.recordArrivalRefusal(refuseArrival(order, guardBin, destNode.ID, arrivalSiteMultiBinDelivery)); r != nil {
			refusals = append(refusals, r)
			continue
		}
		staged, expiresAt := e.resolveNodeStaging(destNode)
		instructions = append(instructions, orders.BinArrivalInstruction{
			BinID:     ob.BinID,
			ToNodeID:  destNode.ID,
			Staged:    staged,
			ExpiresAt: expiresAt,
		})

		// Resolve the per-bin source node (the OrderBin.NodeName is the dot-path
		// of the pickup step). 0 means "unknown source" — kanban will simply not
		// fire the FROM-side check, which is the correct degradation.
		fromNodeID := int64(0)
		if ob.NodeName != "" {
			if srcNode, err := e.db.GetNodeByDotName(ob.NodeName); err == nil && srcNode != nil {
				fromNodeID = srcNode.ID
			}
		}
		fromNodeIDs = append(fromNodeIDs, fromNodeID)
	}

	// ── A PARTIAL SETTLEMENT IS CORRUPTION, SO NOTHING COMMITS ───────────────
	//
	// This used to place the bins that passed and THEN hand the refusals back, so
	// a swap whose second leg was refused had its first leg written, the order
	// failed, and Edge was never told about the bin that did land (D4). Wrong
	// under every disposition the round considered, which is why the mechanics
	// were fixed regardless of how the policy landed.
	//
	// R.26 settled the policy on a plant fact rather than an argument: a dig works
	// LANES and never reclaims a leg of another process's in-flight order, so one
	// bin of a settlement belonging to somebody else is not a race the design
	// permits — it is an integrity failure. The right response to seeing one is to
	// stop loudly with nothing written, not to record half a delivery.
	//
	// THIS IS AN ASSERT, and it is deliberately placed before the commit rather
	// than compensating after it. It is also future-proofing: if a later era
	// changes the dig rule, this fires on day one and the partial-delivery policy
	// question comes back with evidence attached instead of being re-argued.
	//
	// It does NOT widen the blast radius. handleOrderDelivered has failed the
	// order on any refusal since 5c31033e; all that changes is whether a partial
	// write happened first. And the arrived check above narrowed the population
	// reaching here — a repeat delivery event whose bins are still at their
	// destinations is now skipped rather than refused.
	if len(refusals) > 0 {
		for _, r := range refusals {
			e.logFn("ASSERT: order %d settlement refused for bin %d (%s; %s) — NOTHING IS WRITTEN. "+
				"%d of this order's %d bins were about to be placed and are not: a settlement that "+
				"finds one leg's bin no longer belonging to this order is an integrity failure, and "+
				"digs work lanes rather than another process's in-flight legs, so this is not a race "+
				"the design permits (PLAN §R.26).",
				order.ID, r.BinID, r.Reason(), r.Context(), len(instructions), len(orderBins))
		}
		return refusals
	}

	if len(instructions) == 0 {
		return refusals
	}

	evictedGhosts, err := e.db.ApplyMultiBinArrival(order.ID, instructions)
	if err != nil {
		e.logFn("engine: multi-bin delivery arrival for order %d: %v", order.ID, err)
		return refusals
	}
	for _, ghostID := range evictedGhosts {
		e.logFn("WARN: multi-bin delivery for order %d evicted a stale bin record (bin %d) to _TRANSIT — a delivery cannot physically complete onto an occupied slot; recover via the anomalies page",
			order.ID, ghostID)
	}
	// The burial shadow instrument. Explicit here because the multi-bin arrival
	// goes through the store aggregate rather than BinService.ApplyArrival, which
	// calls it for itself. Post-commit and result-free either way.
	for _, inst := range instructions {
		e.binService.NoteBurialShadow(inst.BinID, inst.ToNodeID, order.ID)
	}

	for i, inst := range instructions {
		bin, err := e.db.GetBin(inst.BinID)
		if err != nil {
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			FromNodeID:  fromNodeIDs[i],
			ToNodeID:    inst.ToNodeID,
			NodeID:      inst.ToNodeID,
		}})
	}
	return refusals
}

// handleOrderCompleted runs when Edge confirms receipt. Bin movement already
// happened in handleOrderDelivered, so this is mostly paperwork (compound
// order advancement, cleanup). The bin arrival call is kept as an idempotent
// safety net — if the bin is already at dest, ApplyBinArrival is a no-op.
func (e *Engine) handleOrderCompleted(ev OrderCompletedEvent) {
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil {
		e.logFn("engine: get order %d for completion: %v", ev.OrderID, err)
		return
	}

	// If this is a child of a compound order, advance the parent
	if order.ParentOrderID != nil && e.dispatcher != nil {
		e.dispatcher.HandleChildOrderComplete(order)
	}

	if order.SourceNode == "" || order.DeliveryNode == "" {
		return
	}

	// Check for multi-bin junction table rows (populated by ApplyComplexPlan
	// for orders with 2+ pickup steps). If present, each bin has a per-step
	// destination — use the junction table path instead of the legacy single-bin path.
	orderBins, _ := e.db.ListOrderBins(order.ID)
	if len(orderBins) > 0 {
		e.handleMultiBinCompleted(order, orderBins)
		return
	}

	// Legacy single-bin path: idempotent safety net — bin should already be at
	// dest from handleOrderDelivered, but re-apply in case delivery arrival failed.
	//
	// SHADOWED (silent skip): a coordinator relies on this returning, and so does
	// a defective order — the same branch for opposite reasons.
	if order.BinID == nil {
		owns, oerr := e.db.OrderOwnsNoCargo(order.ID)
		service.NoteFolderShadow(service.FolderSiteCompletionNet, order.ID, owns, oerr)
		return
	}

	bin, err := e.db.GetBin(*order.BinID)
	if err != nil {
		e.logFn("engine: get bin %d for completion: %v", *order.BinID, err)
		return
	}
	destNode, err := e.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil {
		e.logFn("engine: dest node %s not found for completion: %v", order.DeliveryNode, err)
		return
	}
	sourceNode, _ := e.db.GetNodeByDotName(order.SourceNode)
	sourceNodeID := int64(0)
	if sourceNode != nil {
		sourceNodeID = sourceNode.ID
	}

	// The safety net's question — is there anything left to re-apply? — is
	// answered by reapplyRefused, which documents itself. Read it there.
	//
	// The 34 lines that used to sit here narrated the contract as it stood BEFORE
	// cb7ed41d: three claim arms, `claimed_by == nil → Skip` stated
	// unconditionally, no arrived check and no terminal cut. Two of those three
	// answers are now true only downstream of two questions this text did not
	// mention, and a load-bearing comment that describes a predicate's previous
	// shape is worse than none — it is the version a reader trusts (law 14).
	if skip, r := reapplyRefused(order, bin, destNode.ID, arrivalSiteCompletionNet); skip {
		e.recordArrivalRefusal(r) // nil for the ordinary already-landed case
		return
	}

	// Bin still at source — apply arrival as recovery from a missed FINISH

	staged, expiresAt := e.resolveNodeStaging(destNode)

	// Note: see applyBinArrivalForOrder for the override-removal context.
	// Same overrides existed here in the safety-net path and were removed
	// for the same reason.

	evicted, err := e.binService.ApplyArrival(*order.BinID, destNode.ID, staged, expiresAt, order.ID)
	if err != nil {
		e.logFn("engine: apply bin arrival for order %d bin %d: %v", order.ID, *order.BinID, err)
		return
	}
	if evicted {
		e.logFn("WARN: delivery of bin %d to %s evicted a stale bin record there — a delivery cannot physically complete onto an occupied slot, so the completed delivery proves the slot was empty; the stale bin is at _TRANSIT, recover via the anomalies page", *order.BinID, order.DeliveryNode)
	}

	// Emit bin contents changed
	updatedBin, binErr := e.db.GetBin(*order.BinID)
	if binErr != nil {
		e.logFn("engine: get bin %d for completion event: %v", *order.BinID, binErr)
	}
	if updatedBin != nil {
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       updatedBin.ID,
			PayloadCode: updatedBin.PayloadCode,
			FromNodeID:  sourceNodeID,
			ToNodeID:    destNode.ID,
			NodeID:      destNode.ID,
		}})
	}
}

// handleMultiBinCompleted processes completion for orders with multiple claimed bins.
// Each bin is moved to its per-step destination (from the order_bins junction table)
// in a single atomic transaction. Idempotent — skips bins already at their destination
// (normal case when applyMultiBinArrivalForOrder already ran at delivery time).
func (e *Engine) handleMultiBinCompleted(order *orders.Order, orderBins []*orders.OrderBin) {
	var instructions []orders.BinArrivalInstruction
	// fromNodeIDs[i] is the source node of instructions[i] — same purpose as
	// in applyMultiBinArrivalForOrder: keep FromNodeID intact so kanban can
	// fire on storage-slot exit when this safety-net path actually moves a bin.
	var fromNodeIDs []int64

	// Note: previously had an "operatorConfirmed" override forcing staged=false
	// for complex orders with WaitIndex > 0. Removed 2026-04-14 — see
	// applyBinArrivalForOrder for context.

	// Same reading as the delivery-time site, taken separately: this handler fires
	// on (X → delivered) and again on (delivered → confirmed), so a per-site split
	// is what tells a drift that survived the first settle from one that only the
	// safety net ever sees.
	e.noteDestNodeDrift(order, orderBins, driftSiteCompleted)

	for _, ob := range orderBins {
		if ob.DestNode == "" {
			e.logFn("engine: order %d bin %d has no dest_node in order_bins — skipping", order.ID, ob.BinID)
			continue
		}
		destNode, err := e.db.GetNodeByDotName(ob.DestNode)
		if err != nil {
			e.logFn("engine: order %d bin %d dest node %q not found: %v", order.ID, ob.BinID, ob.DestNode, err)
			continue
		}

		// Safety-net invariant: only re-apply this leg's arrival if the
		// bin is STILL claimed by THIS order. Same predicate as the
		// single-bin path — see the long comment there for the
		// SMN_001 / Phase 2 transit-semantics rationale. Compound
		// children skip the guard (overlapping claims by design).
		netBin, err := e.db.GetBin(ob.BinID)
		if err != nil {
			e.logFn("engine: order %d bin %d get for safety-net guard: %v", order.ID, ob.BinID, err)
			continue
		}
		if skip, r := reapplyRefused(order, netBin, destNode.ID, arrivalSiteMultiBinCompleted); skip {
			e.recordArrivalRefusal(r) // nil for the ordinary already-landed case
			continue
		}

		staged, expiresAt := e.resolveNodeStaging(destNode)
		instructions = append(instructions, orders.BinArrivalInstruction{
			BinID:     ob.BinID,
			ToNodeID:  destNode.ID,
			Staged:    staged,
			ExpiresAt: expiresAt,
		})

		// Capture the per-bin source node before we move it so the post-arrival
		// event still has it. The OrderBin.NodeName is the pickup step's dot-path.
		fromNodeID := int64(0)
		if ob.NodeName != "" {
			if srcNode, err := e.db.GetNodeByDotName(ob.NodeName); err == nil && srcNode != nil {
				fromNodeID = srcNode.ID
			}
		}
		fromNodeIDs = append(fromNodeIDs, fromNodeID)
	}

	// Junction rows are deleted only when the order has reached a terminal
	// status (confirmed / failed / cancelled). The Stage 10 action map
	// fires fireCompleted on (X, delivered) AND on (delivered, confirmed),
	// so this handler runs twice per order. Deleting on the first
	// (delivered) call would lose the per-bin destination data the
	// sibling handleOrderDelivered path needs on the same status change;
	// keeping the rows alive until terminal lets every completion firing
	// take the multi-bin idempotent path consistently. The terminal
	// transition (handled by HandleOrderReceipt's MarkConfirmed) is the
	// natural cleanup point — by then no more re-runs of this handler
	// will fire for the order.
	//
	// orderIsTerminal, not a bare protocol.IsTerminal, and this is the site the
	// distinction was written for: protocol.IsTerminal("") is TRUE, so a
	// zero-value status used to reach this line and DELETE the junction rows —
	// the per-bin destinations, and the exact rows whose absence made two
	// specimens unreconstructable after the fact (PLAN §R.5, §R.9). An order that
	// could not be read is the last one whose evidence should be thrown away.
	if orderIsTerminal(order) {
		defer e.db.DeleteOrderBins(order.ID)
	}

	if len(instructions) == 0 {
		e.dbg("multi-bin completion: order %d all bins already at dest — skipping arrival", order.ID)
		return
	}

	evictedGhosts, err := e.db.ApplyMultiBinArrival(order.ID, instructions)
	if err != nil {
		e.logFn("engine: multi-bin arrival for order %d: %v", order.ID, err)
		return
	}
	for _, ghostID := range evictedGhosts {
		e.logFn("WARN: multi-bin arrival for order %d evicted a stale bin record (bin %d) to _TRANSIT — a delivery cannot physically complete onto an occupied slot; recover via the anomalies page",
			order.ID, ghostID)
	}
	// The burial shadow instrument — same reason as the delivery-side multi-bin
	// arrival above: this path does not go through BinService.ApplyArrival.
	for _, inst := range instructions {
		e.binService.NoteBurialShadow(inst.BinID, inst.ToNodeID, order.ID)
	}

	// Emit BinUpdatedEvent only for bins that actually moved
	for i, inst := range instructions {
		bin, err := e.db.GetBin(inst.BinID)
		if err != nil {
			e.logFn("engine: get bin %d for multi-bin event: %v", inst.BinID, err)
			continue
		}
		e.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
			Action:      "moved",
			BinID:       bin.ID,
			PayloadCode: bin.PayloadCode,
			FromNodeID:  fromNodeIDs[i],
			ToNodeID:    inst.ToNodeID,
			NodeID:      inst.ToNodeID,
		}})
	}

	e.logFn("engine: order %d multi-bin completion: %d bins moved", order.ID, len(instructions))
}
