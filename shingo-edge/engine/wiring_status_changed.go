// wiring_status_changed.go — handlers subscribed to EventOrderStatusChanged.
//
// Two handlers:
//   handleSequentialBackfill    – auto-create Order B (backfill) when
//                                 Order A enters in_transit on a sequential
//                                 swap-mode node.
//   handleSiblingReleaseRefire  – fire a two-robot swap leg's release the
//                                 moment it reaches staged, when that leg was
//                                 deferred by ReleaseStagedOrders (Core would
//                                 have refused it) while its sibling released
//                                 on the same operator click (hop A4-ii). Falls
//                                 through to releaseSurvivorOfFinishedPartner
//                                 when the in-memory deferral is not there to
//                                 find — the durable half of the same question.
//
// Wired by wireEventHandlers (wiring.go).
//
// History: handleAutoReleaseOnStaged was removed 2026-04-27 along with the
// auto-release coordination layer, on the theory that ReleaseStagedOrders
// fanning out to both legs unconditionally made a late-sibling auto-release
// unnecessary. The Hopkinsville press-index hang (2026-07-23) showed that
// unconditional fan-out desyncs a not-yet-releasable leg instead; hop A4-i
// makes the fan-out skip such a leg, and handleSiblingReleaseRefire is the
// TARGETED revival of the removed hook — scoped to a leg whose sibling already
// released, it re-fires (never cancels, never re-plans, no timer).

package engine

import (
	"log"

	"shingo/protocol"
	"shingoedge/orders"
)

// handleSequentialBackfill watches for sequential Order A going in_transit
// and auto-creates Order B (backfill) to deliver replacement material.
func (e *Engine) handleSequentialBackfill(changed OrderStatusChangedEvent) {
	if changed.NewStatus != string(protocol.StatusInTransit) || changed.ProcessNodeID == nil {
		return
	}
	order, err := e.db.GetOrder(changed.OrderID)
	if err != nil || order.ProcessNodeID == nil {
		return
	}
	node, err := e.db.GetProcessNode(*order.ProcessNodeID)
	if err != nil {
		return
	}
	runtime, err := e.db.EnsureProcessNodeRuntime(node.ID)
	if err != nil {
		return
	}

	// Only act on the active order (Order A) for this node
	if runtime.ActiveOrderID == nil || *runtime.ActiveOrderID != order.ID {
		return
	}
	// Don't create backfill if one already exists
	if runtime.StagedOrderID != nil {
		return
	}

	claim := findActiveClaim(e.db, node)
	if claim == nil || claim.SwapMode != protocol.SwapModeSequential {
		return
	}

	steps := BuildSequentialBackfillSteps(claim)
	nodeID := node.ID
	// ATTRIBUTED, and it was not. This order is the plant continuing to serve the
	// demand that produced Order A, so it belongs to that demand's episode — see
	// cellEpisodeOrigin for what an unattributed one costs downstream. It JOINS
	// and never mints: a backfill is never itself the origin of a demand.
	orderB, err := e.orderMgr.CreateComplexOrder(&nodeID, 1, claim.CoreNodeName, claim.CoreNodeName, steps,
		e.cellEpisodeOrigin(node, claim)) // delivery_node = CoreNodeName → resets UOP
	if err != nil {
		log.Printf("sequential backfill for node %s: %v", node.Name, err)
		return
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, runtime.ActiveOrderID, &orderB.ID); err != nil {
		log.Printf("update runtime orders for node %d: %v", nodeID, err)
	}
	// LinkOrderSiblings is log-and-continue here (unlike the three
	// operator-initiated sites which return-error). Rationale:
	//   - This runs in the OrderStatusChanged event handler loop; one
	//     handler failing must not abort message processing.
	//   - The backfill is opportunistic; if linkage fails, the L1/L2
	//     side-cycle still works for the operator (only the consolidated
	//     swap_ready RELEASE coordinates via siblings, and sequential
	//     mode is excluded from that path by the SwapMode gate).
	if err := e.db.LinkOrderSiblings(order.ID, orderB.ID); err != nil {
		log.Printf("link sequential siblings %d↔%d: %v", order.ID, orderB.ID, err)
	}
	log.Printf("sequential backfill: created Order B %d for node %s (Order A %d in_transit)", orderB.ID, node.Name, order.ID)
}

