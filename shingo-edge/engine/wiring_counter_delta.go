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
		e.handleABFallthrough(delta.ProcessID, pairedFallbackNode, pairedFallbackRuntime, int(delta.Delta))
	}
}

// handleConsumeTick applies a delta to one active-pull consume node:
// drain lineside first, decrement node UOP, then trigger auto-reorder
// if the threshold is crossed and the node accepts orders. Caller is
// responsible for the A/B inactive-pair check; this is invoked only
// on the active side.
//
// The local UpdateProcessNodeUOP write is a write-through cache —
// Core's bins.uop_remaining is the source of truth (post-bin-as-truth
// refactor). The local row backs the operator UI (instant feedback)
// and the reconciler self-heals it from Core periodically. The bucket
// and bin deltas published below are what move authoritative state.
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

	newRemaining := runtime.RemainingUOPCached - binRemainder
	if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
		log.Printf("update UOP for node %d: %v", node.ID, err)
	}

	e.emitConsumeTickDeltas(node.ID, runtime, claim, drains, binRemainder)

	// Auto-reorder if threshold reached, enabled, and node can accept orders.
	if claim.AutoReorder && newRemaining <= claim.ReorderPoint && newRemaining > 0 {
		if ok, _ := e.CanAcceptOrders(node.ID); ok {
			if _, err := e.RequestNodeMaterial(node.ID, 1); err != nil {
				log.Printf("auto-reorder for node %s: %v", node.Name, err)
			}
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
	newRemaining := runtime.RemainingUOPCached + delta
	if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
		log.Printf("update UOP for node %d: %v", node.ID, err)
	}

	if e.inventoryDelta != nil && delta > 0 {
		if binID, payload := e.binAtNode(runtime, claim); binID > 0 {
			e.inventoryDelta.RecordBin(binID, payload, delta, protocol.ReasonProduceTick)
		}
	}

	// Auto-relief at capacity: finalize the produce node (manifest + swap).
	if claim.AutoReorder && claim.UOPCapacity > 0 && newRemaining >= claim.UOPCapacity {
		if ok, _ := e.CanAcceptOrders(node.ID); ok {
			if _, err := e.FinalizeProduceNode(node.ID); err != nil {
				log.Printf("auto-relief for produce node %s: %v", node.Name, err)
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
func (e *Engine) handleABFallthrough(processID int64, node *processes.Node, runtime *processes.RuntimeState, delta int) {
	log.Printf("A/B fallthrough: no active-pull node for process %d, decrementing fallback node %s",
		processID, node.Name)

	// Lineside-first on the fallback node too.
	claim := findActiveClaim(e.db, node)
	var drains map[string]int
	binRemainder := delta
	if claim != nil {
		drains, binRemainder = e.drainLinesideFirst(node.ID, claim, delta)
	}

	// Signed-bin semantic mirrors handleConsumeTick (see comment there).
	newRemaining := runtime.RemainingUOPCached - binRemainder
	if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
		log.Printf("update UOP for node %d: %v", node.ID, err)
	}

	if claim != nil {
		e.emitFallthroughDeltas(node.ID, runtime, claim, drains, binRemainder)
	}
}

// emitConsumeTickDeltas records the bucket and bin deltas for one
// consume tick. Split out from handleConsumeTick so the same emission
// shape is reusable for handleABFallthrough's fallback path (which
// has subtly different reason mapping).
func (e *Engine) emitConsumeTickDeltas(nodeID int64, runtime *processes.RuntimeState, claim *processes.NodeClaim, drains map[string]int, binRemainder int) {
	if e.inventoryDelta == nil {
		return
	}
	pairKey := claim.PairedCoreNode
	for part, drained := range drains {
		if drained > 0 {
			e.inventoryDelta.RecordBucket(nodeID, pairKey, claim.StyleID, part, -drained, protocol.ReasonConsumeDrain)
		}
	}
	if binRemainder > 0 {
		if binID, payload := e.binAtNode(runtime, claim); binID > 0 {
			e.inventoryDelta.RecordBin(binID, payload, -binRemainder, protocol.ReasonConsumeTick)
		}
	}
}

// emitFallthroughDeltas mirrors emitConsumeTickDeltas but tags the
// bin delta with reason=ab_fallthrough. Bucket deltas keep the
// consume_drain reason — the bucket physically drained regardless of
// which side of the A/B pair the count attributed to.
func (e *Engine) emitFallthroughDeltas(nodeID int64, runtime *processes.RuntimeState, claim *processes.NodeClaim, drains map[string]int, binRemainder int) {
	if e.inventoryDelta == nil {
		return
	}
	pairKey := claim.PairedCoreNode
	for part, drained := range drains {
		if drained > 0 {
			e.inventoryDelta.RecordBucket(nodeID, pairKey, claim.StyleID, part, -drained, protocol.ReasonConsumeDrain)
		}
	}
	if binRemainder > 0 {
		if binID, payload := e.binAtNode(runtime, claim); binID > 0 {
			e.inventoryDelta.RecordBin(binID, payload, -binRemainder, protocol.ReasonABFallthrough)
		}
	}
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
func (e *Engine) binAtNode(runtime *processes.RuntimeState, claim *processes.NodeClaim) (int64, string) {
	if runtime == nil || runtime.ActiveBinID == nil {
		return 0, ""
	}
	return *runtime.ActiveBinID, claim.PayloadCode
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
func (e *Engine) drainLinesideFirst(nodeID int64, claim *processes.NodeClaim, delta int) (drains map[string]int, binRemainder int) {
	drains = make(map[string]int)
	binRemainder = delta
	if delta <= 0 || claim == nil {
		return drains, binRemainder
	}

	// Primary part controls the node-counter math.
	if primary := claim.PayloadCode; primary != "" {
		drained, err := e.db.DrainLinesideBucket(nodeID, claim.StyleID, primary, delta)
		if err != nil {
			log.Printf("lineside: drain primary part %q on node %d: %v", primary, nodeID, err)
		} else {
			if drained > 0 {
				drains[primary] = drained
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
		drained, err := e.db.DrainLinesideBucket(nodeID, claim.StyleID, part, delta)
		if err != nil {
			log.Printf("lineside: drain secondary part %q on node %d: %v", part, nodeID, err)
			continue
		}
		if drained > 0 {
			drains[part] = drained
		}
	}

	return drains, binRemainder
}
