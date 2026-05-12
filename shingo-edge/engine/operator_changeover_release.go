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

	"shingoedge/orders"
	"shingoedge/store/processes"
)

// ReleaseChangeoverWaitResult reports the outcome of a release-wait click so
// the frontend can show the operator how much actually happened. Released is
// the count of legs whose OrderRelease envelopes were queued this call;
// Pending is the count of legs that exist but weren't in staged status yet
// (still sourcing / in_transit / etc.) and so were silently skipped — those
// are the legs the operator may need to come back for on a second click.
// Already-terminal legs (released earlier, cancelled, failed) are not
// counted in either field.
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
	var result ReleaseChangeoverWaitResult

	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		return result, err
	}
	tasks, err := e.db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		return result, err
	}

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
