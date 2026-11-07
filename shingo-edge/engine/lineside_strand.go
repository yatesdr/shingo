package engine

import (
	"database/sql"
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
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
// through the one flip verb below
// (flipActiveStyle). It writes the style and
// syncs the reporting point, each its own statement; then, in one transaction,
// strands the piles, clears the departing part's produce holds, points every
// node the incoming style claims at that claim and records the carrier it
// leaves at every seat that could not say; then publishes
// plant.claims. A node the changeover does not touch is stranded too.

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
// In the same transaction it points each node the incoming style claims at
// that claim (pointNodesAtStyle), and records what the flip leaves
// (flipCarrierStamps): a consume seat of the departing style's whose bound
// carrier could not say its part is recorded as that style's part there.
//
// proc is the process as read before the flip, so its active style is the
// departing one; styleID is the incoming style.
func (e *Engine) strandLinesidePiles(proc *processes.Process, styleID *int64) error {
	// The write and the mark are one change to the seat for the lineside
	// report (countMu); PilesChanged takes the flush lock inside it.
	e.countMu.Lock()
	defer e.countMu.Unlock()
	processID := proc.ID
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return fmt.Errorf("cutover nodes for process %d: %w", processID, err)
	}
	runtimes, err := e.db.ProcessNodeRuntimes(nodeIDsOf(nodes))
	if err != nil {
		return fmt.Errorf("cutover seats for process %d: %w", processID, err)
	}
	clears, err := e.cutoverHoldClears(proc, styleID, nodes, runtimes)
	if err != nil {
		return fmt.Errorf("cutover holds for process %d: %w", processID, err)
	}
	pointers, err := e.pointNodesAtStyle(styleID, nodes, runtimes)
	if err != nil {
		return fmt.Errorf("cutover claim pointers for process %d: %w", processID, err)
	}
	stamps, err := e.flipCarrierStamps(proc, nodes, runtimes)
	if err != nil {
		return fmt.Errorf("cutover carriers for process %d: %w", processID, err)
	}
	stranded, err := e.db.StrandLinesidePiles(processID, func(tx *sql.Tx) error {
		if err := clears.writeIn(tx); err != nil {
			return err
		}
		if err := e.writeFlipStamps(tx, stamps); err != nil {
			return err
		}
		return pointers.writeIn(tx)
	})
	if err != nil {
		return fmt.Errorf("strand lineside piles for process %d: %w", processID, err)
	}
	clears.queued()
	e.logFlipStamps(proc, stamps)
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

// flipActiveStyle is THE active-style flip: db.SetActiveStyle, the reporting
// point's sync, and, when the style actually changed, what every flip owes —
// in one transaction, strand the process's lineside piles, clear its departing
// produce holds, point its nodes at the incoming style's claims and record the
// carrier it leaves at every seat that could not say (strandLinesidePiles);
// then publish the process's plant.claims report so
// Core's mirror marks the new running style within one message instead of up
// to the next snapshot. Re-setting the style a process already runs is not a
// flip: no strand, no pointers, no stamp, no publish.
//
// Both doors route here — the admin switch (admin_style_switch.go) and the
// changeover's completeCutover — so a flip cannot reach the DB without its
// consequences.
//
// THE SYNC CREATES THE POINT. It sits after the style write and before the
// same-style return, so the first active style of a new process gets its
// reporting point (TestFlip_AdminSwitchCreatesThePointInTheEngine), and a retry
// after a partial failure still syncs. Its error is returned: a point left on
// the old style charges the new style's ticks to nobody (style_mismatch).
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
	if err := e.syncPrimaryPoint(processID, proc.CounterPLCName, proc.CounterTagName); err != nil {
		return fmt.Errorf("sync reporting point for process %d: %w", processID, err)
	}
	if sameStyle(proc.ActiveStyleID, styleID) {
		return nil
	}
	if err := e.strandLinesidePiles(proc, styleID); err != nil {
		log.Printf("lineside: style flip on process %d: %v", processID, err)
	}
	e.publishPlantClaims(processID)
	e.catidHealthChanged(processID)
	return nil
}

// claimPointers is the pointer verb's writes: each node's runtime pointed at
// the claim the style has for it.
type claimPointers []claimPointer

type claimPointer struct{ nodeID, claimID int64 }

