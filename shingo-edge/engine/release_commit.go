package engine

// release_commit.go — the release layer's one side-effect site (SHAPE §3.6).
//
// Commit carries out a plan. It cannot refuse on policy: a refused plan's
// refusal is returned before anything is touched, and every effect below runs
// only for a leg the plan let go. What can still fail here is I/O (a flush, a
// write, the outbox), returned in the door's words (release.CommitFailure).
//
// It reads only what an effect it has already been told to perform needs: the
// ledger, the bin a capture names when the order does not, the destination and
// episode a Material-page creation carries. No decision is taken from those
// reads; they are inputs to the effect.
//
// The order of a leg's effects is the trunk's, unchanged: the flip (so no tick
// lands on the departing bin between its count and its clear), the produce
// finalize, then the release itself.

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	"shingoedge/release"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// The release disposition types live in shingoedge/uop; the engine names
// them here for its callers.
type (
	ReleaseDisposition     = uop.ReleaseDisposition
	ReleaseDispositionMode = uop.ReleaseDispositionMode
)

const (
	DispositionCaptureLineside  = uop.DispositionCaptureLineside
	DispositionSendPartialBack  = uop.DispositionSendPartialBack
	DispositionReleaseUnderpack = uop.DispositionReleaseUnderpack
)

// emitLogs writes a decision's log lines to their sinks.
func (e *Engine) emitLogs(logs []release.Log) {
	for _, l := range logs {
		switch l.Sink {
		case release.SinkRelease:
			e.logRelease("%s", l.Text)
		case release.SinkEngine:
			e.logFn("%s", l.Text)
		default:
			log.Print(l.Text)
		}
	}
}

// ── A leg ─────────────────────────────────────────────────────────────────

// commitLeg carries out one leg's plan. released reports whether a release
// envelope was queued.
func (e *Engine) commitLeg(act *releaseAct, p release.LegPlan, ll *legLoad) (released bool, err error) {
	e.emitLogs(p.Logs)
	switch p.Verdict {
	case release.Skip:
		return false, nil
	case release.Refuse:
		return false, p.Refusal
	}
	if err := e.applyLeg(p, ll); err != nil {
		failed := release.CommitFailure(act.Act, ll.snap, err)
		e.emitLogs(failed.Logs)
		return false, failed.Refusal
	}
	return true, nil
}

func (e *Engine) applyLeg(p release.LegPlan, ll *legLoad) error {
	order, disp := ll.order, ll.disp
	if p.Arm == release.ArmPlain {
		// Orders without a process node (pure kanban, generic moves) skip the
		// lineside path: Core gets nil remaining_uop and leaves the bin alone.
		// Intentional, and logged so a "the prompt didn't fire" investigation
		// has something to grep for.
		e.logRelease("order=%d disposition=%q — skipping manifest sync: no_process_node",
			order.ID, string(disp.Mode))
		return e.orderMgr.ReleaseOrder(order.ID, nil, disp.CalledBy)
	}
	if p.Flip {
		if err := e.commitFlip(ll); err != nil {
			return err
		}
	}
	switch p.Arm {
	case release.ArmNoClaim:
		return e.orderMgr.ReleaseOrder(order.ID, nil, disp.CalledBy)
	case release.ArmDrop:
		if p.Finalize {
			if err := e.finalizeDepartingProduce(ll.node, ll.runtime, order, nil); err != nil {
				return err
			}
		}
		return e.releaseOrderDropFastPath(order.ID, ll.node, ll.runtime, disp, p.Finalize)
	}
	// After the flip: on a sequential press the line is already on the
	// partner, so no tick lands on this bin between its count and its clear.
	if p.Finalize {
		if err := e.finalizeDepartingProduce(ll.node, ll.runtime, order, order.SiblingOrderID); err != nil {
			return err
		}
	}
	if p.Arm == release.ArmProduce {
		// Produce nodes don't use lineside buckets and carry no count on the
		// release: the ingest is the bin's manifest.
		e.logRelease("order=%d node=%s disposition=%q — skipping manifest sync: produce_role",
			order.ID, ll.node.Name, string(disp.Mode))
		if err := e.orderMgr.ReleaseOrder(order.ID, nil, disp.CalledBy); err != nil {
			return err
		}
		// U1 AFTER THE RELEASE, NOT BEFORE IT: the side-cycle's premise is "the
		// operator just finished a full bin", so it must not fire for a release
		// that then failed to go out. FROM THE ORDER, NOT THE CLAIM: mid-
		// changeover the claim names the incoming part.
		if p.U1 {
			e.MaybeCreateUnloaderFullIn(order.PayloadCode)
		}
		return nil
	}
	return e.releaseOrderWithFullLineside(order, ll.node, ll.runtime, ll.toClaim, ll.nodeTask, disp, p.SuppressManifest, p.Finalize)
}

