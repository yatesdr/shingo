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

	if rt.ActiveBinID == nil && !adj.Released && protocol.IsLifecycleActor(adj.Actor) {
		// AN EMPTY SLOT IS NOT AN INVITATION. Core generated this from a carrier's
		// own lifecycle — load, clear, release, or a press finishing one — and the
		// bind below is not for that. It is a repair built for a person
		// deliberately correcting a count on a carrier the Edge never bound.
		//
		// Produce finalize is the frequent case and the dangerous one. It fires
		// once per press cycle, and its announcement is enqueued when the press
		// finishes and delivered after the outbox and Kafka have carried it — by
		// which time a robot has very often already lifted the carrier it names.
		// Binding here attaches a carrier that is driving away, and every part the
		// press makes afterwards is charged to it until the next one lands.
		//
		// THE PREVIOUS GUARD WAS A PROXY AND IT HAD A HOLE. It refused only when
		// ticks were being held, on the reasoning that a held pile proves a swap
		// gap. It does — but the pile is empty for the whole interval between the
		// robot lifting the carrier and the press completing its next part, and
		// that interval is exactly where these announcements land. The held-ticks
		// check survives below because it still answers a different question.
		//
		// Waiting costs nothing. The arriving carrier brings its own identity,
		// count and generation with it on delivery.
		log.Printf("uop_adjustment: bin %d at node %s is a Core lifecycle announcement (actor=%q) "+
			"and the slot is empty — not binding a carrier that has left",
			adj.BinID, adj.CoreNodeName, adj.Actor)
		return
	}

	if rt.ActiveBinID == nil && !adj.Released && rt.PendingUOPDelta != 0 {
		// A PERSON's correction, arriving while the slot is mid-swap and holding
		// ticks for the carrier that has not arrived yet. Still do not bind.
		//
		// The machine case is already gone above, so what reaches here is somebody
		// declaring a number for a carrier the Edge is not holding, at a moment
		// when the next tick will replay the held pile onto whatever is bound.
		// Those parts belong to the carrier still on its way, not to the one being
		// corrected.
		//
		// A held pile is proof the slot is mid-swap. Waiting costs nothing — the
		// arriving carrier brings its own count and generation on delivery.
		log.Printf("uop_adjustment: bin %d at node %s arrived while %d ticks are held for the "+
			"next carrier — not binding over them",
			adj.BinID, adj.CoreNodeName, rt.PendingUOPDelta)
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

	// TAKE THE EPOCH WITH THE COUNT. Core sends both in this message and the Edge
	// used to keep only the count, which is incoherent: it accepted "this bin now
	// holds N" and ignored "...and it has started a new life".
	//
	// That was the whole of the count-loss at Hopkinsville. Clearing a bin for
	// reuse on Core's admin screen lands here — neither Bound nor Released, a new
	// count and a new epoch — 153 times in 30 days. The Edge kept its old epoch,
	// so every delta it sent afterwards carried a stale stamp and Core discarded
	// it: 19,245 counts dropped against 19,962 applied, and on 2026-07-30, 3,200
	// dropped with none landing.
	//
	// Safe precisely because it rides the count. The guard above has already
	// established that THIS bin is the one bound here, and the same message
	// resets the count — so there is no question of an old life's ticks being
	// misapplied to a new one. Adopting one half and not the other was the bug.
	//
	// The stamp itself cannot go backwards: the store writes active_bin_epoch
	// under a monotonicity rule, so a message that arrives out of order lands
	// its count and leaves the newer stamp alone.
	//
	// An older Core that does not send an epoch sends zero, and zero must not
	// blank the stamp. That used to be a branch here; it is now just an instance
	// of the store's rule that the stamp only ever moves forward for a given bin
	// (see epochAssign in store/processes). Zero is simply not greater. The count
	// lands either way — it is written unconditionally, the guard is on the epoch
	// column alone.
	if err := e.db.SetProcessNodeRuntimeWithBinAndEpoch(node.ID, rt.ActiveClaimID, rt.ActiveBinID, adj.Epoch, adj.NewRemaining); err != nil {
		log.Printf("uop_adjustment: write remaining_uop=%d epoch=%d for node %s: %v", adj.NewRemaining, adj.Epoch, adj.CoreNodeName, err)
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
