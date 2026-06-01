// wiring_counter_delta.go — counter-delta UOP tracking, auto-reorder/relief,
// A/B paired-node cycling, and the lineside drain that consume ticks
// run before touching the node counter.
//
// Subscribed via wireEventHandlers (wiring.go) on EventCounterDelta;
// dispatches to consume / produce / fallback handlers via handleCounterDelta.

package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// handleCounterDelta processes a production counter tick:
// - For consume nodes: decrement remaining UOP, trigger auto-reorder if at threshold
// - For produce nodes: increment remaining UOP, trigger auto-relief if at capacity
//
// The orchestrator handles validation, per-process node iteration, A/B-pair
// coordination, and dispatches to role-specific helpers. The actual UOP
// arithmetic, lineside drain, and auto-reorder/auto-relief decisions live in
// handleConsumeTick / handleProduceTick / handleABFallthrough.
func (e *Engine) handleCounterDelta(delta CounterDeltaEvent) {
	if delta.ProcessID == 0 || delta.StyleID == 0 || delta.Delta <= 0 {
		return
	}
	if delta.Anomaly == "reset" {
		return
	}

	nodes, err := e.db.ListProcessNodesByProcess(delta.ProcessID)
	if err != nil {
		return
	}
	// A/B fallthrough tracking: if all paired consume nodes are inactive,
	// decrement the first one found as a safety net ("count to lineside storage").
	var pairedFallbackNode *processes.Node
	var pairedFallbackRuntime *processes.RuntimeState
	var pairedFallbackClaim *processes.NodeClaim
	pairedConsumeHandled := false

	for _, node := range nodes {
		runtime, err := e.db.GetProcessNodeRuntime(node.ID)
		if err != nil || runtime == nil {
			continue
		}

		// Look up active claim for this node
		claim := findActiveClaim(e.db, &node)
		if claim == nil {
			continue
		}
		// Only process nodes with a claim matching this style
		if claim.StyleID != delta.StyleID {
			continue
		}

		// manual_swap nodes (loader/unloader) are forklift-managed
		// staging points, not production cells. They have no PLC tags
		// directly tied to their bin contents — the line's PLC counts
		// the parts. Skip cache decrement and Core-side bin delta
		// emission for these nodes so a forklift-loaded bin's manifest
		// doesn't drift from the operator-declared count.
		if claim.SwapMode == protocol.SwapModeManualSwap {
			continue
		}

		switch claim.Role {
		case protocol.ClaimRoleConsume:
			// A/B cycling: only decrement the active-pull side.
			// The inactive side holds staged material.
			if isInactivePairedNode(claim, runtime) {
				// Remember first inactive paired node as fallback
				if pairedFallbackNode == nil {
					nodeCopy := node
					pairedFallbackNode = &nodeCopy
					pairedFallbackRuntime = runtime
					// Capture the claim that just passed the
					// claim.StyleID == delta.StyleID guard above so the
					// fallback emits against the tick's style, not a
					// re-derived one (R43-1).
					pairedFallbackClaim = claim
				}
				continue
			}
			if claim.PairedCoreNode != "" {
				pairedConsumeHandled = true
			}
			nodeCopy := node
			e.handleConsumeTick(&nodeCopy, runtime, claim, int(delta.Delta))

		case protocol.ClaimRoleProduce:
			// A/B cycling: only increment the active-pull side.
			// The inactive side holds its current production.
			if isInactivePairedNode(claim, runtime) {
				continue
			}
			nodeCopy := node
			e.handleProduceTick(&nodeCopy, runtime, claim, int(delta.Delta))
		}
	}

	// A/B fallthrough: if no paired consume node was active but we found
	// an inactive paired node, decrement it as a safety net. This covers
	// the "count to lineside storage" case when neither A nor B is active.
	if !pairedConsumeHandled && pairedFallbackNode != nil && pairedFallbackRuntime != nil {
		e.handleABFallthrough(delta.ProcessID, pairedFallbackNode, pairedFallbackRuntime, pairedFallbackClaim, int(delta.Delta))
	}
}