// commitFlip moves the line to the partner (L3: the release IS the flip):
// the attribution boundary on the partner (flush the outgoing side's residual
// deltas before the new active side starts ticking), the atomic pull-side
// write, then an immediate level sweep of the position that just went dark —
// the sweep's own decision, taken early for immediacy.
func (e *Engine) commitFlip(ll *legLoad) error {
	node, partner, why := ll.node, ll.partner, ll.snap.Flip.NotReady
	// MarkAttributionBoundary is synchronous — an error means the flush
	// failed, and the pull must not move.
	if e.inventoryDelta != nil {
		if err := e.inventoryDelta.MarkAttributionBoundary(partner.ID); err != nil {
			return fmt.Errorf("attribution boundary flush failed: %w", err)
		}
	}
	if err := e.writePullSide(partner.ID, node.ID); err != nil {
		return err
	}
	if why != "" {
		log.Printf("release flip: node %s released — the line is on %s; its parts wait for %s's bin (%s)",
			node.CoreNodeName, partner.CoreNodeName, partner.CoreNodeName, why)
	} else {
		log.Printf("release flip: node %s released — pull side now on partner %s",
			node.CoreNodeName, partner.CoreNodeName)
	}
	e.sweepNodeLevelNow(node.ID)
	return nil
}

// releaseOrderDropFastPath handles the drop-CO release shape. A drop has no
// to-style claim (the new style abandons this node), so the toClaim-based
// bookkeeping doesn't apply, but the operator's disposition still flows to
// Core so the returning bin's manifest reflects the partial count instead of
// arriving empty (plant incident 2026-05-11, ALN_002).
//
// finalized: a produce bin's count went to Core as its ingest, so the release
// carries none (the slot's count is already cleared and would wipe it).
func (e *Engine) releaseOrderDropFastPath(orderID int64, node *processes.Node, runtime *processes.RuntimeState, disp ReleaseDisposition, finalized bool) error {
	// No CaptureToLineside here (no to-style claim to fill buckets against),
	// so resolvedBinID=0: a PULL PARTS LINESIDE shape falls through to the
	// legacy &0 wipe rather than a delta-driven write.
	manifestUOP := computeReleaseRemainingUOP(disp, runtime, 0)
	if finalized {
		manifestUOP = nil
	}
	wireDisposition := buildProtocolDisposition(disp, runtime)
	e.logRelease("order=%d node=%s disposition=%q — drop release: passing manifest sync through, skipping toClaim-dependent bookkeeping",
		orderID, node.Name, string(disp.Mode))
	return e.orderMgr.ReleaseOrderWithDisposition(orderID, manifestUOP, wireDisposition, disp.CalledBy)
}

