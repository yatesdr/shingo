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
	// NeedsFlip names the A/B positions a SWEEP declined to release because the
	// line is still pulling from them. A sweep carries no per-node intent, so it
	// can never answer the confirm the guard asks for — it reports them instead,
	// by name, so the operator knows exactly which presses still want a click.
	NeedsFlip []string `json:"needs_flip,omitempty"`
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
//     DRAINED (RemainingUOPCached == 0), the evac is release_empty — manifest
//     cleared, preserving the 2026-04 ALN_001 fix intent (bin can't land at
//     OutboundDestination tagged with stale payload). The operator never
//     types a number; the system already knows it.
//
//     DRAINED IS THE EDGE COUNT, "EMPTY" IS WHAT THE RELEASE MAKES IT. The bin
//     still carries its payload and its manifest while the counter reads zero;
//     clearing the manifest is what turns it into the empty carrier Core's own
//     vocabulary means by that word. The mode is named release_empty because it
//     describes the outcome, not the precondition.
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
	res, err := e.ReleaseChangeoverWaitForNode(node.ProcessID, nodeID, disp)
	if err != nil {
		return true, err
	}
	e.logFn("release-staged node=%s: single-leg changeover release — released=%d pending=%d",
		node.Name, res.Released, res.Pending)
	return true, nil
}

// linePullsFrom reports whether a node is one half of an A/B pair that the line
// is CURRENTLY DRAWING FROM, and names its partner.
//
// That is the physical reason a robot must not strip a position, and it is the
// whole of it — no changeover vocabulary, no situation, no mode. It is equally
// true in steady state: sending a robot to lift the bin the line is pulling
// from stops production whether or not a changeover is running.
//
// Reads nothing it does not need: the node's active claim for the A/B geometry
// (PairedCoreNode, the same predicate wiring.go uses for "is this the parked
// side"), and the runtime row for the bit.
func (e *Engine) linePullsFrom(nodeID int64) (pulling bool, own, partner string, err error) {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil || node == nil {
		return false, "", "", fmt.Errorf("read process node %d: %w", nodeID, err)
	}
	claim := findActiveClaim(e.db, node)
	if claim == nil || claim.PairedCoreNode == "" {
		return false, node.CoreNodeName, "", nil // not a paired position — nothing to say
	}
	// ── SEQUENTIAL ONLY, AND NOT AS AN EXCEPTION ──────────────────────────
	//
	// This was written mode-agnostically, on "half an A/B pair", and the sim
	// refused it: TestReleaseStagedOrders went red across two_robot and
	// press-index because those modes RELEASE AT THE ACTIVE PULL POINT by
	// design. A press-index front position is the active one AND the one being
	// swapped — the index motion moves bins between positions with the press
	// still running — so "do not strip the position the line is pulling from" is
	// simply not a true statement about that choreography.
	//
	// It is true of SEQUENTIAL and only of sequential, because that is the mode
	// whose whole premise is that the OTHER position takes over first: the flip
	// is what makes this side safe to clear. The scope is not a carve-out for a
	// mode; it is the rule being stated about the choreography it describes.
	if claim.SwapMode != protocol.SwapModeSequential {
		return false, node.CoreNodeName, claim.PairedCoreNode, nil
	}
	rt, err := e.db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		return false, node.CoreNodeName, claim.PairedCoreNode,
			fmt.Errorf("read runtime for node %d: %w", nodeID, err)
	}
	return rt.ActivePull, node.CoreNodeName, claim.PairedCoreNode, nil
}

