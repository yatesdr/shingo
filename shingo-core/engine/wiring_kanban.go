// wiring_kanban.go — Kanban demand signalling.
//
// handleKanbanDemand examines bin-move events and, when a bin enters or
// leaves a storage slot (child of a LANE node), looks up the demand
// registry for the payload code and pushes a DemandSignal to each
// matching Edge station. Producers get "produce" signals when supply
// drops; consumers get "consume" signals when supply arrives.

package engine

import (
	"fmt"

	"shingo/protocol"
)

// handleKanbanDemand checks if a bin event at a storage node should trigger
// demand signals to Edge stations via the demand registry.
//
// Kanban triggers:
//   - Bin moved FROM a storage slot → supply decreased → signal "produce" stations to replenish
//   - Bin moved TO a storage slot   → supply increased → signal "consume" stations that material is available
func (e *Engine) handleKanbanDemand(ev BinUpdatedEvent) {
	if ev.PayloadCode == "" {
		e.dbg("kanban: skipped bin=%d — empty payload, action=%s", ev.BinID, ev.Action)
		return
	}

	// Only bin movements trigger kanban demand.
	if ev.Action != "moved" {
		e.dbg("kanban: skipped bin=%d — action=%q (want moved), payload=%s", ev.BinID, ev.Action, ev.PayloadCode)
		return
	}

	fromMatched := ev.FromNodeID != 0 && e.isStorageSlot(ev.FromNodeID)
	toMatched := ev.ToNodeID != 0 && e.isStorageSlot(ev.ToNodeID)

	// Bin left a storage slot → supply decreased → tell producers to replenish.
	if fromMatched {
		e.sendDemandSignals(ev.PayloadCode, "produce",
			fmt.Sprintf("bin %d removed from storage (payload %s)", ev.BinID, ev.PayloadCode))
	} else if ev.FromNodeID != 0 {
		// Non-storage origin: produce signal intentionally suppressed.
		// Surface at INFO so claim-misconfiguration (consume-role claim
		// on a non-LANE node, or produce-role on a lineside cell) is
		// visible without enabling engine debug.
		e.dbg("kanban: suppressed produce signal for bin=%d payload=%s from=%d (parent not LANE)", ev.BinID, ev.PayloadCode, ev.FromNodeID)
	}

	// Bin arrived at a storage slot → supply increased → tell consumers material is available.
	if toMatched {
		e.sendDemandSignals(ev.PayloadCode, "consume",
			fmt.Sprintf("bin %d arrived at storage (payload %s)", ev.BinID, ev.PayloadCode))
	} else if ev.ToNodeID != 0 {
		// Non-storage destination: consume signal intentionally suppressed.
		// Same diagnostic motivation as the produce branch above.
		e.dbg("kanban: suppressed consume signal for bin=%d payload=%s to=%d (parent not LANE)", ev.BinID, ev.PayloadCode, ev.ToNodeID)
	}

	// Log when neither endpoint matched — helps diagnose NGRP-direct and non-storage moves.
	if !fromMatched && !toMatched {
		e.dbg("kanban: no storage match for bin=%d payload=%s from=%d to=%d", ev.BinID, ev.PayloadCode, ev.FromNodeID, ev.ToNodeID)
	}
}

// isStorageSlot returns true if the node is a storage slot (child of a LANE node).
func (e *Engine) isStorageSlot(nodeID int64) bool {
	node, err := e.db.GetNode(nodeID)
	if err != nil || node.ParentID == nil {
		return false
	}
	parent, err := e.db.GetNode(*node.ParentID)
	if err != nil {
		return false
	}
	return parent.NodeTypeCode == "LANE"
}

// sendDemandSignals looks up the demand registry for the given payload code and role,
// then sends a DemandSignal to each matching Edge station.
func (e *Engine) sendDemandSignals(payloadCode, role, reason string) {
	entries, err := e.db.LookupDemandRegistry(payloadCode)
	if err != nil {
		e.logFn("engine: kanban demand registry lookup for %s: %v", payloadCode, err)
		return
	}

	for _, entry := range entries {
		if entry.Role != role {
			continue
		}
		signal := &protocol.DemandSignal{
			CoreNodeName: entry.CoreNodeName,
			PayloadCode:  payloadCode,
			Role:         role,
			Reason:       reason,
		}
		if err := e.SendDataToEdge(protocol.SubjectDemandSignal, entry.StationID, signal); err != nil {
			e.logFn("engine: send demand signal to %s for %s: %v", entry.StationID, payloadCode, err)
		} else {
			e.dbg("kanban: sent demand signal to %s: node=%s payload=%s role=%s",
				entry.StationID, entry.CoreNodeName, payloadCode, role)
		}
	}
}