// handleConsumeTick applies a delta to one active-pull consume node:
// drain lineside first, decrement node UOP, then trigger auto-reorder
// if the threshold is crossed and the node accepts orders. Caller is
// responsible for the A/B inactive-pair check; this is invoked only
// on the active side.
//
// Post-May-4 (commit 6d226d1) Edge is authoritative for the count of
// any bin physically at one of its nodes. The local UpdateProcessNodeUOP
// write is the durable truth for at-node bins, not a write-through
// cache — there is no reconciler healing back from Core. Core mirrors
// via the bucket and bin deltas published below. If a delta is rejected
// at Core (e.g., payload_code mismatch), FlushFailures surfaces the
// drift; no automatic heal exists.
//
// Per SME lock (open-items.md §"Process semantics"): bins can go
// negative. A real bin nominally rated 1000 might overpack to 1005
// (operator runs an extra cycle before noticing); the next bin
// underpacks to 995. Over time these wash out at the inventory
// aggregate level. Tracking the actual count — including signed
// values — is the whole point of bin-as-truth. The runtime cache
// must mirror this; clamping at zero would force a permanent drift
// against Core (which doesn't clamp) and noisy reconciliation logs
// as the heal/clamp/heal/clamp loop ping-pongs forever.
func (e *Engine) handleConsumeTick(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, delta int) {
	// Lineside first: drain the active bucket for this node's primary part
	// before touching the node counter. The bucket represents parts the
	// operator pulled to lineside during the last swap, which physically
	// leave the station before the new bin is tapped. Remainder flows to
	// the bin counter.
	drains, binRemainder := e.drainLinesideFirst(node.ID, claim, delta)

	// Hold-and-replay. The count follows the bin physically at the slot
	// (active_bin_id). When a bin is bound we apply this tick PLUS any
	// counts held while the slot was empty (pending_uop_delta), then clear
	// the hold. When no bin is bound (the pickup→delivery gap), we hold the
	// bin portion so it lands on the next bin instead of being lost or
	// charged to a departed bin. The lineside drain emits every tick
	// regardless — parts leaving the rack are independent of which bin is
	// at the slot.
	bound := runtime.ActiveBinID != nil
	newRemaining := runtime.RemainingUOPCached
	binAttributed := binRemainder
	if bound {
		binAttributed = binRemainder + int(runtime.PendingUOPDelta)
		newRemaining = runtime.RemainingUOPCached - binAttributed
		if runtime.PendingUOPDelta != 0 {
			if err := e.db.SetProcessNodeUOPClearPending(node.ID, newRemaining); err != nil {
				log.Printf("update UOP (replay pending) for node %d: %v", node.ID, err)
			}
		} else if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
			log.Printf("update UOP for node %d: %v", node.ID, err)
		}
	} else if binRemainder != 0 {
		if err := e.db.AddPendingUOPDelta(node.ID, binRemainder); err != nil {
			log.Printf("hold pending UOP for node %d: %v", node.ID, err)
		}
	}

	// emitConsumeTickDeltas emits the lineside-drain bucket deltas always,
	// and a bin delta for binAttributed — which binAtNode skips when no bin
	// is bound (binID 0), so the held portion isn't double-emitted; it
	// ships on the bound tick that replays it.
	e.emitConsumeTickDeltas(node, runtime, claim, drains, binAttributed)

	// Auto-reorder if threshold reached, enabled, and node can accept orders.
	// During the gap newRemaining is unchanged so the threshold isn't crossed
	// twice for the same supply event.
	//
	// UOP-threshold replenishment (Phase 1): the existing condition
	// (newRemaining <= ReorderPoint && newRemaining > 0) is logically
	// unsatisfiable when ReorderPoint = 0, so the path was silent-inert
	// for plants that never set a value. The explicit `ReorderPoint > 0`
	// gate makes the opt-in semantic visible without changing behaviour
	// — any plant with a 0 threshold sees identical behaviour to before.
	// Diagnostic log line fires on every consume tick where AutoReorder
	// is on, so engineers can see exactly why nothing fires (gate=insteady
	// _state_gap during release window, gate=below_floor when the
	// remaining count is already at 0, etc.).
	if claim.AutoReorder {
		if !bound {
			e.debugFn("autoreorder eval: claim=%d node=%s gate=no_bin_bound (pickup-to-delivery gap; ticks held)",
				claim.ID, node.Name)
		} else if claim.ReorderPoint <= 0 {
			e.debugFn("autoreorder eval: claim=%d node=%s gate=opt_out (reorder_point=0) — legacy silent-inert path",
				claim.ID, node.Name)
		} else if newRemaining <= 0 {
			e.debugFn("autoreorder eval: claim=%d node=%s remaining=%d threshold=%d gate=at_floor (nothing left to reorder)",
				claim.ID, node.Name, newRemaining, claim.ReorderPoint)
		} else if newRemaining <= claim.ReorderPoint {
			canAccept, reason := e.CanAcceptOrders(node.ID)
			e.logFn("autoreorder eval: claim=%d node=%s remaining=%d threshold=%d canAccept=%v reason=%s gate=consume_tick",
				claim.ID, node.Name, newRemaining, claim.ReorderPoint, canAccept, reason)
			if canAccept {
				if _, err := e.RequestNodeMaterial(node.ID, 1); err != nil {
					log.Printf("auto-reorder for node %s: %v", node.Name, err)
				}
			}
		} else {
			e.debugFn("autoreorder eval: claim=%d node=%s remaining=%d threshold=%d gate=above_threshold",
				claim.ID, node.Name, newRemaining, claim.ReorderPoint)
		}
	}
}