// releaseOrderWithFullLineside is the lineside release: the capture of parts
// pulled to lineside (once per order, through the ledger), the old bin's
// finalized count on the runtime row, the changeover task to "released", the
// flush, and the OrderRelease envelope.
//
// isSupply: the supply leg of a two-robot swap; its fresh bin's manifest is
// never synced (the Bug A guard, ALN_002) and it captures nothing.
// finalized: a produce bin finalized at this release (a produce press changing
// over to a consume part); its manifest is Core's ingest, so the release
// carries no count.
func (e *Engine) releaseOrderWithFullLineside(order *storeorders.Order, node *processes.Node, runtime *processes.RuntimeState, toClaim *processes.NodeClaim, nodeTask *processes.NodeTask, disp ReleaseDisposition, isSupply, finalized bool) error {
	orderID := order.ID

	// The bin the capture_reduction emit and the manifest-sync fallback
	// name. order.BinID is usually set by Core's OrderDelivered reply; when
	// it is not, PULL PARTS LINESIDE asks Core which bin is at this slot, and
	// if Core cannot say, resolvedBinID stays 0 and the legacy-&0 fallback
	// fires so the bin doesn't return with its original UOP intact.
	var resolvedBinID int64
	if order.BinID != nil {
		resolvedBinID = *order.BinID
	} else if disp.Mode == uop.DispositionCaptureLineside && len(disp.LinesideCapture) > 0 && e.coreClient.Available() {
		bin, _, err := e.coreClient.BinAtLineside(node.CoreNodeName)
		switch {
		case err != nil:
			e.logRelease("order=%d node=%s — BinAtLineside resolve failed (%v); capture_reduction will skip and manifest-sync fallback will fire",
				orderID, node.Name, err)
		case bin != nil:
			resolvedBinID = bin.BinID
		}
	}

	manifestUOP := computeReleaseRemainingUOP(disp, runtime, resolvedBinID)
	// The protocol disposition rides alongside RemainingUOP for the override
	// audit, built before the supply guard so a supply leg still ships it.
	wireDisposition := buildProtocolDisposition(disp, runtime)

	if manifestUOP != nil && isSupply {
		e.logRelease("order=%d node=%s disposition=%q — skipping manifest sync: supply_bin_guard (two-robot swap)",
			orderID, node.Name, string(disp.Mode))
		manifestUOP = nil
	}
	if finalized {
		manifestUOP = nil
	}

	// The capture: the piles' gain and the bin's capture_reduction, as one
	// verb, against the ORDER's payload (the bin's), not the target claim's.
	if e.inventoryDelta != nil {
		var binEpoch int64
		if runtime != nil && runtime.ActiveBinID != nil && *runtime.ActiveBinID == resolvedBinID {
			binEpoch = runtime.ActiveBinEpoch
		}
		// CAPTURE ONCE PER ORDER. The capture is the one ADDITIVE piece of
		// release paperwork; a repeated release (Core refused it and the
		// operator clicks again, or a second click reaches a leg already
		// moving) applies only what this order has not already captured.
		captureDisp, prior, err := e.captureOnce(order.ID, disp, isSupply)
		if err != nil {
			return err
		}
		e.countMu.Lock()
		_, err = e.inventoryDelta.CaptureToLineside(uop.CaptureEvent{
			NodeID:           node.ID,
			CoreNodeName:     node.CoreNodeName,
			Disposition:      captureDisp,
			BinID:            resolvedBinID,
			PayloadCode:      order.PayloadCode,
			BinEpoch:         binEpoch,
			SuppressBinDelta: isSupply,
		})
		e.countMu.Unlock()
		if err != nil {
			return err
		}
		e.recordCapture(order.ID, disp, captureDisp, prior, resolvedBinID, binEpoch, isSupply)
	}

	// The OLD bin's local count follows what Core was told (RELEASE EMPTY →
	// 0, SEND PARTIAL BACK → the partial). The incoming bin's count arrives on
	// its OrderDelivered envelope; ticks in between are held and replayed.
	if manifestUOP != nil {
		if err := e.db.UpdateProcessNodeUOP(node.ID, *manifestUOP); err != nil {
			e.logRelease("finalize old-bin cache for node %s on release: %v", node.Name, err)
		}
	}
	if nodeTask != nil {
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, domain.NodeTaskReleased); err != nil {
			e.logRelease("update node task %d to released: %v", nodeTask.ID, err)
		}
	}

	// Flush boundary: the accumulated deltas and pile levels reach the outbox
	// before the OrderRelease envelope.
	if e.inventoryDelta != nil {
		e.inventoryDelta.Flush()
	}
	return e.orderMgr.ReleaseOrderWithDisposition(orderID, manifestUOP, wireDisposition, disp.CalledBy)
}

func computeReleaseRemainingUOP(disp ReleaseDisposition, runtime *processes.RuntimeState, resolvedBinID int64) *int {
	return uop.ComputeReleaseRemainingUOP(disp, runtime, resolvedBinID)
}

func buildProtocolDisposition(disp ReleaseDisposition, runtime *processes.RuntimeState) *protocol.UOPDisposition {
	return uop.BuildProtocolDisposition(disp, runtime)
}

// ── The ledger: capture ───────────────────────────────────────────────────

