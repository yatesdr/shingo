// operator_release.go — unified release path for staged orders.
//
// ReleaseOrderWithLineside performs the operator's "I'm done, push it"
// click atomically with capture of any parts the operator pulled to
// lineside during the swap, the UOP reset, the changeover-task state
// transition, and the bin manifest sync at Core (via the OrderRelease
// envelope's RemainingUOP field for RELEASE EMPTY / SEND PARTIAL BACK,
// or via the BinUOPDelta(capture_reduction) stream for PULL PARTS
// LINESIDE).
//
// Disposition modes
// -----------------
//
//   - DispositionCaptureLineside: operator declares the line emptied the bin.
//     Captures any pulled-to-lineside parts as buckets, and tells Core to
//     clear the bin's manifest (remaining_uop = 0). Default for the existing
//     "NOTHING PULLED" / per-part flows.
//   - DispositionSendPartialBack: operator wants the partially-consumed bin
//     returned to the supermarket as-is. No bucket capture, and Core syncs
//     the bin's UOP to the runtime's RemainingUOP at click time (manifest
//     preserved). Used by the "SEND PARTIAL BACK" button.
//   - "" (empty / zero-value disposition): no manifest action — Core gets nil
//     for remaining_uop and leaves the bin alone. Used by Order A in two-
//     robot swaps (the supply order has no outgoing line bin) and by every
//     fallback path that currently calls orderMgr.ReleaseOrder directly.
//     Preserves pre-disposition behavior so older clients/paths don't
//     accidentally start clearing manifests.
//
// Before this handler existed, the UOP reset fired when Order B (or A,
// depending on swap mode) *completed* — i.e. when the bots dropped the
// empty bin back at the AMR supermarket. That meant the node counter
// lied about station state during the entire "bots home" leg, and any
// parts the operator had already run off at lineside weren't tracked
// anywhere. Reset-on-release closes both gaps.
//
// The completion handlers in wiring.go still perform the UOP reset +
// "released" state transition as a safety net for paths that never hit
// this release handler (e.g. changeover restore, future AutoConfirm
// edge cases). Those handlers check nodeTask.State first — if the
// release handler already ran, they no-op.
package engine