// handleProduceTick applies a delta to one active-pull produce node:
// increment node UOP, then trigger auto-relief (manifest + swap) if the
// claim has a UOP capacity and the new value reaches it. Caller is
// responsible for the A/B inactive-pair check.
//
// Phase 1: emits BinUOPDelta(produce_tick, +delta) for the bin being
// filled. No bucket delta — produce nodes don't drain lineside; they
// fill bins directly via the PLC.
func (e *Engine) handleProduceTick(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, delta int) {
	// Hold-and-replay, mirror of consume. Increment the bin physically at
	// the slot; when none is bound (finalize→new-empty gap) hold the
	// produced parts in pending and replay onto the next empty bin when it
	// binds. The finished-good production tally (EventProducedReport below)
	// is bin-independent and fires every tick regardless.
	bound := runtime.ActiveBinID != nil
	newRemaining := runtime.RemainingUOPCached
	binAttributed := delta
	if bound {
		binAttributed = delta + int(runtime.PendingUOPDelta)
		newRemaining = runtime.RemainingUOPCached + binAttributed
		if runtime.PendingUOPDelta != 0 {
			if err := e.db.SetProcessNodeUOPClearPending(node.ID, newRemaining); err != nil {
				log.Printf("update UOP (replay pending) for node %d: %v", node.ID, err)
			}
		} else if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
			log.Printf("update UOP for node %d: %v", node.ID, err)
		}
	} else if delta != 0 {
		if err := e.db.AddPendingUOPDelta(node.ID, delta); err != nil {
			log.Printf("hold pending UOP for node %d: %v", node.ID, err)
		}
	}

	if e.inventoryDelta != nil && binAttributed > 0 {
		binID, payload, epoch := e.binAtNode(runtime, claim)
		_ = e.inventoryDelta.Produced(uop.TickEvent{
			NodeID:       node.ID,
			StyleID:      claim.StyleID,
			PairKey:      claim.PairedCoreNode,
			BinID:        binID,
			PayloadCode:  payload,
			BinEpoch:     epoch,
			BinRemainder: binAttributed, // this tick + any replayed held parts
		})
	}

	// Report finished-good production to Core keyed by this produce node's
	// payload (the catalog part code demands match on), resolved per node so
	// multi-part styles attribute to the right part. Independent of the
	// inventory delta above — the production reporter subscribes to
	// EventProducedReport. Empty payload = misconfigured node; skip.
	if delta > 0 && claim.PayloadCode != "" {
		e.Events.Emit(Event{Type: EventProducedReport, Payload: ProducedReportEvent{
			PayloadCode: claim.PayloadCode,
			Delta:       int64(delta),
		}})
	}

	// Auto-relief at capacity: finalize the produce node (manifest + swap).
	//
	// UOP-threshold replenishment (Phase 1) diagnostic: same logging
	// shape as the consume side so engineers can see produce-tick
	// evaluation outcomes alongside consume-tick.
	if claim.AutoReorder && claim.UOPCapacity > 0 {
		if !bound {
			e.debugFn("autoreorder eval (produce): claim=%d node=%s gate=no_bin_bound (ticks held)",
				claim.ID, node.Name)
		} else if newRemaining < claim.UOPCapacity {
			e.debugFn("autoreorder eval (produce): claim=%d node=%s remaining=%d capacity=%d gate=below_capacity",
				claim.ID, node.Name, newRemaining, claim.UOPCapacity)
		} else {
			canAccept, reason := e.CanAcceptOrders(node.ID)
			e.logFn("autoreorder eval (produce): claim=%d node=%s remaining=%d capacity=%d canAccept=%v reason=%s gate=produce_tick",
				claim.ID, node.Name, newRemaining, claim.UOPCapacity, canAccept, reason)
			if canAccept {
				if _, err := e.FinalizeProduceNode(node.ID); err != nil {
					log.Printf("auto-relief for produce node %s: %v", node.Name, err)
				}
			}
		}
	}
}

