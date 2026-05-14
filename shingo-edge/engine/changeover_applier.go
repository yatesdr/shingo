package engine

import (
	"log"

	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
)

// applyChangeoverPlan creates orders for each NodeAction in the plan, links
// them to their changeover node tasks, and advances the task state.
//
// Error handling policy: log and continue. A failure on one node must not
// abort the rest of the changeover. See 2567plandiscussion.md.
func (e *Engine) applyChangeoverPlan(co *processes.Changeover, plan changeover.Plan) {
	for _, action := range plan.Actions {
		nodeTask, err := e.db.GetChangeoverNodeTaskByNode(co.ID, action.NodeID)
		if err != nil {
			log.Printf("changeover: cannot find node task for %s: %v", action.NodeName, err)
			continue
		}
		if action.Err != nil {
			log.Printf("changeover: auto-create orders for %s (%s): %v — operator must handle manually",
				action.NodeName, action.Situation, action.Err)
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error"); err != nil {
				log.Printf("changeover: update node task %d state to error: %v", nodeTask.ID, err)
			}
			continue
		}
		e.applyNodeAction(nodeTask, action)
	}
}

func (e *Engine) applyNodeAction(nodeTask *processes.NodeTask, action changeover.NodeAction) {
	nodeID := action.NodeID

	var supplyID, evacID *int64
	if action.SupplyOrder != nil {
		id, err := e.createPlannedOrder(nodeID, action.SupplyOrder)
		if err != nil {
			log.Printf("changeover: auto-create orders for %s (%s): create supply order: %v — operator must handle manually",
				action.NodeName, action.Situation, err)
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error"); err != nil {
				log.Printf("changeover: update node task %d state to error: %v", nodeTask.ID, err)
			}
			return
		}
		supplyID = &id
	}
	if action.EvacOrder != nil {
		id, err := e.createPlannedOrder(nodeID, action.EvacOrder)
		if err != nil {
			log.Printf("changeover: auto-create orders for %s (%s): create evac order: %v — operator must handle manually",
				action.NodeName, action.Situation, err)
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error"); err != nil {
				log.Printf("changeover: update node task %d state to error: %v", nodeTask.ID, err)
			}
			return
		}
		evacID = &id
	}

	if supplyID != nil || evacID != nil {
		if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, supplyID, evacID); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
	}
	// Durable supply ↔ evac sibling linkage for two-robot swap pairs.
	// Mirrors operator_stations.go:134 (the operator-initiated path) —
	// without it, isSupplyOrderInTwoRobotSwap can't identify the supply
	// leg via SiblingOrderID, and the supply_bin_guard at
	// operator_release.go:246-256 misses. Plant 2026-05-11 (SNF2 ALN_001):
	// changeover-driven two-robot swap's supply bin (3600 parts) was
	// wiped on a per-order admin release because the guard couldn't
	// identify it as supply without the sibling pointer.
	//
	// Same fingerprint as the 2026-04-23 ALN_002 incident
	// (operator_release.go:497-498), fixed for the operator-initiated
	// path but never backported here.
	// LinkOrderSiblings is log-and-continue here (unlike the three
	// operator-initiated sites in operator_stations.go / operator_bin_ops.go
	// / operator_produce.go which return-error). Rationale:
	//   - Orders are already persisted by createPlannedOrder above; we'd
	//     need a rollback to abort cleanly.
	//   - applyChangeoverPlan iterates per-node and a single node's
	//     failure must not abort the rest of the plan (see comment at
	//     applyChangeoverPlan: "log and continue").
	// Residual risk: a silent linkage failure here leaves a changeover
	// two-robot pair without sibling pointers, which makes
	// ComputeSwapReady return false (operator gets WAITING FOR OTHER
	// ROBOT with no escape). SHINGO_TODO.md "Residual risk" entry tracks
	// the mitigation (on-read repair or startup audit).
	if supplyID != nil && evacID != nil {
		if err := e.db.LinkOrderSiblings(*supplyID, *evacID); err != nil {
			log.Printf("changeover: link order siblings %d↔%d for node task %d: %v",
				*supplyID, *evacID, nodeTask.ID, err)
		}
	}
	if action.NextState != "" {
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, action.NextState); err != nil {
			log.Printf("changeover: update node task %d state to %s: %v", nodeTask.ID, action.NextState, err)
		}
	}

	logChangeoverAction(action, supplyID, evacID)
}

func (e *Engine) createPlannedOrder(nodeID int64, spec *changeover.OrderSpec) (int64, error) {
	switch {
	case spec.Complex != nil:
		return e.createComplexFromSpec(nodeID, spec.Complex)
	case spec.Retrieve != nil:
		return e.createRetrieveFromSpec(nodeID, spec.Retrieve)
	}
	return 0, nil
}

func (e *Engine) createComplexFromSpec(nodeID int64, c *changeover.ComplexOrderSpec) (int64, error) {
	o, err := e.orderMgr.CreateComplexOrderWithPayload(&nodeID, 1, c.DeliveryNode, c.ProcessNode, c.Steps, c.AutoConfirm, c.PayloadCode)
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

func (e *Engine) createRetrieveFromSpec(nodeID int64, r *changeover.RetrieveOrderSpec) (int64, error) {
	o, err := e.orderMgr.CreateRetrieveOrder(&nodeID, r.RetrieveEmpty, 1, r.DeliveryNode, r.SourceNode, r.StagingNode, r.LoadType, r.PayloadCode, r.AutoConfirm, false)
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

func logChangeoverAction(action changeover.NodeAction, supplyID, evacID *int64) {
	switch action.LogTag {
	case "swap":
		log.Printf("changeover: swap node %s — supply=%d (staging), evac=%d (swap w/ wait)", action.NodeName, derefID(supplyID), derefID(evacID))
	case "evacuate":
		log.Printf("changeover: evacuate node %s — supply=%d (staging), evac=%d (evacuate w/ 2 waits)", action.NodeName, derefID(supplyID), derefID(evacID))
	case "drop":
		log.Printf("changeover: drop node %s — evac=%d (single-robot release w/ staged wait)", action.NodeName, derefID(evacID))
	case "keep_staged_split":
		log.Printf("changeover: keep-staged split node %s — supply=%d (deliver w/ wait), evac=%d (evac w/ wait)", action.NodeName, derefID(supplyID), derefID(evacID))
	case "keep_staged_combined":
		log.Printf("changeover: keep-staged combined node %s — supply=%d (combined w/ wait), evac=%d (evac w/ wait)", action.NodeName, derefID(supplyID), derefID(evacID))
	}
}

func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}
