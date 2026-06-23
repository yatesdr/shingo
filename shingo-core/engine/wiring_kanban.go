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

// TODO(kanban-eval): this demand-signal path (handleKanbanDemand →
// sendDemandSignals → DemandSignal → edge MaybeCreate{Loader,Unloader}*) is
// WIRED but DORMANT in practice. At Hopkinsville (2026-06-17) every
// BinUpdatedEvent skips or suppresses: empties are ignored, and the live press
// flow delivers fulls/empties via complex orders that bypass the kanban
// auto-replenish/drain entirely. DECIDE: remove as dead code, or finish wiring
// and adopt it. Study the produce (loader empty-in) and consume (unloader
// full-in) paths SEPARATELY — produce may be live at other plants, consume is
// dormant; verify other plants (e.g. Springfield) before any removal.
// NB: do NOT fold isStorageSlot into this decision — it is shared with
// arrival-staging (resolveNodeStaging) and is load-bearing regardless.
//
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
		e.sendDemandSignals(ev.PayloadCode, protocol.ClaimRoleProduce,
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
		e.sendDemandSignals(ev.PayloadCode, protocol.ClaimRoleConsume,
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

// isStorageSlot returns true if the node is a storage slot — either a
// LANE/NGRP itself, a direct child of one, or a dedicated loader home/buffer
// position. NGRP children added 2026-04-29: plants modeling supermarkets as
// an NGRP → direct concrete children (no LANE in the path) need those
// children treated as storage so arriving bins land `available`, not `staged`.
// Lineside cells remain parented under processes/zones and continue to stage
// on arrival.
//
// Dedicated loader home/buffer positions (bin_loader_homes) are always
// storage-like: the loader aggregate owns their inventory, bins should arrive
// available so the threshold monitor and swap planner see them correctly.
// These nodes are parentless (no LANE/NGRP in their lineage), so the
// bin_loader_homes check must come before the parentless early-return.
//
// The string was "NODE_GROUP" until the SMKT→NGRP rename (commit 3e3fb4a)
// dropped the legacy code — anything still comparing to "NODE_GROUP" is
// a dead branch.
func (e *Engine) isStorageSlot(nodeID int64) bool {
	node, err := e.db.GetNode(nodeID)
	if err != nil {
		return false
	}
	if node.NodeTypeCode == protocol.NodeClassLANE || node.NodeTypeCode == protocol.NodeClassNGRP {
		return true
	}
	if node.ParentID == nil {
		// Parentless nodes default to lineside (staged arrivals) unless this is a
		// dedicated loader home or buffer position — those are storage-like.
		home, herr := e.db.GetLoaderHomeByPositionNode(nodeID)
		return herr == nil && home != nil
	}
	parent, err := e.db.GetNode(*node.ParentID)
	if err != nil {
		return false
	}
	return parent.NodeTypeCode == protocol.NodeClassLANE || parent.NodeTypeCode == protocol.NodeClassNGRP
}

// TODO(kanban-eval): part of the dormant demand-signal path — see the eval note
// on handleKanbanDemand before removing or rewiring this.
//
// sendDemandSignals looks up the demand registry for the given payload code and role,
// then sends a DemandSignal to each matching Edge station.
func (e *Engine) sendDemandSignals(payloadCode string, role protocol.ClaimRole, reason string) {
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