// captureOnce returns the disposition the capture step should apply for this
// order: the operator's per-part quantities less what an earlier attempt at
// the same release already captured, floored at zero. prior is what had been
// captured (nil when nothing had), for recordCapture. Only a release that
// carries captures reads the ledger. Keyed by the order, not the bin: the bin
// a release resolves can differ between attempts.
func (e *Engine) captureOnce(orderID int64, disp ReleaseDisposition, isSupply bool) (ReleaseDisposition, *store.ReleaseCaptureRecord, error) {
	if isSupply || disp.Mode != uop.DispositionCaptureLineside || len(disp.LinesideCapture) == 0 {
		return disp, nil, nil
	}
	prior, err := e.db.GetReleaseCapture(orderID)
	if err != nil {
		return disp, nil, fmt.Errorf("order %d: could not read what this release already captured (%w)", orderID, err)
	}
	if prior == nil {
		return disp, nil, nil
	}
	diff := make(map[string]int, len(disp.LinesideCapture))
	for part, qty := range disp.LinesideCapture {
		if extra := qty - prior.Parts[part]; extra > 0 {
			diff[part] = extra
		}
	}
	out := disp
	out.LinesideCapture = diff
	return out, prior, nil
}

// recordCapture writes what this order has now captured — per part, the
// larger of the earlier record and this attempt — and logs what was applied
// and what was skipped as a repeat. Written after the capture, not in its
// transaction (the capture's writes live in the uop mutator): a crash between
// the two leaves the capture unrecorded, and a retry applies it again.
func (e *Engine) recordCapture(orderID int64, requested, applied ReleaseDisposition, prior *store.ReleaseCaptureRecord,
	binID, epoch int64, isSupply bool) {
	if isSupply || requested.Mode != uop.DispositionCaptureLineside || len(requested.LinesideCapture) == 0 {
		return
	}
	merged := map[string]int{}
	if prior != nil {
		for part, qty := range prior.Parts {
			merged[part] = qty
		}
	}
	for part, qty := range requested.LinesideCapture {
		if qty > merged[part] {
			merged[part] = qty
		}
	}
	if err := e.db.PutReleaseCapture(orderID, store.ReleaseCaptureRecord{BinID: binID, Epoch: epoch, Parts: merged}); err != nil {
		e.logRelease("order=%d bin=%d capture: applied but NOT recorded (%v) — a retry of this release will capture again", orderID, binID, err)
	}
	parts := make([]string, 0, len(requested.LinesideCapture))
	for part := range requested.LinesideCapture {
		parts = append(parts, part)
	}
	sort.Strings(parts)
	var lines []string
	for _, part := range parts {
		asked, got := requested.LinesideCapture[part], applied.LinesideCapture[part]
		switch {
		case prior == nil:
			lines = append(lines, fmt.Sprintf("%s=%d applied", part, asked))
		case got == 0:
			lines = append(lines, fmt.Sprintf("%s=%d skipped as a repeat (already %d)", part, asked, prior.Parts[part]))
		default:
			lines = append(lines, fmt.Sprintf("%s=%d applied %d (already %d)", part, asked, got, prior.Parts[part]))
		}
	}
	e.logRelease("order=%d bin=%d epoch=%d capture: %s", orderID, binID, epoch, strings.Join(lines, ", "))
}

// ── The ledger: the produce finalize ──────────────────────────────────────

