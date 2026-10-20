package engine

import (
	"fmt"
	"log"

	"shingoedge/store/lineside"
	"shingoedge/store/processes"
)

// lineside_strand.go — a cutover strands the process's lineside piles.
//
// THE RULE (owner, 2026-09-25): "Cutover on pulled parts strands them as an
// anomaly. From there they're not counted. When that style is changed back
// into, they don't count on the Edge either, and ticks deduct from the new
// bin."
//
// Operators run out what they pull, so a pile with parts left at a cutover is
// most likely the size of a declaration error, not parts on the bench. It is
// kept as a stranded record, labelled a count anomaly at cutover, and never
// drains, counts or revives. The next pull of that part makes a new active
// pile.
//
// "Cutover" is every active-style flip on the process: the changeover's
// (completeCutover) and the admin's (SetProcessActiveStyle). Both route
// through the one flip verb below (flipActiveStyle), which writes the style,
// strands the piles and clears the departing part's produce holds in one
// transaction, and publishes plant.claims.
// A node the changeover does not touch is stranded too.

// strandLinesidePiles folds every active pile at the process's nodes into its
// (node, payload) stranded row, logs each as a count anomaly, and sends both
// levels (active 0, stranded N) for each.
//
// In the same transaction it clears the departing part's produce holds
// (cutoverHoldClears): every hold on a produce seat of the process whose part
// is not the part of the node's incoming produce claim, a hold recorded with no
// part ("") included, with one hold_dropped (cutover) row per seat. A hold of
// the incoming claim's own part is kept: a same-part changeover's strokes are
// the next bin's. A consume seat's hold is not touched; it waits for the next
// carrier of its part (pending_uop_payload, Lane H R2, DECISIONS D12).
//
// proc is the process as read before the flip, so its active style is the
// departing one; styleID is the incoming style.
func (e *Engine) strandLinesidePiles(proc *processes.Process, styleID *int64) error {
	// The write and the mark are one change to the seat for the lineside
	// report (countMu); PilesChanged takes the flush lock inside it.
	e.countMu.Lock()
	defer e.countMu.Unlock()
	processID := proc.ID
	clears, err := e.cutoverHoldClears(proc, styleID)
	if err != nil {
		return fmt.Errorf("cutover holds for process %d: %w", processID, err)
	}
	stranded, err := e.db.StrandLinesidePiles(processID, clears.writeIn)
	if err != nil {
		return fmt.Errorf("strand lineside piles for process %d: %w", processID, err)
	}
	clears.queued()
	keys := make([]lineside.Key, 0, 2*len(stranded))
	for _, s := range stranded {
		log.Printf("lineside: count anomaly at cutover: node=%d core_node=%s payload=%s qty=%d stranded (process %d)",
			s.NodeID, s.CoreNodeName, s.PayloadCode, s.Qty, processID)
		keys = append(keys,
			lineside.Key{NodeID: s.NodeID, CoreNodeName: s.CoreNodeName, PayloadCode: s.PayloadCode, State: lineside.StateActive},
			lineside.Key{NodeID: s.NodeID, CoreNodeName: s.CoreNodeName, PayloadCode: s.PayloadCode, State: lineside.StateStranded})
	}
	if len(keys) > 0 && e.inventoryDelta != nil {
		e.inventoryDelta.PilesChanged(keys...)
	}
	return nil
}

// SetProcessActiveStyle is the admin style flip. It delegates to
// flipActiveStyle — the one flip verb — so an admin flip strands the piles
// and reaches Core exactly like a changeover's cutover does.
func (e *Engine) SetProcessActiveStyle(processID int64, styleID *int64) error {
	return e.flipActiveStyle(processID, styleID)
}

// flipActiveStyle is THE active-style flip: db.SetActiveStyle plus, when the
// style actually changed, the two side effects every flip owes — strand the
// process's lineside piles and clear its departing produce holds (below, one
// transaction) and publish the process's plant.claims
// report so Core's mirror marks the new running style within one message
// instead of up to the next snapshot. Re-setting the style a process already
// runs is not a flip: no strand, no publish.
//
// Both doors route here — the admin verb above and the changeover's
// completeCutover — so a flip cannot reach the DB without its consequences.
//
// The strand and the publish failures are logged, not returned: the style is
// already set, and the flip is what the caller asked for. The piles stay
// active and the holds stand until the next flip (an unbound seat's hold with
// no carrier coming is still dropped by the tick or the sweep); the snapshot
// is the publish's safety net.
func (e *Engine) flipActiveStyle(processID int64, styleID *int64) error {
	proc, err := e.db.GetProcess(processID)
	if err != nil {
		return fmt.Errorf("get process %d: %w", processID, err)
	}
	if err := e.db.SetActiveStyle(processID, styleID); err != nil {
		return err
	}
	if sameStyle(proc.ActiveStyleID, styleID) {
		return nil
	}
	if err := e.strandLinesidePiles(proc, styleID); err != nil {
		log.Printf("lineside: style flip on process %d: %v", processID, err)
	}
	e.publishPlantClaims(processID)
	return nil
}

// publishPlantClaims rings the injected plant-claims publish (engine.go,
// wired at the composition root). The notifier's own errors are logged
// inside the closure — a failed publish must not surface as a flip failure.
func (e *Engine) publishPlantClaims(processID int64) {
	if e.plantClaimsFn != nil {
		e.plantClaimsFn(processID)
	}
}

func sameStyle(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
