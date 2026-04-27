// wiring_counter_delta.go — counter-delta UOP tracking, auto-reorder/relief,
// A/B paired-node cycling, and the lineside drain that consume ticks
// run before touching the node counter.
//
// Subscribed via wireEventHandlers (wiring.go) on EventCounterDelta;
// dispatches to consume / produce / fallback handlers via handleCounterDelta.

package engine

import (
	"log"

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
		case "consume":
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

		case "produce":
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
// drain lineside first, decrement node UOP (clamped at zero), then trigger
// auto-reorder if the threshold is crossed and the node accepts orders.
// Caller is responsible for the A/B inactive-pair check; this is invoked
// only on the active side.
func (e *Engine) handleConsumeTick(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, delta int) {
	// Lineside first: drain the active bucket for this node's primary part
	// before touching the node counter. The bucket represents parts the
	// operator pulled to lineside during the last swap, which physically
	// leave the station before the new bin is tapped. Remainder flows to
	// the bin counter.
	remainder := e.drainLinesideFirst(node.ID, claim, delta)

	newRemaining := runtime.RemainingUOP - remainder
	if newRemaining < 0 {
		newRemaining = 0
	}
	if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
		log.Printf("update UOP for node %d: %v", node.ID, err)
	}

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
func (e *Engine) handleProduceTick(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, delta int) {
	newRemaining := runtime.RemainingUOP + delta
	if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
		log.Printf("update UOP for node %d: %v", node.ID, err)
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
func (e *Engine) handleABFallthrough(processID int64, node *processes.Node, runtime *processes.RuntimeState, delta int) {
	log.Printf("A/B fallthrough: no active-pull node for process %d, decrementing fallback node %s",
		processID, node.Name)

	// Lineside-first on the fallback node too.
	claim := findActiveClaim(e.db, node)
	remainder := delta
	if claim != nil {
		remainder = e.drainLinesideFirst(node.ID, claim, delta)
	}

	newRemaining := runtime.RemainingUOP - remainder
	if newRemaining < 0 {
		newRemaining = 0
	}
	if err := e.db.UpdateProcessNodeUOP(node.ID, newRemaining); err != nil {
		log.Printf("update UOP for node %d: %v", node.ID, err)
	}
}

// drainLinesideFirst decrements the active lineside bucket(s) for the
// claim's parts and returns the remainder that should flow to the node
// counter. For a single-part claim, the primary PayloadCode is used;
// the remainder is delta - drained.
//
// Multi-part claims (claims with more than one entry in AllowedPayloads)
// drain each part by up to delta independently, but the node counter
// still represents one unit of production per tick, so the remainder
// tracks the *primary* part's drain. The rationale: the node counter is
// a single integer (one UOP = one assembly), and staging/reorder
// thresholds key off that value. Secondary part buckets drain so the
// UI stays honest, even though their draining doesn't affect the node
// counter arithmetic. If a plant ever ships a claim where secondary
// parts can deplete independently (e.g. consumables), revisit this.
func (e *Engine) drainLinesideFirst(nodeID int64, claim *processes.NodeClaim, delta int) int {
	if delta <= 0 || claim == nil {
		return delta
	}

	// Primary part controls the node-counter math.
	primaryRemainder := delta
	if primary := claim.PayloadCode; primary != "" {
		drained, err := e.db.DrainLinesideBucket(nodeID, claim.StyleID, primary, delta)
		if err != nil {
			log.Printf("lineside: drain primary part %q on node %d: %v", primary, nodeID, err)
		} else {
			primaryRemainder = delta - drained
		}
	}

	// Secondary parts: drain independently for UI honesty. Skip if they
	// match the primary (avoids a double-drain when AllowedPayloads
	// includes the primary).
	for _, part := range claim.AllowedPayloads() {
		if part == "" || part == claim.PayloadCode {
			continue
		}
		if _, err := e.db.DrainLinesideBucket(nodeID, claim.StyleID, part, delta); err != nil {
			log.Printf("lineside: drain secondary part %q on node %d: %v", part, nodeID, err)
		}
	}

	return primaryRemainder
}