// handleSiblingReleaseRefire fires a two-robot swap leg's release the moment it
// reaches staged, when that leg's consolidated RELEASE was deferred by
// ReleaseStagedOrders because Core would have refused it while its sibling was
// released on the same operator click (hop A4-ii). It completes a release the
// operator ALREADY requested — never a reaper, never an auto-cancel — and fires
// only for a leg whose sibling already went (rememberDeferredSiblingRelease
// records the entry only in that case).
//
// On any terminal transition it drops a stale entry, so a deferred leg that is
// cancelled or fails (rather than reaching staged) can't leak the map.
func (e *Engine) handleSiblingReleaseRefire(changed OrderStatusChangedEvent) {
	newStatus := protocol.Status(changed.NewStatus)

	// Cleanup: a deferred leg that went terminal without ever staging will
	// never re-fire; drop it so the map can't grow without bound.
	if protocol.IsTerminal(newStatus) {
		e.pendingSiblingReleaseMu.Lock()
		delete(e.pendingSiblingRelease, changed.OrderID)
		e.pendingSiblingReleaseMu.Unlock()
		e.survivorReleasedMu.Lock()
		delete(e.survivorReleased, changed.OrderID)
		e.survivorReleasedMu.Unlock()
		// ── AND THE OTHER ORDER OF EVENTS ────────────────────────────────
		//
		// The survivor arm below fires when the SURVIVOR stages. This one fires
		// when the PARTNER finishes, and both are needed because the two events
		// arrive in either order and only one of them is guaranteed to happen at
		// all.
		//
		// MEASURED, run 2026-08-30 order 112 (partner 111). 112 was released
		// past its first wait at 14:19:24 and drove to its second, where the
		// fleet reported it WAITING — but Core never wrote a second `staged`, so
		// its row sat at `in_transit` with the robot parked. 111 confirmed a
		// minute later. There was no staged transition left for the arm below to
		// fire on, and 112 held AMR-19 for 28 minutes under a board reading
		// "Waiting for partner robot" about a partner that had finished.
		//
		// `in_transit` is RELEASABLE at Core (orders.ReleasableAtCore accepts it
		// for exactly this multi-wait re-release), so nothing about the release
		// itself needed changing — only something to ask for it.
		//
		// Same one-shot bound, same terminal-SUCCESS test, same refusal to touch
		// a leg whose partner DIED: releaseSurvivorOfFinishedPartner re-reads and
		// re-decides everything for itself.
		if orders.IsTerminalSuccess(newStatus) {
			if o, err := e.db.GetOrder(changed.OrderID); err == nil && o.SiblingOrderID != nil {
				e.releaseSurvivorOfFinishedPartner(*o.SiblingOrderID)
			}
		}
		return
	}

	if newStatus != protocol.StatusStaged {
		return
	}

	e.pendingSiblingReleaseMu.Lock()
	disp, ok := e.pendingSiblingRelease[changed.OrderID]
	if ok {
		delete(e.pendingSiblingRelease, changed.OrderID)
	}
	e.pendingSiblingReleaseMu.Unlock()
	if !ok {
		// No entry — either this Edge restarted since the click that deferred
		// this leg, or the leg re-staged at a later wait and consumed its entry
		// at an earlier one. Ask the durable question instead of the map's.
		e.releaseSurvivorOfFinishedPartner(changed.OrderID)
		return
	}

	// The leg is at staged now, so it is releasable at Core; releaseIfReleasable
	// is still the gate (defends against a race that moved it again) and reports
	// whether the release actually queued.
	released, err := e.releaseIfReleasable(changed.OrderID, "sibling-release-refire", disp)
	if err != nil {
		e.logFn("sibling-release-refire: order %d reached staged but release failed: %v", changed.OrderID, err)
		return
	}
	if released {
		e.logFn("sibling-release-refire: order %d reached staged — released (sibling already released on the operator's click)", changed.OrderID)
	} else {
		e.logFn("sibling-release-refire: order %d reached staged but was not releasable — dropped", changed.OrderID)
	}
}

