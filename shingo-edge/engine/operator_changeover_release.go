// operator_changeover_release.go — operator-driven release of staged
// changeover orders (the "release wait" click).
//
// ReleaseChangeoverWait fires the evac leg at click time and defers the
// supply leg to HandleBinPickedUp (Phase 2 evac-first sequencing).
// evacDispositionForTask picks send_partial_back vs release_empty from
// the line's runtime cache, with operator override.

package engine

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// ReleaseChangeoverWaitResult reports the outcome of a release-wait click so
// the frontend can show the operator how much actually happened. Released is
// the count of legs whose OrderRelease envelopes were queued this call;
// Pending is the count of legs that exist but could not be released this
// call — either a supply leg deliberately deferred to evac-pickup confirm, or
// a leg Core would refuse because it has not reached staged yet (queued /
// sourcing / dispatched / acknowledged). Both are legs the operator may need
// to come back for on a second click. Already-terminal legs (released
// earlier, cancelled, failed) are not counted in either field.
type ReleaseChangeoverWaitResult struct {
	Released int `json:"released"`
	Pending  int `json:"pending"`
}

// ReleaseChangeoverWait releases all evacuation orders that are currently staged
// (waiting at a wait step). Called once per operator gate:
//   - First call releases the "ready" wait on all nodes
//   - For evacuate nodes, orders stage again at the second wait, and the second
//     call releases "tooling done"
//
// Per-slot disposition: each task carries up to two staged orders — the evac
// leg (OldMaterialReleaseOrderID) and the supply leg (NextMaterialOrderID).
// They get DIFFERENT dispositions:
//
//   - Evac leg: auto-detected per task from the line's runtime cache. If the
//     line still has parts (RemainingUOPCached > 0), the evac is sent as
//     send_partial_back with that exact count — Core syncs the bin's
//     manifest to the partial value at release time, and the bin arrives at
//     the supermarket flagged as partial with the right qty. If the line is
//     empty (RemainingUOPCached == 0), the evac is release_empty — manifest
//     cleared, preserving the 2026-04 ALN_001 fix intent (bin can't land at
//     OutboundDestination tagged with stale payload). The operator never
//     types a number; the system already knows it.
//
//     The caller's disposition (passed in `disp`) acts as an override: if
//     they supplied Mode=send_partial_back with a PartialCount, that count
//     wins over the runtime auto-detect. Useful for future flows where an
//     operator manually overrides the cached value, but the default path
//     (no modal, just a click) bypasses operator entry entirely.
//
//   - Supply leg: receives Mode="" (zero-value) regardless of anything else.
//     buildProtocolDisposition translates this to nil on the wire, and
//     Core's SyncOrClearForReleased hits the no-op branch — the supply
//     bin's manifest is left alone. The supply bin is mid-transit from the
//     supermarket carrying its real uop_remaining; applying any evac-leg
//     disposition would zero a manifest that should ride through to
//     delivery. (Confirmed regression on plant order 682 / 2026-05-06.)
//
// TODO: expand to per-bin disposition flow when a plant scenario needs
// it (e.g., operator override of the runtime count, or different
// dispositions per evac bin). Engine is already neutral; this is a
// frontend + handler-shape change.
//
// disp.CalledBy is plumbed through for audit on both legs.
//
// F' Phase 2 — evac-first sequencing for paired tasks.
//
// When a task has both an evac leg (OldMaterialReleaseOrderID) and a
// supply leg (NextMaterialOrderID), only the evac fires at click time.
// The supply leg auto-releases mid-evac, when the evac robot finishes
// picking up the bin and starts moving away from the slot. This is
// NOT when the evac order is fully complete (drop at outbound is later);
// it's when the pickup block within the evac order transitions to
// FINISHED, which is precisely the moment the slot is physically clear
// for the supply robot. Core's RDS poller emits the per-block FINISHED
// transition and publishes BinPickedUp; handler_bin_picked_up.go's
// HandleBinPickedUp looks up the paired supply order via
// GetChangeoverNodeTaskByEvacOrderID (NOT SiblingOrderID — that's used
// by operator-station two-robot paths and is intentionally untouched
// here) and calls releaseUnlessTerminal on it. This eliminates the
// crash-race window where the supply robot could arrive at the slot
// before the evac robot has cleared it.
//
// Pre-Phase-2 behaviour: both legs fired together, gated on
// Status==Staged. If the operator clicked the changeover-wide release
// before R1 was at its wait point, the staged-only switch made the
// click a no-op — but flipping to "release any non-terminal" without
// the evac-first defer would race R2 to the slot. Phase 2 collapses
// the staged-only switch (Friday-incident fix) AND adds the defer
// (the safer architecture the collapse demands).
//
// Result.Pending: includes both deferred-supply legs (non-terminal,
// will fire on evac pickup) and any standalone-leg orders we skipped
// because they weren't in a releasable state at click time.
func (e *Engine) ReleaseChangeoverWait(processID int64, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	return e.releaseChangeoverWaitScoped(processID, 0, disp)
}

