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

	var orderAID, orderBID *int64
	if action.OrderA != nil {
		id, err := e.createPlannedOrder(nodeID, action.OrderA)
		if err != nil {
			log.Printf("changeover: auto-create orders for %s (%s): create order A: %v — operator must handle manually",
				action.NodeName, action.Situation, err)
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error"); err != nil {
				log.Printf("changeover: update node task %d state to error: %v", nodeTask.ID, err)
			}
			return
		}
		orderAID = &id
	}
	if action.OrderB != nil {
		id, err := e.createPlannedOrder(nodeID, action.OrderB)
		if err != nil {
			log.Printf("changeover: auto-create orders for %s (%s): create order B: %v — operator must handle manually",
				action.NodeName, action.Situation, err)
			if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "error"); err != nil {
				log.Printf("changeover: update node task %d state to error: %v", nodeTask.ID, err)
			}
			return
		}
		orderBID = &id
	}

	if orderAID != nil || orderBID != nil {
		if err := e.db.LinkChangeoverNodeOrders(nodeTask.ID, orderAID, orderBID); err != nil {
			log.Printf("changeover: link orders for node task %d: %v", nodeTask.ID, err)
		}
	}
	if action.NextState != "" {
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, action.NextState); err != nil {
			log.Printf("changeover: update node task %d state to %s: %v", nodeTask.ID, action.NextState, err)
		}
	}

	logChangeoverAction(action, orderAID, orderBID)
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
	create := e.orderMgr.CreateComplexOrder
	if c.AutoConfirm {
		create = e.orderMgr.CreateComplexOrderWithAutoConfirm
	}
	o, err := create(&nodeID, 1, c.DeliveryNode, c.Steps)
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

func (e *Engine) createRetrieveFromSpec(nodeID int64, r *changeover.RetrieveOrderSpec) (int64, error) {
	o, err := e.orderMgr.CreateRetrieveOrder(&nodeID, r.RetrieveEmpty, 1, r.DeliveryNode, r.StagingNode, r.LoadType, r.PayloadCode, r.AutoConfirm)
	if err != nil {
		return 0, err
	}
	return o.ID, nil
}

func logChangeoverAction(action changeover.NodeAction, orderAID, orderBID *int64) {
	switch action.LogTag {
	case "swap":
		log.Printf("changeover: swap node %s — Order A=%d (staging), Order B=%d (swap w/ wait)", action.NodeName, derefID(orderAID), derefID(orderBID))
	case "evacuate":
		log.Printf("changeover: evacuate node %s — Order A=%d (staging), Order B=%d (evacuate w/ 2 waits)", action.NodeName, derefID(orderAID), derefID(orderBID))
	case "drop":
		log.Printf("changeover: drop node %s — Order B=%d (evacuation)", action.NodeName, derefID(orderBID))
	case "keep_staged_split":
		log.Printf("changeover: keep-staged split node %s — Order A=%d (deliver w/ wait), Order B=%d (evac w/ wait)", action.NodeName, derefID(orderAID), derefID(orderBID))
	case "keep_staged_combined":
		log.Printf("changeover: keep-staged combined node %s — Order A=%d (combined w/ wait), Order B=%d (evac w/ wait)", action.NodeName, derefID(orderAID), derefID(orderBID))
	}
}

func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}