import (
	"errors"
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// ReleaseDispositionMode controls how a release action interacts with the
// bin's manifest at Core. See operator_release.go for a full discussion.
type ReleaseDispositionMode string

const (
	// DispositionCaptureLineside is the default operator-confirmed-empty
	// disposition: any pulled parts are captured as lineside buckets and
	// Core clears the bin's manifest (remaining_uop=0).
	DispositionCaptureLineside ReleaseDispositionMode = "capture_lineside"
	// DispositionSendPartialBack returns the partially-consumed bin to the
	// supermarket with its current UOP count. No bucket capture; Core syncs
	// uop_remaining and preserves the manifest.
	DispositionSendPartialBack ReleaseDispositionMode = "send_partial_back"
	// DispositionReleaseUnderpack — operator declares the bin is
	// physically empty before the tracked count reaches zero (e.g.
	// bin labeled 1200 actually held 1190; cell starves at
	// runtime=10). Wire shape mirrors RELEASE EMPTY (RemainingUOP =
	// &0, manifest cleared); the disposition tag carries the
	// "physical inventory was less than tracked" signal forward so
	// Core's audit row records released_underpack instead of
	// released_empty. Forensics trend the missing-inventory delta
	// from suggested_uop - after_uop in bin_uop_audit.
	DispositionReleaseUnderpack ReleaseDispositionMode = "release_underpack"
)

// ReleaseDisposition carries the operator's release-time intent from the HTTP
// handler down through ReleaseOrderWithLineside to the order manager. The
// zero value (all fields zero/nil) is the backward-compat default — no
// manifest change at Core.
//
// Phase 0b adds the operator-override audit fields. The HTTP handler
// captures whichever values the system would have suggested at modal-open
// time and threads them through here so Core can record divergences:
//
//   - LinesideCaptureSuggested: per-part baseline for the capture path
//     (chip pre-population came from runtime.RemainingUOPCached / manifest qtys).
//   - PartialCount, PartialCountSuggested: operator-entered count and the
//     pre-populated baseline for the SEND PARTIAL BACK path. PartialCount
//     supersedes runtime.RemainingUOPCached for the wire when set.
//
// Suggested fields are nil-safe — legacy HTTP clients that don't ship
// the override-aware body just don't populate them, and Core writes no
// override audit row.
type ReleaseDisposition struct {
	Mode                     ReleaseDispositionMode
	LinesideCapture          map[string]int // qty per part — only valid when Mode == DispositionCaptureLineside
	LinesideCaptureSuggested map[string]int // system-suggested per-part qty at modal-open (Phase 0b)
	PartialCount             *int           // operator-entered count for SEND PARTIAL BACK (Phase 0b); supersedes runtime when set
	PartialCountSuggested    *int           // system-suggested count at modal-open for SEND PARTIAL BACK (Phase 0b)
	CalledBy                 string         // operator identity for audit
}

// ReleaseOrderWithLineside performs the operator's release click:
//
//  1. Resolves the order's process node + active claim. Orders without a
//     process node (pure kanban, generic moves) skip the lineside path
//     entirely and fall through to a plain release.
//  2. Computes the remaining_uop value for Core from the disposition:
//     - DispositionCaptureLineside → &0 (mark bin empty)
//     - DispositionSendPartialBack → &runtime.RemainingUOPCached (partial; falls
//       back to &0 if runtime UOP is non-positive, e.g. sentinel)
//     - "" (empty Mode) → nil (no manifest change)
//  3. Captures any pulled-to-lineside parts (capture_lineside only) and
//     deactivates buckets for other styles on this node (always — release
//     click is the "this style is now active here" point, regardless of
//     disposition).
//  4. Resets RemainingUOP to the target claim's UOPCapacity for the next bin
//     and points runtime.ActiveClaimID at the target claim.
//  5. Advances the changeover node task state to "released" if applicable.
//  6. Calls orderMgr.ReleaseOrder with the computed remaining_uop, which
//     embeds it in the OrderRelease envelope. Core's HandleOrderRelease
//     then routes through BinManifestService.SyncOrClearForReleased before
//     dispatching the fleet.
//
// For orders that don't match a process node (produce-only, generic kanban,
// etc.) this falls back to a plain orderMgr.ReleaseOrder with nil
// remaining_uop — apiReleaseOrder can keep calling this method for every
// release without special-casing, and Core's manifest stays untouched on
// those legacy paths.
func (e *Engine) ReleaseOrderWithLineside(orderID int64, disp ReleaseDisposition) error {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("get order %d: %w", orderID, err)
	}

	// Orders without a process node (pure kanban, generic moves) skip
	// the lineside path entirely. No disposition mapping — Core gets
	// nil remaining_uop and leaves the bin alone (legacy behavior).
	//
	// This is an intentional skip, not a bug — but make it observable.
	// A "the prompt didn't fire" investigation that lands here without a
	// log line has nothing to grep for. See the cleanup-2026-04-27
	// synthesis for the four sites this pattern was added at.
	if order.ProcessNodeID == nil {
		e.logFn("release: order=%d disposition=%q — skipping manifest sync: no_process_node",
			orderID, string(disp.Mode))
		return e.orderMgr.ReleaseOrder(orderID, nil, disp.CalledBy)
	}

	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil {
		return fmt.Errorf("get process node %d: %w", *order.ProcessNodeID, err)
	}
	runtime, err := e.db.EnsureProcessNodeRuntime(node.ID)
	if err != nil {
		return fmt.Errorf("ensure runtime for node %d: %w", node.ID, err)
	}

	// Resolve the target claim: if a changeover is active, use the
	// to-style claim; otherwise use the currently-active claim.
	toClaim, nodeTask := e.resolveReleaseClaim(node, runtime)
	if toClaim == nil {
		// No claim to reset against — still want the release to go out.
		// Skip the disposition mapping; this covers config drift / ingest-only
		// nodes where Core has no opinion on the bin manifest anyway.
		//
		// Diagnostic: this nil-return silently drops the operator's disposition.
		// Surface it so a recurrence is visible in logs instead of producing the
		// invisible "remaining_uop=<nil>" symptom downstream. Was a candidate
		// failure mode for the ALN_002 incident class; see bug-fix-review-plan.md
		// item 1.3.
		// Print "<nil>" rather than 0 when the runtime slot is unset — at 2am
		// in the Debug Log UI, "ActiveClaimID=0" is ambiguous (could be a
		// real ID, could be unset); "ActiveClaimID=<nil>" is unmistakable.
		activeClaimStr := "<nil>"
		if runtime != nil && runtime.ActiveClaimID != nil {
			activeClaimStr = fmt.Sprintf("%d", *runtime.ActiveClaimID)
		}
		e.logFn("release: order %d on node %s — toClaim is nil (runtime.ActiveClaimID=%s), skipping manifest sync; disposition %q dropped",
			orderID, node.Name, activeClaimStr, string(disp.Mode))
		return e.orderMgr.ReleaseOrder(orderID, nil, disp.CalledBy)
	}

	// Two-robot supply-order detection. The per-order release path
	// (apiReleaseOrder, /api/orders/{id}/release) doesn't know whether the
	// order being released is the supply (Order A) or the evac (Order B),
	// so it forwards the operator's chosen disposition either way. We have
	// to discriminate server-side based on which order is which in the
	// runtime's order slots.
	isSupply := e.isSupplyOrderInActiveTwoRobotSwap(node.ID, orderID)

	// Side-cycle trigger (L1 / U1): fires only when the operator declares the
	// bin emptied/full (capture_lineside) and only on the line side of a swap
	// (the supply order in a two-robot swap is suppressed by isSupply). A
	// SEND PARTIAL BACK release explicitly returns the bin to the
	// supermarket — no new empty needs to land at the loader, no new full
	// needs to land at the unloader. Firing on REQUEST instead would
	// over-supply both side-cycle queues whenever the line returns partials.
	//
	//   consume role (line consuming parts) → loader needs an empty bin (L1)
	//   produce role (line producing parts) → unloader needs the full bin (U1)
	//
	// Sits above the produce-role early return so produce-side releases still
	// fan out to the unloader even though they skip Core manifest sync.
	if !isSupply && disp.Mode == DispositionCaptureLineside {
		switch toClaim.Role {
		case protocol.ClaimRoleConsume:
			e.MaybeCreateLoaderEmptyIn(toClaim.PayloadCode)
		case protocol.ClaimRoleProduce:
			e.MaybeCreateUnloaderFullIn(toClaim.PayloadCode)
		}
	}

	// Produce nodes don't use lineside buckets — skip capture, skip UOP
	// reset (produce resets on ingest completion, not release). Pass
	// nil remaining_uop so Core leaves the produce bin's manifest alone.
	//
	// Intentional skip; logged for investigation breadcrumbs (see the
	// no_process_node site above for the rationale).
	if toClaim.Role == protocol.ClaimRoleProduce {
		e.logFn("release: order=%d node=%s disposition=%q — skipping manifest sync: produce_role",
			orderID, node.Name, string(disp.Mode))
		return e.orderMgr.ReleaseOrder(orderID, nil, disp.CalledBy)
	}

	// Compute the manifest-sync UOP from the disposition. Renamed from
	// `remainingUOP` to disambiguate from Manager.ReleaseOrder's parameter of
	// the same name; both flow to the same envelope field but live in
	// different scopes.
	manifestUOP := computeReleaseRemainingUOP(disp, runtime)

	// Phase 0b — protocol Disposition rides alongside the legacy
	// RemainingUOP pointer. Core uses RemainingUOP for the manifest sync
	// (unchanged behavior); Disposition's CountSuggested /
	// CapturesSuggested feed the override audit. Built before the
	// supply-bin guard so isSupply paths still ship the disposition for
	// audit completeness even when the manifest sync is suppressed.
	wireDisposition := buildProtocolDisposition(disp, runtime)

	// Two-robot supply-bin protection (Bug A guard, ALN_002 plant test
	// 2026-04-23): if this is the supply order, skip the manifest sync.
	// Order A's bin is the freshly-loaded supply bin from the supermarket,
	// and clearing its manifest right before delivery destroys the payload
	// data the line is about to need. The consolidated ReleaseStagedOrders
	// path already does this by passing ReleaseDisposition{} for Order A;
	// this guard is the safety net when the per-order path runs instead.
	if manifestUOP != nil && isSupply {
		// Use e.logFn (not the package log) so unit tests can capture
		// this line via the engine's injected logger. disp.Mode is
		// included so a future investigation can see which operator-
		// declared disposition was overridden by the supply-bin guard.
		e.logFn("release: order=%d node=%s disposition=%q — skipping manifest sync: supply_bin_guard (two-robot swap)",
			orderID, node.Name, string(disp.Mode))
		manifestUOP = nil
	}

	// Capture lineside buckets (conditional on disposition) and always
	// deactivate other styles on this node.
	capturedTotal, err := e.captureLinesideOnRelease(node, toClaim, disp)
	if err != nil {
		return err
	}

	// Emit BinUOPDelta(capture_reduction) for the released bin. The
	// bucket fills already shipped via captureLinesideOnRelease; this
	// is the paired bin-side decrement. Suppressed on supply orders
	// (Order A in two-robot swaps): a fresh supply bin had nothing
	// pulled from it, so applying capture_reduction would corrupt the
	// authoritative count. Same isSupply gate the manifestUOP
	// suppression uses.
	//
	// PayloadCode comes from the order, not the claim. The bin being
	// captured-from is the order's bin (delivered or evac); its payload
	// is what the order recorded at create-time. toClaim.PayloadCode is
	// the target-style template, which differs from the bin's payload
	// during changeover or reassignment scenarios — passing it would
	// trip Core's payload-mismatch validation in
	// inventory_delta_service.ApplyBinUOPDelta and the delta would be
	// silently rejected.
	//
	// IMPORTANT: this rides ALONGSIDE the legacy RemainingUOP=&0 send
	// (which Core's existing manifest-sync still applies). The legacy
	// send and the capture_reduction delta both target the same
	// authoritative bins.uop_remaining; net result is correct because
	// the legacy send wipes-then-the-delta-reduces and "no manifest
	// = no bin" semantics make the resulting count consistent. The
	// dual-write removal is a future cleanup item.
	if e.inventoryDelta != nil && capturedTotal > 0 && !isSupply {
		if order.BinID != nil {
			e.inventoryDelta.RecordBin(*order.BinID, order.PayloadCode,
				-capturedTotal, protocol.ReasonCaptureReduction)
		}
	}

	// Runtime UOP reset moved to delivery completion (handleChangeoverRelease /
	// handleComplexOrderBCompletion / handleKeepStagedOrderBCompletion). If
	// the line resets at release-time and the supply bin then fails to arrive
	// (robot fault, network blip between release and FINISHED), the operator
	// is left looking at a "fresh" capacity number that doesn't match the
	// physical bin still on the node. Resetting at delivery binds the UOP
	// turnover to the moment the new bin is actually present.
	if nodeTask != nil {
		if err := e.db.UpdateChangeoverNodeTaskState(nodeTask.ID, "released"); err != nil {
			e.logFn("release: update node task %d to released: %v", nodeTask.ID, err)
		}
	}

	// Flush boundary + pending-delta guard: drain any accumulated
	// bin/bucket deltas for this scope to the outbox before shipping
	// the OrderRelease envelope. After the flush, if the reporter
	// still reports the bin as pending the enqueue failed transiently
	// — abort the release so it doesn't race with the next flush
	// attempt at Core. The operator retries; the periodic loop
	// recovers the outbox in the meantime.
	if e.inventoryDelta != nil {
		e.inventoryDelta.Flush()
		if order.BinID != nil && e.inventoryDelta.IsPendingBinDelta(*order.BinID) {
			return ErrCountChangePending
		}
	}

	if err := e.orderMgr.ReleaseOrderWithDisposition(orderID, manifestUOP, wireDisposition, disp.CalledBy); err != nil {
		return err
	}

	return nil
}