// ReleaseChangeoverWaitForNode releases only the task at one process node.
//
// Same function, same evac-first sequencing, same per-slot disposition rules —
// only the task set is narrower. Per-node release is the operator's ACTUAL
// workflow (the changeover-wide header button was removed in 2026-05-10 HMI
// Tier 2), so it routes through this one path rather than a second engine
// method that would drift from it.
func (e *Engine) ReleaseChangeoverWaitForNode(processID, processNodeID int64, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	return e.releaseChangeoverWaitScoped(processID, processNodeID, disp)
}

// releaseSingleLegChangeoverNode handles the per-node RELEASE click for a node
// whose changeover work is NOT a coordinated swap pair.
//
// A cleared position's leg is one order on one robot: lift the bin off the position,
// take it wherever this cell's bins go, fetch the replacement, hold it at
// staging, set it down when the operator says the setup is finished. There is
// no sibling because there is no second robot, and ResolveSwapPair — which
// exists to keep a two-robot swap's two legs together — refused it for that:
// "order 58 has no sibling — not a coordinated pair (single-leg flow should use
// per-order release)". A position node has no claim row either, so the same click
// could equally bounce on "no active claim for release". Both were the operator
// pressing the only release button in front of them and getting nothing (N1-d,
// sim 2026-08-24).
//
// The changeover path already releases exactly this shape, and correctly: it
// fires a lone supply leg at click time and defers one that has an evac sibling
// to that sibling's pickup. So route to it, and leave every pair on the pair
// path — handled reports whether this took the click.
func (e *Engine) releaseSingleLegChangeoverNode(nodeID int64, disp ReleaseDisposition) (handled bool, err error) {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil || node == nil {
		return false, nil
	}
	changeover, err := e.db.GetActiveProcessChangeover(node.ProcessID)
	if err != nil || changeover == nil {
		return false, nil // not in a changeover; the pair path owns this node
	}
	task, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, nodeID)
	if err != nil || task == nil {
		return false, nil
	}
	// A task with BOTH legs is a coordinated pair and stays on the pair path,
	// which has its own inversion handling and collision gate. Only the
	// single-leg shape comes here.
	if task.OldMaterialReleaseOrderID != nil && task.NextMaterialOrderID != nil {
		return false, nil
	}
	if task.OldMaterialReleaseOrderID == nil && task.NextMaterialOrderID == nil {
		return false, nil // nothing to release; let the pair path say so
	}
	// ── AT A SEQUENTIAL SWAP, THIS BUTTON IS THE CUTOVER ──────────────────
	//
	// The operator is standing at the press. Their click means "go" — and at
	// this one position "go" is not a bare permission, it is flip the pull to
	// the freshly-stocked side and THEN let the robot clear this one. Routing
	// here rather than adding a second button is the point: one control that
	// does the correct thing, instead of a real button beside a trap that
	// releases the wait with the line still pulling from the position.
	//
	// The precondition comes along for free — the parked side must have its bin
	// before the press can be cut onto it — because it lives in the handler this
	// now calls rather than in the button.
	//
	// SCOPED TO THE ACTIVE SIDE. The parked position's order has no wait; it ran
	// at dispatch. Handing its node id to the cutover would flip the pull and
	// then fail on a wait that does not exist, so it stays on the ordinary path
	// below, where an already-running order is simply not releasable.
	isSeqSwap, isActiveSide, roleErr := e.sequentialSwapRole(task)
	if roleErr != nil {
		return true, fmt.Errorf("release node %s: could not tell whether this is a sequential cutover "+
			"(%w) — refusing rather than risk plain-releasing a cutover wait", node.Name, roleErr)
	}
	if isSeqSwap && isActiveSide {
		e.logFn("release-staged node=%s: sequential swap — routing to cutover (flip pull, then release)",
			node.Name)
		return true, e.SequentialChangeoverCutover(node.ProcessID, nodeID, disp.CalledBy)
	}
	res, err := e.ReleaseChangeoverWaitForNode(node.ProcessID, nodeID, disp)
	if err != nil {
		return true, err
	}
	e.logFn("release-staged node=%s: single-leg changeover release — released=%d pending=%d",
		node.Name, res.Released, res.Pending)
	return true, nil
}