// finalizeDepartingProduce is the one finalize of a produce bin, run at the
// operator's RELEASE of the leg that takes the bin away, in every mode and at
// every door. The count splits at that press (owner, 2026-09-30): parts made
// before it belong to the departing bin, parts made after it to the next one.
// It is the operator's declaration, so it stands through a Core refusal.
//
// Three steps, in this order:
//  1. flush the accumulator, so the ticks of the last window reach Core
//     before the ingest bumps the bin's epoch (no stale-epoch drops);
//  2. queue the ingest: the Edge's count, the bin and its epoch, and the
//     departing ORDER's payload (for a changeover evac, the outgoing style's).
//     No manifest lines: Core resolves the payload's template;
//  3. clear the slot (carrierLeft), so later ticks hold and replay onto the
//     next bin.
//
// ONCE PER DEPARTING ORDER, through the ledger (release_paperwork, kind
// ingest). A zero count ships nothing.
func (e *Engine) finalizeDepartingProduce(node *processes.Node, runtime *processes.RuntimeState, departing *storeorders.Order, placingOrderID *int64) error {
	if runtime == nil || runtime.RemainingUOPCached <= 0 {
		e.logFn("produce release: node %s remaining=%d — no release-time manifest to stamp",
			node.Name, runtimeRemaining(runtime))
		return nil
	}
	shipped, err := e.db.ReleaseIngestShipped(departing.ID)
	if err != nil {
		return fmt.Errorf("node %s: %w", node.Name, err)
	}
	if shipped {
		e.logFn("produce release: node %s order %d — its ingest already shipped; not again",
			node.Name, departing.ID)
		return nil
	}
	payload := departing.PayloadCode
	if payload == "" {
		if resident, rerr := e.residentClaim(node, runtime); rerr == nil && resident != nil {
			payload = resident.PayloadCode
		}
	}
	qty := int64(runtime.RemainingUOPCached)
	var binID int64
	if runtime.ActiveBinID != nil {
		binID = *runtime.ActiveBinID
	}
	if e.inventoryDelta != nil {
		e.inventoryDelta.Flush()
	}
	// Quantity is the CYCLE count, not a part count: Core writes it to
	// uop_remaining, and the part count is uop_remaining x the template's
	// parts_per_cycle.
	if err := e.orderMgr.QueueIngestManifest(
		payload, "", binID, runtime.ActiveBinEpoch, node.CoreNodeName, qty, nil,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("queue release-time ingest for node %s: %w", node.Name, err)
	}
	if err := e.db.MarkReleaseIngestShipped(departing.ID, binID, qty); err != nil {
		e.logFn("produce release: node %s order %d — ingest shipped but not recorded (%v); a second door may ship it again",
			node.Name, departing.ID, err)
	}
	// The placed-bin gate: is the bound bin the one LEAVING, or the one the
	// placing leg just ARRIVED with? Discriminated by bin identity; an
	// ambiguous answer (a placing order terminal with no bin on its row)
	// keeps the slot bound rather than risk erasing a bin on the press.
	if placingOrderID != nil {
		if placing, err := e.db.GetOrder(*placingOrderID); err == nil {
			if placing.BinID != nil && runtime.ActiveBinID != nil &&
				*placing.BinID == *runtime.ActiveBinID {
				e.logFn("produce release: node %s active bin %d is the bin order %d PLACED here — not the departing one; keeping the slot bound",
					node.Name, *runtime.ActiveBinID, placing.ID)
				return nil
			}
			if placing.BinID == nil && ordermgr.IsTerminalSuccess(placing.Status) {
				e.logFn("produce release: node %s placing order %d is terminal with no bin on the row — skipping the clear rather than risk erasing a bin standing on the position",
					node.Name, placing.ID)
				return nil
			}
		}
	}
	// The count now belongs to the departing bin: clear the slot, so the
	// hold-and-replay window starts HERE. Log-only on failure: the ingest
	// already shipped.
	if err := e.carrierLeft(node.ID, node.CoreNodeName); err != nil {
		log.Printf("produce release: clear active bin for node %d: %v", node.ID, err)
	}
	return nil
}

// runtimeRemaining is a nil-safe read for log lines.
func runtimeRemaining(runtime *processes.RuntimeState) int {
	if runtime == nil {
		return 0
	}
	return runtime.RemainingUOPCached
}

// ── Door 1's paperwork and deferral ───────────────────────────────────────

// commitPairPaperwork carries out the pair click's door-level plan: a refusal
// is returned with nothing changed; otherwise the departing bin is finalized
// FIRST, before either release envelope, whether or not every leg goes on this
// click — the operator's RELEASE is the declaration that the bin is full. The
// supply rides along so the finalize can tell the placed bin from the departing
// one.
func (e *Engine) commitPairPaperwork(p release.PairPlan, pl *pairLoad) error {
	e.emitLogs(p.Logs)
	if p.Verdict == release.Refuse {
		return p.Refusal
	}
	if p.Finalize {
		return e.finalizeDepartingProduce(pl.node, pl.runtime, pl.evac, p.Supply)
	}
	return nil
}

// commitDeferral remembers a pair leg the click could not release yet, so it
// is released when it reaches its wait (door 8). In memory: an Edge restart
// loses it, and the survivor rule (door 9) asks the durable question.
func (e *Engine) commitDeferral(remember bool, logs []release.Log, legID int64, disp ReleaseDisposition) {
	if !remember {
		return
	}
	e.pendingSiblingReleaseMu.Lock()
	if e.pendingSiblingRelease == nil {
		e.pendingSiblingRelease = make(map[int64]ReleaseDisposition)
	}
	e.pendingSiblingRelease[legID] = disp
	e.pendingSiblingReleaseMu.Unlock()
	e.emitLogs(logs)
}