// ErrCountChangePending is returned by ReleaseOrderWithLineside when the
// reporter still has unflushed deltas for the released bin after a
// flush attempt. The operator UI surfaces this as "count change
// pending, please retry" — the in-flight delta needs to land at Core
// before the manifest-sync release can ship.
var ErrCountChangePending = errors.New("count change pending — retry release after delta flush completes")

// computeReleaseRemainingUOP turns the operator's declared disposition into
// the *int that gets sent on the OrderRelease envelope.
//
// The empty-Mode case returns nil deliberately: it preserves the
// pre-disposition behavior for callers that don't opt in (Order A in
// two-robot swaps, fallback paths, older HTTP clients posting bare bodies).
// Without this gate, any zero-value ReleaseDisposition would silently
// start sending remaining_uop=0 and clearing manifests Core has never
// touched on release before — including Order A's freshly-loaded supply bin.
//
// SendPartialBack source priority (Phase 0b):
//  1. disp.PartialCount (operator-entered via the keypad) when set and >0.
//     Per the SME contract (plan §2.5) the operator's count is ground truth.
//  2. runtime.RemainingUOPCached when >0. Fallback for legacy HTTP clients that
//     don't ship the override-aware body shape.
//  3. &0 otherwise — no positive baseline to preserve, declare empty.
func computeReleaseRemainingUOP(disp ReleaseDisposition, runtime *processes.RuntimeState) *int {
	switch disp.Mode {
	case DispositionCaptureLineside:
		// RELEASE EMPTY (no captures, just operator-confirmed empty)
		// keeps the legacy &0 path: Core's SyncOrClearForReleased(0)
		// wipes the manifest and audits as released_empty.
		//
		// PULL PARTS LINESIDE (with captures) returns nil — the
		// BinUOPDelta(capture_reduction) is now the single writer to
		// bins.uop_remaining; Core's capture-reduction-to-zero
		// trigger handles the manifest clear and audits as
		// released_capture_empty. Item 6 of the bin-as-truth refactor
		// retires the dual-write at this site.
		if len(disp.LinesideCapture) == 0 {
			zero := 0
			return &zero
		}
		return nil
	case DispositionSendPartialBack:
		if disp.PartialCount != nil && *disp.PartialCount > 0 {
			v := *disp.PartialCount
			return &v
		}
		if runtime != nil && runtime.RemainingUOPCached > 0 {
			v := runtime.RemainingUOPCached
			return &v
		}
		// Non-positive runtime UOP: nothing to preserve, fall through to empty.
		zero := 0
		return &zero
	case DispositionReleaseUnderpack:
		// Same wire shape as RELEASE EMPTY — bin physically empty,
		// manifest cleared at Core. The audit-tag distinction lives
		// in the disposition Kind (released_underpack) which
		// buildProtocolDisposition threads to Core.
		zero := 0
		return &zero
	default:
		// "" / unknown mode → backward-compat: no manifest action.
		return nil
	}
}