// handleABFallthrough is the safety-net path when no active-pull consume node
// existed for a process tick but at least one inactive paired consume node
// was visible. It decrements the first such fallback node so the count
// flows to lineside storage instead of being dropped on the floor.
//
// Phase 1: emits BinUOPDelta(ab_fallthrough, ...) and bucket deltas
// against the inactive-paired node's bin and active buckets. The plan
// (B5 fix) singles this path out — it's the case where neither
// operator action nor active-pull state surfaces a flush trigger, so
// the periodic flush is the only signal that captures the change.
func (e *Engine) handleABFallthrough(processID int64, node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, delta int) {
	log.Printf("A/B fallthrough: no active-pull node for process %d, decrementing fallback node %s",
		processID, node.Name)

	// claim is the one captured in handleCounterDelta's loop, which
	// already passed the claim.StyleID == delta.StyleID guard. Do NOT
	// re-derive via findActiveClaim here: it prefers the process
	// ActiveStyleID claim, which during a changeover can differ from the
	// tick's style and mis-attribute the lineside drain and bin delta
	// (R43-1).
	var drains map[string]uop.LinesideDrain
	binRemainder := delta
	if claim != nil {
		drains, binRemainder = e.drainLinesideFirst(node.ID, claim, delta)
	}

	// Hold-and-replay, mirror of handleConsumeTick: decrement the bound
	// bin (this tick + any held pending), or hold when no bin is bound.
	bound := runtime.ActiveBinID != nil
	binAttributed := binRemainder
	if bound {
		binAttributed = binRemainder + int(runtime.PendingUOPDelta)
		newRemaining := runtime.RemainingUOPCached - binAttributed
		if runtime.PendingUOPDelta != 0 {
			if err := e.db.SetProcessNodeUOPClearPending(node.ID, newRemaining); err != nil {
				log.Printf("update UOP (replay pending) for node %d: %v", node.ID, err)
			}
		} else if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
			log.Printf("update UOP for node %d: %v", node.ID, err)
		}
	} else if binRemainder != 0 {
		if err := e.db.AddPendingUOPDelta(node.ID, binRemainder); err != nil {
			log.Printf("hold pending UOP for node %d: %v", node.ID, err)
		}
	}

	if claim != nil {
		e.emitFallthroughDeltas(node, runtime, claim, drains, binAttributed)
	}
}