// takeDeferral consumes a leg's deferral, if it has one.
func (e *Engine) takeDeferral(orderID int64) (ReleaseDisposition, bool) {
	e.pendingSiblingReleaseMu.Lock()
	defer e.pendingSiblingReleaseMu.Unlock()
	disp, ok := e.pendingSiblingRelease[orderID]
	if ok {
		delete(e.pendingSiblingRelease, orderID)
	}
	return disp, ok
}

// forgetLeg drops a terminal leg's deferral and survivor mark, so neither map
// grows without bound.
func (e *Engine) forgetLeg(orderID int64) {
	e.pendingSiblingReleaseMu.Lock()
	delete(e.pendingSiblingRelease, orderID)
	e.pendingSiblingReleaseMu.Unlock()
	e.survivorReleasedMu.Lock()
	delete(e.survivorReleased, orderID)
	e.survivorReleasedMu.Unlock()
}

// markSurvivorFired spends the survivor rule's one release for this order.
// Spent on a release, not on a try: an attempt that never reached Core cannot
// have flapped anything.
func (e *Engine) markSurvivorFired(orderID int64) {
	e.survivorReleasedMu.Lock()
	if e.survivorReleased == nil {
		e.survivorReleased = make(map[int64]struct{})
	}
	e.survivorReleased[orderID] = struct{}{}
	e.survivorReleasedMu.Unlock()
}

// noteReleaseHeld puts an automatic release's curtain refusal on the order,
// for the board's chip. Only the curtain's refusals: every other reason an
// automatic release declines is the deferral working, and has its own account.
func (e *Engine) noteReleaseHeld(orderID int64, err error) {
	var held *CurtainHeldError
	if !errors.As(err, &held) {
		return
	}
	if nerr := e.orderMgr.NoteReleaseHeld(orderID, held.Sentence); nerr != nil {
		e.logFn("release held for order %d (%s) but the note could not be written: %v", orderID, held.Sentence, nerr)
	}
}

// ── Doors 3 and 4: the creation ───────────────────────────────────────────

// commitMaterial creates the move that carries the node's bin to its outbound
// destination, for a plan that let it go.
func (e *Engine) commitMaterial(p release.MaterialPlan, ml *materialLoad, qty int64, overrideRemainingUOP *int) (*storeorders.Order, error) {
	if p.Verdict == release.Refuse {
		return nil, p.Refusal
	}
	nodeID, node, runtime := ml.nodeID, ml.node, ml.runtime
	// THE FOURTH DOOR's destination: whatever carrier stands on the cell, read
	// off the RESIDENT claim (the defect 1b17b0f9 fixed at the swap builders).
	// Applied after the gates, deliberately: an unconfigured requested claim
	// still refuses. The override redirects a release; it does not authorise one.
	claim := withResidentEvacDest(ml.claim, e.residentEvacDest(runtime, ml.claim))
	// The operator's count (the Material page's prompt) supersedes the cache.
	var remainingUOP *int
	if overrideRemainingUOP != nil {
		v := *overrideRemainingUOP
		remainingUOP = &v
	} else if runtime.RemainingUOPCached >= 0 {
		v := runtime.RemainingUOPCached
		remainingUOP = &v
	}
	// The release is the return leg of the circle the inbound delivery opened
	// (§R.87): it joins that cell's episode, never mints one. The carrier's
	// OWN payload, when Core has told us what it is.
	order, err := e.orderMgr.CreateMoveOrderWithUOP(&nodeID, qty, claim.CoreNodeName, claim.OutboundDestination,
		string(runtime.LinesidePayloadCode), remainingUOP, claim.AutoConfirm || e.cfg.Web.AutoConfirm,
		e.cellEpisodeOrigin(node, claim))
	if err != nil {
		return nil, err
	}
	if err := e.db.SetProcessNodeRuntimeActiveOrder(nodeID, &order.ID); err != nil {
		e.logFn("station: update runtime orders for node %d: %v", nodeID, err)
	}
	refreshed, err := e.db.GetOrder(order.ID)
	if err != nil {
		e.logFn("station: re-read order %d after runtime update: %v", order.ID, err)
		return order, nil
	}
	return refreshed, nil
}
