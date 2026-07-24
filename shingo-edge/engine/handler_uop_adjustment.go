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
//
// When adj.Released is set (Core moved the bin off this node via admin
// Move), Edge instead CLEARS the node's active_bin_id so its PLC ticks stop
// attributing consumption to a bin that has left — fixing the "moved bin
// keeps counting down" bug. NewRemaining is ignored in that case.
//
// When adj.Bound is set (Core moved the bin ONTO this node via admin Move),
// Edge instead BINDS the node's runtime to the bin — active_bin_id, the epoch
// from adj.Epoch, and the cached UOP from adj.NewRemaining — so its PLC ticks
// resume counting the arrived bin. The dual of Released. This runs ahead of
// the active-bin guard below, because the destination is blank (or stale) by
// definition and that guard would otherwise reject it; Core's Move already
// refused the relocation if the destination held another bin, so any stale
// pointer is safe to overwrite.
//
// A plain count correction (neither Bound nor Released) addressed to a node
// with NO bin bound also binds: the carrier is staged there and Edge never
// bound it (the SNF3 detachment). Rather than throwing the correction away as
// stale, Edge seeds the runtime with the corrected value + adj.Epoch so the bin
// binds and its ticks resume (P2-C5). A correction naming a bin that a DIFFERENT
// bin already occupies is still rejected — it is for a bin bound elsewhere.
// On Released, Edge also blanks the cached count so the operator tile never
// shows a dead number for the now-empty slot.
func (e *Engine) HandleUOPAdjustment(adj protocol.UOPAdjustment) {
	node, err := e.db.GetProcessNodeByCoreNodeName(adj.CoreNodeName)
	if err != nil || node == nil {
		log.Printf("uop_adjustment: process node %q not found: %v", adj.CoreNodeName, err)
		return
	}

	if adj.Bound {
		// Bind the destination's runtime to the moved bin. EnsureProcessNodeRuntime
		// because a never-active destination may have no runtime row yet.
		// rt.ActiveClaimID is preserved — the move changes which bin sits at the
		// slot, not what the node produces/consumes.
		rt, err := e.db.EnsureProcessNodeRuntime(node.ID)
		if err != nil || rt == nil {
			log.Printf("uop_adjustment: bind bin %d — ensure runtime for node %s: %v", adj.BinID, adj.CoreNodeName, err)
			return
		}
		if rt.ActiveBinID != nil && *rt.ActiveBinID != adj.BinID {
			log.Printf("uop_adjustment: bind bin %d onto node %s overwrote stale active_bin_id=%d (Core moved destination to empty)",
				adj.BinID, adj.CoreNodeName, *rt.ActiveBinID)
		}
		if err := e.db.SetProcessNodeRuntimeWithBinAndEpoch(node.ID, rt.ActiveClaimID, &adj.BinID, adj.Epoch, adj.NewRemaining); err != nil {
			log.Printf("uop_adjustment: bind active bin %d to node %s: %v", adj.BinID, adj.CoreNodeName, err)
			return
		}
		log.Printf("uop_adjustment: bound bin %d to node %s (remaining=%d epoch=%d, moved in Core)",
			adj.BinID, adj.CoreNodeName, adj.NewRemaining, adj.Epoch)
		e.Events.Emit(Event{Type: EventUOPAdjusted, Payload: UOPAdjustedEvent{
			ProcessNodeID: node.ID,
			CoreNodeName:  adj.CoreNodeName,
			BinID:         adj.BinID,
			NewRemaining:  adj.NewRemaining,
			Actor:         adj.Actor,
		}})
		return
	}

	rt, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil || rt == nil {
		log.Printf("uop_adjustment: runtime for node %s (id=%d) not found: %v", adj.CoreNodeName, node.ID, err)
		return
	}

	if rt.ActiveBinID == nil && !adj.Released {
		// The node has no bin bound but Core is correcting THIS bin's count for
		// THIS node — i.e. the carrier is staged here and Edge never bound it (the
		// SNF3 detachment: a delivered bin whose ticks stranded). Bind it with the
		// corrected value via the same machinery the Bound=true path uses, so its
		// PLC ticks resume against the right count and epoch. This is the front
		// door that repairs a wrong tile — required per §4b-6. (A Released
		// correction on an already-unbound node has nothing to clear; it falls
		// through to the guard below and no-ops.)
		if err := e.db.SetProcessNodeRuntimeWithBinAndEpoch(node.ID, rt.ActiveClaimID, &adj.BinID, adj.Epoch, adj.NewRemaining); err != nil {
			log.Printf("uop_adjustment: bind staged bin %d to node %s via count correction: %v", adj.BinID, adj.CoreNodeName, err)
			return
		}
		log.Printf("uop_adjustment: bound staged bin %d to node %s via count correction (remaining=%d epoch=%d)",
			adj.BinID, adj.CoreNodeName, adj.NewRemaining, adj.Epoch)
		e.Events.Emit(Event{Type: EventUOPAdjusted, Payload: UOPAdjustedEvent{
			ProcessNodeID: node.ID,
			CoreNodeName:  adj.CoreNodeName,
			BinID:         adj.BinID,
			NewRemaining:  adj.NewRemaining,
			Actor:         adj.Actor,
		}})
		return
	}

	if rt.ActiveBinID == nil || *rt.ActiveBinID != adj.BinID {
		// A different bin is bound here (or nothing is, on a Released path) — the
		// correction is for a bin bound somewhere else. Keep rejecting: never evict
		// or overwrite the bin physically present at this node.
		log.Printf("uop_adjustment: bin %d not active at node %s (active_bin_id=%v) — bound elsewhere, ignoring",
			adj.BinID, adj.CoreNodeName, rt.ActiveBinID)
		return
	}

	if adj.Released {
		// Core moved this bin off CoreNodeName (admin Move). Clear this node's
		// active_bin_id so its PLC ticks stop attributing consumption to a bin
		// that has physically left — the "moved bin keeps counting down" bug —
		// AND blank the cached count so the operator tile never reads a dead
		// number for an empty slot (a stale count on an unbound slot is the
		// "which one is right" confusion the SNF3 write-up called out). The guard
		// above already confirmed this node still points at the bin.
		if err := e.db.SetProcessNodeActiveBinID(node.ID, nil); err != nil {
			log.Printf("uop_adjustment: release active bin %d from node %s: %v", adj.BinID, adj.CoreNodeName, err)
			return
		}
		if err := e.db.UpdateProcessNodeUOP(node.ID, 0); err != nil {
			log.Printf("uop_adjustment: blank tile count for node %s on release of bin %d: %v", adj.CoreNodeName, adj.BinID, err)
			return
		}
		log.Printf("uop_adjustment: released bin %d from node %s (moved in Core), tile blanked", adj.BinID, adj.CoreNodeName)
		e.Events.Emit(Event{Type: EventUOPAdjusted, Payload: UOPAdjustedEvent{
			ProcessNodeID: node.ID,
			CoreNodeName:  adj.CoreNodeName,
			BinID:         adj.BinID,
			NewRemaining:  0,
			Actor:         adj.Actor,
		}})
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