// activePullGuard decides what a release click may do at one task's node.
//
// ── A SPEED BUMP, NOT A WALL (owner ruling 2026-08-28) ────────────────────
//
// `active_pull` is a bit, and bits go stale — a PLC that missed an edge, a
// runtime row written before someone moved a bin by hand. The person standing
// at the press can see the aisle and the system cannot, so the guard states the
// fact and names the next click; it never outranks him. An explicit confirm
// releases anyway and is audited.
//
// THE SWEEP CANNOT CONFIRM. A plant-wide release carries no per-node intent —
// a supervisor letting robots into six stopped stations has not looked at Press
// 2's aisle — so it declines and reports the node by name rather than deciding
// on his behalf. That is not an exception for a mode; it is the difference
// between a click aimed at one press and a click aimed at all of them.
//
// An unreadable role declines the same way for the same reason.
func (e *Engine) activePullGuard(task processes.NodeTask, onlyNodeID int64, disp ReleaseDisposition) (skip bool, err error) {
	pulling, own, partner, pErr := e.linePullsFrom(task.ProcessNodeID)
	if pErr != nil {
		log.Printf("release changeover wait node %s: %v — declining rather than releasing on an "+
			"unread pull state", task.NodeName, pErr)
		if onlyNodeID != 0 {
			return true, fmt.Errorf("node %s: could not read whether the line is pulling from it (%w)",
				task.NodeName, pErr)
		}
		return true, nil
	}
	if !pulling {
		return false, nil
	}
	// ── THE SWEEP ARM COMES FIRST, AND THE ORDER IS THE POINT ─────────────
	//
	// A confirm is an answer about ONE aisle: the operator looked at this press
	// and said release anyway. A plant-wide click was not aimed at one press, so
	// it cannot carry that answer — and if this arm sat below the confirm check,
	// a single confirm on a sweep would spend itself on every press at once,
	// which is the whole guard undone by one flag.
	if onlyNodeID == 0 {
		log.Printf("release changeover wait: node %s skipped — the line is pulling from it and a "+
			"plant-wide sweep cannot confirm on the operator's behalf", own)
		return true, nil
	}
	if disp.ConfirmActivePull {
		log.Printf("AUDIT release-override: node=%s order=%v called_by=%q — the line was recorded as "+
			"pulling from this position and the operator released it anyway",
			own, task.NextMaterialOrderID, disp.CalledBy)
		return false, nil
	}
	return true, fmt.Errorf("the line is pulling from %s; flip to %s first, or confirm to release anyway",
		own, partner)
}

// coreNameOf is the node's CORE name — what the flip button and the board key
// on — falling back to the display name if the row cannot be read.
func coreNameOf(e *Engine, task processes.NodeTask) string {
	if n, err := e.db.GetProcessNode(task.ProcessNodeID); err == nil && n != nil && n.CoreNodeName != "" {
		return n.CoreNodeName
	}
	return task.NodeName
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
	// ConfirmActivePull rides along. It is an override of a PHYSICAL guard, not a
	// manifest instruction, so stripping it with the mode would leave the
	// operator's confirm answered at this layer and refused one call later by the
	// trunk guard in ReleaseOrderWithLineside — which is exactly what happened
	// the first time.
	supplyDisp := ReleaseDisposition{CalledBy: disp.CalledBy, ConfirmActivePull: disp.ConfirmActivePull}

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
		// A ROBOT MAY NOT STRIP A POSITION THE LINE IS PULLING FROM.
		// See activePullGuard: refuse-by-default with an operator override,
		// and a sweep declines because it cannot answer for him.
		if skip, gErr := e.activePullGuard(task, onlyNodeID, disp); skip {
			if gErr != nil {
				return result, gErr
			}
			result.NeedsFlip = append(result.NeedsFlip, coreNameOf(e, task))
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
		return ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: override.CalledBy,
			ConfirmActivePull: override.ConfirmActivePull}
	}

	if runtime != nil && runtime.RemainingUOPCached > 0 {
		count := runtime.RemainingUOPCached
		return ReleaseDisposition{
			Mode:              DispositionSendPartialBack,
			ConfirmActivePull: override.ConfirmActivePull,
			PartialCount:      &count,
			CalledBy:          override.CalledBy,
		}
	}
	return ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: override.CalledBy,
		ConfirmActivePull: override.ConfirmActivePull}
}