// sequentialSwapRole classifies a changeover node task for the two release
// doors: is it a sequential SWAP — whose releaser is the cutover and not a plain
// release — and if so, is THIS node the active side, the one whose order carries
// the cutover wait.
//
// ── WHY THE SITUATION AND NOT THE MODE ────────────────────────────────────
//
// A sequential EVACUATE also parks a wait on each position, and that one is a
// BARE wait released by the tooling-done click through the plant-wide fan-out —
// one click, both positions, which is its entire design. Keying either door on
// "the mode is sequential" would break every A/B press's tooling changeover. The
// wait that must not be swept is specifically the SWAP's cutover wait.
//
// ── AND WHY THE SIDE MATTERS ──────────────────────────────────────────────
//
// Only the active position's order opens with the cutover wait; the parked one
// dispatches and runs. SequentialChangeoverCutover resolves inactive/active from
// the snapshot itself, so handing it the PARKED node's id would still flip the
// pull and then fail releasing an order with no wait — production moved, robot
// still parked. So the reroute is scoped to the active side and the parked side
// keeps the ordinary path, where its already-running order is simply not
// releasable.
//
// Errors are returned rather than swallowed because the two callers want
// opposite things from "I could not tell", and both of those are "do not act":
// the sweep skips (it must not flip a press on a guess) and the per-node click
// refuses (it must not plain-release what might be a cutover wait).
func (e *Engine) sequentialSwapRole(task *processes.NodeTask) (isSeqSwap, isActiveSide bool, err error) {
	if task == nil || task.Situation != "swap" || task.FromClaimID == nil {
		return false, false, nil
	}
	fromClaim, err := e.db.GetStyleNodeClaim(*task.FromClaimID)
	if err != nil {
		return false, false, fmt.Errorf("read from-claim for node task %d: %w", task.ID, err)
	}
	if fromClaim == nil || fromClaim.SwapMode != protocol.SwapModeSequential || fromClaim.PairedCoreNode == "" {
		return false, false, nil
	}
	processNode, err := e.db.GetProcessNode(task.ProcessNodeID)
	if err != nil || processNode == nil {
		return true, false, fmt.Errorf("read process node %d for sequential task %d: %w",
			task.ProcessNodeID, task.ID, err)
	}
	nodes, err := e.db.ListProcessNodesByProcess(processNode.ProcessID)
	if err != nil {
		return true, false, fmt.Errorf("list process nodes for sequential task %d: %w", task.ID, err)
	}
	_, active := resolveSequentialActivePull(fromClaim, e.activePullSnapshot(nodes))
	return true, processNode.CoreNodeName == active, nil
}