// pointNodesAtStyle reads what the pointer verb writes: every node the style
// claims BY NAME whose runtime points elsewhere. A node the style does not
// claim keeps its pointer — never NULL: a NULL pointer drops the operator's
// RELEASE PARTIAL/EMPTY with no count (resolveReleaseClaim), and a fanned-out
// position carries its parent's claim under no name of its own. Nodes whose
// task already advanced the pointer write nothing. One statement (the
// style's claims); the nodes and runtimes are the caller's, read before the
// transaction the writes ride (the store is one connection).
func (e *Engine) pointNodesAtStyle(styleID *int64, nodes []processes.Node,
	runtimes map[int64]*processes.RuntimeState) (claimPointers, error) {
	if styleID == nil || len(nodes) == 0 {
		return nil, nil
	}
	claims, err := e.db.NodeClaimsForStyles([]int64{*styleID})
	if err != nil {
		return nil, err
	}
	only := &processes.Process{ActiveStyleID: styleID}
	var out claimPointers
	for i := range nodes {
		rt := runtimes[nodes[i].ID]
		claim := claims.Resolve(only, &nodes[i], store.ActiveStyleFirst)
		if rt == nil || claim == nil || (rt.ActiveClaimID != nil && *rt.ActiveClaimID == claim.ID) {
			continue
		}
		out = append(out, claimPointer{nodeID: nodes[i].ID, claimID: claim.ID})
	}
	return out, nil
}

// writeIn points each node at its claim: the claim only, never the count
// (TestFlip_LeavesTheCount).
func (p claimPointers) writeIn(ex processes.DBTX) error {
	for _, w := range p {
		claimID := w.claimID
		if err := processes.SetRuntimeClaim(ex, w.nodeID, &claimID); err != nil {
			return err
		}
	}
	return nil
}

// flipStamps is what a flip records it leaves: each seat's carrier, by the
// departing style's part there.
type flipStamps []flipStamp

type flipStamp struct {
	nodeID int64
	node   string
	part   string
}

// flipCarrierStamps reads what the flip records it leaves: every node whose
// bound carrier could not say its part (!LinesidePayloadKnown) and whose
// departing-style claim (proc's active style, exactly) is consume, not a
// loader, and names a part. The cart standing there was bound under that
// style, so it is that style's part there; after the flip nothing else can
// say so, and the readers that fall back to the active style's part
// (binAtNode, carrierIsPart) would name the incoming one. A seat still unknown
// after a flip was bound since it, and the fallback is right for it by
// construction. The claims are read only when such a seat exists: +0
// statements on a flip that leaves none.
func (e *Engine) flipCarrierStamps(proc *processes.Process, nodes []processes.Node,
	runtimes map[int64]*processes.RuntimeState) (flipStamps, error) {
	if proc == nil || proc.ActiveStyleID == nil {
		return nil, nil
	}
	var unsaid []*processes.Node
	for i := range nodes {
		if rt := runtimes[nodes[i].ID]; rt != nil && rt.ActiveBinID != nil && !rt.LinesidePayloadKnown {
			unsaid = append(unsaid, &nodes[i])
		}
	}
	if len(unsaid) == 0 {
		return nil, nil
	}
	departing := *proc.ActiveStyleID
	claims, err := e.db.NodeClaimsForStyles([]int64{departing})
	if err != nil {
		return nil, err
	}
	var out flipStamps
	for _, node := range unsaid {
		claim := claims.Resolve(proc, node, store.ExactStyle(departing))
		if claim == nil || claim.Role != protocol.ClaimRoleConsume || claim.IsLoaderNode() || claim.PayloadCode == "" {
			continue
		}
		out = append(out, flipStamp{nodeID: node.ID, node: node.CoreNodeName, part: claim.PayloadCode})
	}
	return out, nil
}

// writeFlipStamps records each seat's carrier through the lineside doorway, on
// the strand's transaction.
func (e *Engine) writeFlipStamps(tx *sql.Tx, stamps flipStamps) error {
	for _, s := range stamps {
		carrier := domain.KnownCarrier(domain.LinesidePayloadCode(s.part))
		if err := e.recordLinesideCarrierIn(tx, s.nodeID, carrier, domain.CarrierFromFlip); err != nil {
			return err
		}
	}
	return nil
}

// logFlipStamps names each recorded seat once, after the strand's commit: the
// process, the node, the part and the style the flip left.
func (e *Engine) logFlipStamps(proc *processes.Process, stamps flipStamps) {
	if len(stamps) == 0 {
		return
	}
	departing := fmt.Sprintf("style %d", *proc.ActiveStyleID)
	if s, err := e.db.GetStyle(*proc.ActiveStyleID); err == nil && s != nil && s.Name != "" {
		departing = s.Name
	}
	for _, s := range stamps {
		log.Printf("lineside carrier: process %s node %s: recorded as %s at the switch off %s, the style its cart "+
			"was bound under; the cart could not say its part (source %s)", proc.Name, s.node, s.part, departing,
			domain.CarrierFromFlip)
	}
}

// pointProcessAtStyle is the pointer verb on its own, for the cancel: the
// style does not change there, so no strand carries it.
func (e *Engine) pointProcessAtStyle(processID int64, styleID *int64) error {
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return err
	}
	runtimes, err := e.db.ProcessNodeRuntimes(nodeIDsOf(nodes))
	if err != nil {
		return err
	}
	pointers, err := e.pointNodesAtStyle(styleID, nodes, runtimes)
	if err != nil {
		return err
	}
	return pointers.writeIn(e.db.DB)
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