// emitConsumeTickDeltas records the bucket and bin deltas for one
// consume tick. Resolves the bin context via binAtNode, then delegates
// the actual emission to uop.Mutator.Consumed which locks in the
// reason taxonomy (consume_drain + consume_tick).
func (e *Engine) emitConsumeTickDeltas(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, drains map[string]uop.LinesideDrain, binRemainder int) {
	if e.inventoryDelta == nil {
		return
	}
	binID, payload, epoch := e.binAtNode(runtime, claim)
	_ = e.inventoryDelta.Consumed(uop.TickEvent{
		NodeID:       node.ID,
		StyleID:      claim.StyleID,
		PairKey:      claim.PairedCoreNode,
		CoreNodeName: node.CoreNodeName,
		BinID:        binID,
		PayloadCode:  payload,
		BinEpoch:     epoch,
		Drains:       drains,
		BinRemainder: binRemainder,
	})
}

// emitFallthroughDeltas mirrors emitConsumeTickDeltas but routes through
// uop.Mutator.Fallthrough which tags the bin delta with ab_fallthrough
// while keeping consume_drain on the bucket deltas (the bucket
// physically drained regardless of which side of the A/B pair the
// count attributed to).
func (e *Engine) emitFallthroughDeltas(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, drains map[string]uop.LinesideDrain, binRemainder int) {
	if e.inventoryDelta == nil {
		return
	}
	binID, payload, epoch := e.binAtNode(runtime, claim)
	_ = e.inventoryDelta.Fallthrough(uop.TickEvent{
		NodeID:       node.ID,
		StyleID:      claim.StyleID,
		PairKey:      claim.PairedCoreNode,
		CoreNodeName: node.CoreNodeName,
		BinID:        binID,
		PayloadCode:  payload,
		BinEpoch:     epoch,
		Drains:       drains,
		BinRemainder: binRemainder,
	})
}

// binAtNode resolves the bin currently associated with a node tick.
// Returns (0, "") when no bin is tracked at the slot — the caller
// skips bin delta emission in that case.
//
// runtime.ActiveBinID is the canonical "bin physically at this slot"
// pointer. Set on delivery completion (when the bin arrives at the
// node), cleared on pickup (when the bin physically leaves). Edge
// owns this pointer; no order walk is needed. PLC ticks attribute to
// whatever bin is at the slot regardless of which order delivered it
// — covering the gap between order completion and the next order's
// delivery, manual loads, and any other path where a bin is present
// without a tracking order.
//
// payload returns the claim's PayloadCode so Core can validate the
// wire envelope's payload_code against the bin row.
// binAtNode resolves the bin attribution for an emitted delta:
// (binID, payloadCode, epoch). epoch is the bin's load-lifecycle
// epoch — used to stamp the outgoing BinUOPDelta so Core's
// epoch-aware dedup accepts it. Returns (0, "", 0) when no bin is
// at the slot (gap window with active_bin_id nil); caller skips
// bin delta emission in that case.
func (e *Engine) binAtNode(runtime *processes.RuntimeState, claim *processes.NodeClaim) (int64, string, int64) {
	if runtime == nil || runtime.ActiveBinID == nil {
		return 0, "", 0
	}
	return *runtime.ActiveBinID, claim.PayloadCode, runtime.ActiveBinEpoch
}