// buildProtocolDisposition translates the Edge-side ReleaseDisposition
// into the wire-shape protocol.UOPDisposition. Phase 0b: rides alongside
// the legacy RemainingUOP field on OrderRelease and carries the
// suggested baselines for Core's override audit.
//
// Mode mapping:
//
//   - DispositionCaptureLineside with non-empty captures → DispositionPullParts
//   - DispositionCaptureLineside with no captures → DispositionReleaseEmpty
//     (matches the current "RELEASE EMPTY" UI body shape: capture_lineside
//     with an empty qty_by_part map)
//   - DispositionSendPartialBack → DispositionReleasePartial. Count comes
//     from PartialCount when set, else from runtime.RemainingUOPCached — same
//     priority as computeReleaseRemainingUOP.
//   - "" (zero Mode) → nil. Legacy callers ship no Disposition.
//
// Returns nil when there's nothing meaningful to ship (preserves the
// "no manifest action" semantic).
func buildProtocolDisposition(disp ReleaseDisposition, runtime *processes.RuntimeState) *protocol.UOPDisposition {
	switch disp.Mode {
	case DispositionCaptureLineside:
		// Empty captures map === RELEASE EMPTY in the current UI.
		if len(disp.LinesideCapture) == 0 {
			return &protocol.UOPDisposition{Kind: protocol.DispositionReleaseEmpty}
		}
		return &protocol.UOPDisposition{
			Kind:              protocol.DispositionPullParts,
			Captures:          disp.LinesideCapture,
			CapturesSuggested: disp.LinesideCaptureSuggested,
		}
	case DispositionSendPartialBack:
		d := &protocol.UOPDisposition{Kind: protocol.DispositionReleasePartial}
		switch {
		case disp.PartialCount != nil && *disp.PartialCount > 0:
			d.Count = *disp.PartialCount
		case runtime != nil && runtime.RemainingUOPCached > 0:
			d.Count = runtime.RemainingUOPCached
		}
		d.CountSuggested = disp.PartialCountSuggested
		return d
	case DispositionReleaseUnderpack:
		// CountSuggested carries the system's expected count (the
		// runtime cache at click time). Core's bin_uop_audit row
		// will pick up before_uop = current bins.uop_remaining,
		// after_uop = 0; the suggested_uop - after_uop gap is the
		// missing-inventory delta forensics read.
		d := &protocol.UOPDisposition{Kind: protocol.DispositionReleaseUnderpack}
		if runtime != nil {
			v := runtime.RemainingUOPCached
			d.CountSuggested = &v
		}
		return d
	default:
		return nil
	}
}