// releaseSurvivorOfFinishedPartner is the DURABLE half of the pair-release
// deferral: it asks the database the question the in-memory map would have
// answered, for a swap leg whose partner has finished.
//
// TWO CALLERS, ONE PER ORDER OF EVENTS: the survivor staging with no map entry,
// and the partner reaching a successful terminal. It re-reads and re-decides
// everything for itself, so neither caller needs to know anything.
//
// ── WHAT IT IS FOR ────────────────────────────────────────────────────────
//
// Nothing anywhere fires when a swap peer terminalizes SUCCESSFULLY. Every arm
// that watches a peer go terminal watches for a DEATH — Core's
// HandleSwapPeerTerminal unwinds the survivor, and swapTerminalKind returns ""
// for confirmed so even the dispatch-path re-run skips it. That asymmetry is
// right as far as it goes: the unwind exists to clean up after a death, and a
// completed peer did its half. The survivor of a SUCCESSFUL half-swap needs no
// unwind. It needs a release, and it had no releaser.
//
// MEASURED, run 12d (2026-08-31). Order 84's partner 85 confirmed at 23:55:20.
// 84 was released past its first wait one second later and re-staged at its
// SECOND wait at 23:56:19 — by which time the pending-release map entry had
// already been consumed at the first wait, so handleSiblingReleaseRefire found
// nothing and returned. 84 held AMR-15 for the rest of the run. The same shape
// arrives a second way, which is why this is durable rather than a second map:
// pendingSiblingRelease is in-memory, so an Edge restart between the click and
// the leg's staging loses the deferral entirely.
//
// ── WHAT IT IS NOT ────────────────────────────────────────────────────────
//
// Not a sweep, not a timer, not a reaper. It runs on one status transition, does
// at most two reads, and completes a release the operator ALREADY asked for when
// they clicked RELEASE on a pair whose other half then went and finished. It
// never cancels and never re-plans. A leg with no sibling, or one whose sibling
// died rather than finished, is left exactly as it is today.
//
// The ONE-SHOT bound is not tidiness — see Engine.survivorReleased for the
// refusal flap it prevents and the discriminator the Edge is not sent.
func (e *Engine) releaseSurvivorOfFinishedPartner(orderID int64) {
	order, err := e.db.GetOrder(orderID)
	if err != nil || order.SiblingOrderID == nil {
		return // not a paired leg; nothing to re-derive
	}
	sibling, err := e.db.GetOrder(*order.SiblingOrderID)
	if err != nil || !orders.IsTerminalSuccess(sibling.Status) {
		return
	}

	// One shot per order per Edge lifetime — SPENT ON A RELEASE, NOT ON A TRY.
	//
	// This marked the order before attempting, and that was wrong in a way the
	// fixture found within the hour. Order 123 (partner 122 confirmed): the arm
	// fired while 123 was mid-flap and releaseIfReleasable answered "not
	// releasable at Core", so no envelope was ever sent — and the shot was gone.
	// 123 then settled into `in_transit`, which IS releasable, and nothing could
	// ask again. It stood 26 minutes holding AMR-13. Forty-four of the run's
	// eighty-nine survivor log lines were that same wasted shot.
	//
	// The bound exists to stop a REFUSAL FLAP: Core refuses a lane wait, the Edge
	// rolls the leg back to staged, and that rollback is another staged
	// transition. In that path the release genuinely fires — the order is staged,
	// so an envelope goes out — so counting envelopes still bounds the flap at
	// exactly one. What it stops counting is an attempt that never reached Core
	// and therefore cannot have flapped anything.
	e.survivorReleasedMu.Lock()
	if _, already := e.survivorReleased[orderID]; already {
		e.survivorReleasedMu.Unlock()
		return
	}
	e.survivorReleasedMu.Unlock()

	// The zero disposition, deliberately: this is the SUPPLY-leg disposition
	// (ReleaseStagedOrders gives the placing leg an empty one to preserve its
	// bin's manifest). A survivor whose partner has already completed is holding
	// material somebody still expects to arrive intact, and inventing a
	// disposition here would apply a UOP decision no operator made.
	released, err := e.releaseIfReleasable(orderID, "swap-survivor", ReleaseDisposition{CalledBy: "swap-survivor-release"})
	if err != nil {
		e.logFn("swap-survivor release: order %d staged with partner %d already completed, but release failed: %v",
			orderID, sibling.ID, err)
		return
	}
	if released {
		e.survivorReleasedMu.Lock()
		if e.survivorReleased == nil {
			e.survivorReleased = make(map[int64]struct{})
		}
		e.survivorReleased[orderID] = struct{}{}
		e.survivorReleasedMu.Unlock()
		e.logFn("swap-survivor release: order %d released — its partner %d completed at %s and nothing was left to wait for",
			orderID, sibling.ID, sibling.Status)
		return
	}
	e.logFn("swap-survivor release: order %d staged with partner %d already completed, but it was not releasable at Core",
		orderID, sibling.ID)
}