// drainLinesideFirst decrements the active lineside bucket(s) for the
// claim's parts and returns:
//
//   - drains: per-part qty actually drained from each affected bucket.
//     One entry per (style, part) that drained any non-zero qty.
//     Empty map (not nil-valued — the caller iterates safely) means
//     no bucket drained.
//   - binRemainder: the units that should flow to the node counter
//     after the primary-part drain. The primary's drain reduces
//     binRemainder; secondary drains do not (they keep the UI
//     honest but the node counter is one unit per assembly).
//
// The per-part drains are reported as
// LinesideBucketDelta(consume_drain) by the caller, and the
// binRemainder becomes a BinUOPDelta(consume_tick).
//
// Multi-part claims (claims with more than one entry in
// AllowedPayloads) drain each part by up to delta independently. The
// rationale: the node counter is a single integer (one UOP = one
// assembly), and staging/reorder thresholds key off that value;
// secondary part buckets drain so the UI stays honest, even though
// their draining doesn't affect the node counter arithmetic. If a
// plant ever ships a claim where secondary parts can deplete
// independently (e.g. consumables), revisit this.
func (e *Engine) drainLinesideFirst(nodeID int64, claim *processes.NodeClaim, delta int) (drains map[string]uop.LinesideDrain, binRemainder int) {
	drains = make(map[string]uop.LinesideDrain)
	binRemainder = delta
	if delta <= 0 || claim == nil {
		return drains, binRemainder
	}

	// Primary part controls the node-counter math. Round-3 A*: the
	// matched bucket may carry a style_id that differs from
	// claim.StyleID during a cutover (bucket captured under the
	// outgoing style, drained while the incoming style is now active).
	// Preserve the matched style for downstream LinesideBucketDelta
	// attribution so Core's dedup scope_key (...|<StyleID>|...)
	// matches the bucket's own ID-space.
	if primary := claim.PayloadCode; primary != "" {
		drained, matchedStyleID, err := e.db.DrainLinesideBucket(nodeID, primary, delta)
		if err != nil {
			log.Printf("lineside: drain primary part %q on node %d: %v", primary, nodeID, err)
		} else {
			if drained > 0 {
				drains[primary] = uop.LinesideDrain{Qty: drained, StyleID: matchedStyleID}
			} else {
				e.logUnexpectedDrainMiss(nodeID, primary, "primary")
			}
			binRemainder = delta - drained
		}
	}

	// Secondary parts: drain independently for UI honesty. Skip if they
	// match the primary (avoids a double-drain when AllowedPayloads
	// includes the primary).
	for _, part := range claim.AllowedPayloads() {
		if part == "" || part == claim.PayloadCode {
			continue
		}
		drained, matchedStyleID, err := e.db.DrainLinesideBucket(nodeID, part, delta)
		if err != nil {
			log.Printf("lineside: drain secondary part %q on node %d: %v", part, nodeID, err)
			continue
		}
		if drained > 0 {
			drains[part] = uop.LinesideDrain{Qty: drained, StyleID: matchedStyleID}
		} else {
			e.logUnexpectedDrainMiss(nodeID, part, "secondary")
		}
	}

	return drains, binRemainder
}

// logUnexpectedDrainMiss fires when Drain returned zero qty for a
// (node, part) but ListActiveLinesideBuckets reports at least one
// active bucket present at that node — the "we expected to drain but
// didn't" signal that pre-Round-3 was swallowed by silent
// `log.Printf + continue` (and so didn't surface the original
// styleID-mismatch bug for weeks). After Round-3 A* the WHERE clause
// no longer filters by style, so a drain miss really does mean
// "no matching part_number active here," not "wrong style." Keeping
// the diagnostic anyway because the failure mode is cheap to log and
// useful when investigating future inventory-delta drift.
//
// Best-effort: a DB error on the visibility check is silently ignored
// — the function is purely diagnostic, not load-bearing.
func (e *Engine) logUnexpectedDrainMiss(nodeID int64, partNumber, role string) {
	active, err := e.db.ListActiveLinesideBuckets(nodeID)
	if err != nil {
		return
	}
	if len(active) == 0 {
		// No active buckets at all — drain miss is expected.
		return
	}
	matched := false
	visible := make([]string, 0, len(active))
	for _, b := range active {
		visible = append(visible, b.PartNumber)
		if b.PartNumber == partNumber {
			matched = true
		}
	}
	if !matched {
		// Active buckets at this node, but none match this part —
		// nothing to drain. Don't bother logging unless callers want
		// the visibility detail; the part-vs-bucket mismatch is the
		// normal case (e.g. secondary parts that haven't been pulled
		// this cycle).
		return
	}
	log.Printf("lineside: %s part %q on node %d returned 0 drained despite an active bucket existing — possible drain/capture race (visible parts: %v)",
		role, partNumber, nodeID, visible)
}