// isSupplyOrderInActiveTwoRobotSwap reports whether the given order is the
// supply order (Order A) in a currently-staged two-robot swap on the given
// node. Used by ReleaseOrderWithLineside to suppress the manifest sync for
// Order A — the supply bin coming from the supermarket should never have
// its manifest cleared at release time, only the evac bin (Order B) at the
// line should.
//
// Identification: in a two-robot swap, the runtime tracks both order IDs
// via UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, &orderB.ID). The
// first slot (ActiveOrderID) is Order A (supply); the second
// (StagedOrderID) is Order B (evac). The claim's SwapMode must be
// "two_robot". All three signals must be present — we don't want this
// guard firing on non-two-robot orders that happen to share a node ID.
//
// Returns false on any DB read error (defensive — better to allow the
// release than block it on a transient lookup failure).
func (e *Engine) isSupplyOrderInActiveTwoRobotSwap(nodeID, orderID int64) bool {
	runtime, err := e.db.GetProcessNodeRuntime(nodeID)
	if err != nil || runtime == nil {
		return false
	}
	if runtime.ActiveOrderID == nil || runtime.StagedOrderID == nil {
		return false
	}
	if *runtime.ActiveOrderID != orderID {
		// The order being released isn't the supply slot — either it's
		// the evac order (Order B in StagedOrderID) or an unrelated order
		// that just shares the node. Manifest sync is fine for the evac
		// order; that's the path designed to clear the line bin.
		return false
	}
	if runtime.ActiveClaimID == nil {
		return false
	}
	claim, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID)
	if err != nil || claim == nil {
		return false
	}
	return claim.SwapMode == "two_robot"
}