// sweepSkipsSequentialSwap is the plant-wide release's one policy exception, and
// the caller counts a skip as Pending.
//
// At every other mode a release is robot permission, so sweeping it plant-wide
// is harmless. At a sequential SWAP the wait is the CUTOVER wait: releasing it
// changes which position the press draws parts from, and releasing it without
// flipping the pull first sends a robot to clear the position the line is still
// running on.
//
// A supervisor letting robots into six stopped stations has not decided to
// change what Press 2 is producing. That is a production decision and it belongs
// to the cutover button at the press — which the per-node click routes to, so
// the skip costs the operator nothing but a click they were going to make
// anyway. Skipping on an unreadable role too: a press must not be cut over on a
// guess.
//
// PENDING, NOT A FAILURE. One skipped node must not turn the sweep's response
// into an error for five unrelated stations, and Pending already means "still
// waiting on a click".
func (e *Engine) sweepSkipsSequentialSwap(task *processes.NodeTask) bool {
	isSeqSwap, _, err := e.sequentialSwapRole(task)
	if err == nil && !isSeqSwap {
		return false
	}
	why := "its releaser is the cutover"
	if err != nil {
		why = fmt.Sprintf("its role could not be read (%v), so it is left alone", err)
	}
	log.Printf("release changeover wait: skipping sequential swap at node %s — %s. The cutover "+
		"button at that press flips the pull and then releases; a sweep that released this wait "+
		"would send a robot into the position the line is still pulling from", task.NodeName, why)
	return true
}

// releaseChangeoverWaitScoped is the shared body. onlyNodeID == 0 means every
// task (the changeover-wide release); non-zero narrows to that node.
func (e *Engine) releaseChangeoverWaitScoped(processID, onlyNodeID int64, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	var result ReleaseChangeoverWaitResult

	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return result, err
	}
	tasks, err := e.db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		return result, err
	}

	// Entry log. Until now this function produced NO record of having been
	// called — only per-slot failures logged, so a click that released
	// nothing (every leg terminal, or every task "unchanged") was
	// indistinguishable in the logs from a click that never happened. That
	// ambiguity is what left "did the operator actually press release?"
	// unanswerable in the Hopkinsville changeover post-mortems. One line per
	// invocation, before any slot is examined, so the record exists even when
	// the loop does nothing.
	scope := "all-nodes"
	if onlyNodeID != 0 {
		scope = fmt.Sprintf("node=%d", onlyNodeID)
	}
	e.logFn("release_changeover_wait: process=%d changeover=%d tasks=%d scope=%s called_by=%q",
		processID, changeover.ID, len(tasks), scope, disp.CalledBy)

	// Supply leg always rides through with no manifest action regardless of
	// what the operator chose. Empty Mode → buildProtocolDisposition returns
	// nil → Core no-op. CalledBy still flows for audit.
	supplyDisp := ReleaseDisposition{CalledBy: disp.CalledBy}

	// Collect per-task failures rather than swallowing them. Pre-fix
	// behaviour was log-and-continue + return nil, which silently recreated
	// the original ALN_001 incident on partial failure: one node's manifest
	// stays stale, the operator gets a 200 OK, and the bin loader can't
	// move that bin. Returning errors.Join ensures the handler surfaces
	// the failed node names instead of lying about success.
	var failures []error
	for _, task := range tasks {
		if task.Situation == "unchanged" {
			continue
		}
		if onlyNodeID != 0 && task.ProcessNodeID != onlyNodeID {
			continue
		}
		// ── THE SWEEP DOES NOT CUT A PRESS OVER ───────────────────────
		//
		// At every other mode a release is robot permission, so sweeping it
		// plant-wide is harmless. At a sequential SWAP the wait is the CUTOVER
		// wait: releasing it changes which position the press draws parts from,
		// and releasing it without flipping the pull first sends a robot to
		// clear the position the line is still running on.
		//
		// A supervisor letting robots into six stopped stations has not decided
		// to change what Press 2 is producing. That is a production decision and
		// it belongs to the cutover button at the press, which the per-node
		// click now routes to. So this skips and names it.
		//
		// PENDING, NOT A FAILURE: one skipped node must not turn the sweep's
		// response into an error for five unrelated stations, and Pending is
		// already the count that means "still waiting on a click".
		if onlyNodeID == 0 && e.sweepSkipsSequentialSwap(&task) {
			result.Pending++
			continue
		}
		// Auto-detect evac disposition from the line's runtime cache for
		// THIS task's node. Operator override (caller-supplied
		// SendPartialBack with a count) wins if present.
		evacDisp := evacDispositionForTask(e, task, disp)

		hasEvac := task.OldMaterialReleaseOrderID != nil
		hasSupply := task.NextMaterialOrderID != nil
		pairedEvacSupply := hasEvac && hasSupply

		type slot struct {
			id   *int64
			disp ReleaseDisposition
			kind string // for log/error context only
		}
		var slots []slot
		if hasEvac {
			slots = append(slots, slot{id: task.OldMaterialReleaseOrderID, disp: evacDisp, kind: "evac"})
		}
		// Supply leg fires at click time ONLY when there's no paired evac
		// (e.g., add-situation tasks). When paired with evac, we defer to
		// HandleBinPickedUp which fires the sibling release on evac pickup
		// confirm — see Phase 2 docstring above.
		if hasSupply && !pairedEvacSupply {
			slots = append(slots, slot{id: task.NextMaterialOrderID, disp: supplyDisp, kind: "supply"})
		}

		for _, s := range slots {
			if s.id == nil {
				continue
			}
			order, err := e.db.GetOrder(*s.id)
			if err != nil {
				log.Printf("release changeover wait node %s (%s): get order: %v", task.NodeName, s.kind, err)
				failures = append(failures, fmt.Errorf("node %s (%s): get order: %w", task.NodeName, s.kind, err))
				continue
			}
			if orders.IsTerminal(order.Status) {
				// Already released earlier, cancelled, or failed. No
				// operator action required.
				continue
			}
			if !orders.ReleasableAtCore(order.Status) {
				// Core refuses a release for anything that isn't staged or
				// in_transit, and Manager.ReleaseOrderWithDisposition would
				// force this row to in_transit anyway — leaving Edge and Core
				// disagreeing, and Released counting a release that never
				// happened. Count it Pending (the operator clicks again once
				// it stages) and say so in the log. This is the skip the
				// Pending docstring above has always promised.
				log.Printf("release changeover wait node %s (%s): order %d status=%q not releasable at Core — counting pending",
					task.NodeName, s.kind, order.ID, order.Status)
				result.Pending++
				continue
			}
			if err := e.ReleaseOrderWithLineside(order.ID, s.disp); err != nil {
				log.Printf("release changeover wait node %s (%s): %v", task.NodeName, s.kind, err)
				failures = append(failures, fmt.Errorf("node %s (%s): %w", task.NodeName, s.kind, err))
				continue
			}
			result.Released++
		}

		// Count deferred supply legs (paired-with-evac) so the operator
		// HMI can show "released N, M deferred for pickup-confirm." Skip
		// counting if the supply is already terminal.
		if pairedEvacSupply {
			supply, err := e.db.GetOrder(*task.NextMaterialOrderID)
			if err == nil && !orders.IsTerminal(supply.Status) {
				result.Pending++
			}
		}
	}
	return result, errors.Join(failures...)
}

