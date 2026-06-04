package engine

import (
	"log"

	"shingo/protocol"
)

// HandleUOPAdjustment processes Core's admin-originated UOP adjustment.
// Core validates the value is within [0, payload.UOPCapacity] before
// sending. Edge writes the absolute value directly to the runtime cache
// and emits EventUOPAdjusted so the operator screen refreshes via SSE.
//
// PLC ticks accumulate from the new value naturally — no accumulator
// involvement.
func (e *Engine) HandleUOPAdjustment(adj protocol.UOPAdjustment) {
	node, err := e.db.GetProcessNodeByCoreNodeName(adj.CoreNodeName)
	if err != nil || node == nil {
		log.Printf("uop_adjustment: process node %q not found: %v", adj.CoreNodeName, err)
		return
	}

	rt, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil || rt == nil {
		log.Printf("uop_adjustment: runtime for node %s (id=%d) not found: %v", adj.CoreNodeName, node.ID, err)
		return
	}

	if rt.ActiveBinID == nil || *rt.ActiveBinID != adj.BinID {
		log.Printf("uop_adjustment: bin %d not active at node %s (active_bin_id=%v) — stale adjustment",
			adj.BinID, adj.CoreNodeName, rt.ActiveBinID)
		return
	}

	if err := e.db.UpdateProcessNodeUOP(node.ID, adj.NewRemaining); err != nil {
		log.Printf("uop_adjustment: write remaining_uop=%d for node %s: %v", adj.NewRemaining, adj.CoreNodeName, err)
		return
	}

	e.Events.Emit(Event{Type: EventUOPAdjusted, Payload: UOPAdjustedEvent{
		ProcessNodeID: node.ID,
		CoreNodeName:  adj.CoreNodeName,
		BinID:         adj.BinID,
		NewRemaining:  adj.NewRemaining,
		Actor:         adj.Actor,
	}})
}