// resolveReleaseClaim returns the claim whose capacity the release
// should reset UOP against, plus the changeover node task when one is
// active. For a changeover the target is the to-style claim; otherwise
// it's the currently-active claim on the node.
func (e *Engine) resolveReleaseClaim(node *processes.Node, runtime *processes.RuntimeState) (*processes.NodeClaim, *processes.NodeTask) {
	if changeover, err := e.db.GetActiveProcessChangeover(node.ProcessID); err == nil {
		if toClaim, err := e.db.GetStyleNodeClaimByNode(changeover.ToStyleID, node.CoreNodeName); err == nil {
			if task, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, node.ID); err == nil {
				return toClaim, task
			}
			return toClaim, nil
		}
	}
	if runtime.ActiveClaimID == nil {
		return nil, nil
	}
	claim, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID)
	if err != nil {
		return nil, nil
	}
	return claim, nil
}

// captureLinesideOnRelease records any parts the operator pulled to
// lineside (only when the disposition is capture_lineside) and always
// deactivates buckets for other styles on this node. The deactivation
// fires on every disposition because the release click is the "this
// node is running this style now" moment regardless of where the bin
// is heading.
//
// Note: count for the SEND PARTIAL BACK disposition is captured at
// release-click time, not at robot-pickup time. For two-robot swaps,
// the evacuation robot picks up the old bin moments after release —
// any further consumption between click and pickup isn't tracked. The
// operator's intent at click time is the source of truth.
//
// Emits one LinesideBucketDelta(capture_fill, +qty) per captured
// (style, part). Returns the sum of captured qty so the caller can
// emit the paired BinUOPDelta(capture_reduction) after the supply-bin
// guard resolves.
func (e *Engine) captureLinesideOnRelease(node *processes.Node, toClaim *processes.NodeClaim, disp ReleaseDisposition) (capturedTotal int, err error) {
	pairKey := toClaim.PairedCoreNode

	// Only capture parts when disposition is capture_lineside. Other
	// dispositions (send_partial_back, empty/default) skip the bucket loop
	// entirely.
	if disp.Mode == DispositionCaptureLineside {
		for part, qty := range disp.LinesideCapture {
			if qty <= 0 || part == "" {
				continue
			}
			if _, err := e.db.CaptureLinesideBucket(node.ID, pairKey, toClaim.StyleID, part, qty); err != nil {
				return capturedTotal, fmt.Errorf("capture lineside bucket (node=%d style=%d part=%s): %w",
					node.ID, toClaim.StyleID, part, err)
			}
			if e.inventoryDelta != nil {
				e.inventoryDelta.RecordBucket(node.ID, pairKey, toClaim.StyleID, part, qty, protocol.ReasonCaptureFill)
			}
			capturedTotal += qty
		}
	}

	// Always deactivate other styles on this node.
	if err := e.db.DeactivateOtherLinesideStyles(node.ID, toClaim.StyleID); err != nil {
		return capturedTotal, fmt.Errorf("deactivate other lineside styles on node %d: %w", node.ID, err)
	}
	return capturedTotal, nil
}