// evacDispositionForTask picks the right evac-leg disposition. Operator
// override wins; otherwise auto-detect from the node's runtime cache.
//
//   - Caller passed Mode=send_partial_back with PartialCount > 0 → use it.
//   - Caller passed any other non-empty Mode → use it as-is (escape hatch
//     for future flows).
//   - Caller passed Mode="" → look up node runtime. If RemainingUOPCached >
//     0, send_partial_back with that count; else release_empty
//     (capture_lineside + empty captures → wire-form release_empty;
//     preserves the ALN_001 fix).
//
// On runtime lookup failure: fall back to release_empty rather than
// failing the whole release. The whole point of the manifest clear is to
// prevent stale payload at OutboundDestination — better to clear than to
// silently no-op when we can't read the current count.
func evacDispositionForTask(e *Engine, task processes.NodeTask, override ReleaseDisposition) ReleaseDisposition {
	if override.Mode != "" {
		return override
	}

	runtime, err := e.db.GetProcessNodeRuntime(task.ProcessNodeID)
	if err != nil {
		log.Printf("release changeover wait node %s: runtime lookup failed (%v); defaulting evac to release_empty", task.NodeName, err)
		return ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: override.CalledBy}
	}

	if runtime != nil && runtime.RemainingUOPCached > 0 {
		count := runtime.RemainingUOPCached
		return ReleaseDisposition{
			Mode:         DispositionSendPartialBack,
			PartialCount: &count,
			CalledBy:     override.CalledBy,
		}
	}
	return ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: override.CalledBy}
}
