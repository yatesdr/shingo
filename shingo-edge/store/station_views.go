package store

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// NodeBinState, StationNodeView, and OperatorStationView are the
// HMI-facing view types rendered by the operator-station page. The
// structs live in shingoedge/domain (Stage 2A.2) so www handlers
// can build response shapes without importing this persistence
// package; these aliases keep the existing store.X names that
// service code and the operator-station handlers reference.
type (
	NodeBinState        = domain.NodeBinState
	StationNodeView     = domain.StationNodeView
	OperatorStationView = domain.OperatorStationView
)

// BuildOperatorStationView body lives in
// shingoedge/service/station_service.go::StationService.BuildView
// (Phase 6.4a). Helpers ComputeSwapReady and LookupLastReleaseError
// stay here so the existing station_views_test.go tests of the swap-
// ready logic don't need to move; the service body invokes them.

// releaseErrorPrefix is the leading substring written by
// orders.Manager.RollbackForRetry into the order_history detail when a
// manifest_sync_failed rollback occurs. The operator UI keys off this
// prefix to render the release-error chip.
const releaseErrorPrefix = "Manifest sync failed at Core"

// LookupLastReleaseError returns the rollback detail for the runtime's
// tracked orders if either of them has a recent manifest_sync_failed
// rollback in its history. Returns the most recent matching detail, or
// empty string if no error is pending.
//
// We check both ActiveOrderID and StagedOrderID because the rollback can
// land on either depending on which order was being released. The history
// query is cheap (indexed on order_id) and best-effort — any failure to
// read history just leaves the chip absent rather than blocking the view.
func LookupLastReleaseError(db *DB, runtime *processes.RuntimeState) string {
	if runtime == nil {
		return ""
	}
	var detail string
	for _, oid := range []*int64{runtime.ActiveOrderID, runtime.StagedOrderID} {
		if oid == nil {
			continue
		}
		hist, err := db.ListOrderHistory(*oid)
		if err != nil || len(hist) == 0 {
			continue
		}
		// Most recent first. ListOrderHistory returns oldest-first, so walk
		// from the end.
		for i := len(hist) - 1; i >= 0; i-- {
			d := hist[i].Detail
			if d == "" {
				continue
			}
			if len(d) >= len(releaseErrorPrefix) && d[:len(releaseErrorPrefix)] == releaseErrorPrefix {
				detail = d
				break
			}
			// Stop scanning once we hit a non-error transition — the rollback
			// is the most recent thing or it isn't pending.
			break
		}
		if detail != "" {
			return detail
		}
	}
	return ""
}

// ComputeSwapReady returns true when a two-robot swap can be released via
// the consolidated single-click path: a sibling-linked pair of orders
// exists at this node and the lineside leg is parked at staged.
//
// The predicate keys on the **order graph** — specifically the durable
// sibling pointer set at order-creation time by every site that creates
// a pair (LinkOrderSiblings). This is the structural answer to "is there
// a second robot involved here?" Single-leg flows (drops, manual single,
// sequential — though sequential is also excluded by the SwapMode gate
// for disposition-routing reasons) have no sibling pointer and naturally
// short-circuit. Pre-2026-05-11 this function keyed on claim.SwapMode
// and task.Situation, which are policy/intent signals; they're correct
// 80% of the time but disagree with the order graph during changeovers
// where a drop on a two_robot node creates only an evac order without
// a sibling. Three patches landed trying to filter for that case (the
// task-fallback drop guard in 1ed6e18, the top-of-function drop guard
// added 2026-05-11). The order-graph predicate makes those guards
// structurally unnecessary: a drop has no sibling, so it can never read
// as a coordinated pair.
//
// SwapMode outer gate is preserved because it still does real work:
//   - Excludes sequential, which has sibling-linked pairs but uses
//     per-order release semantics (different disposition routing).
//   - Excludes manual_swap, which doesn't create pairs at all.
//
// Per-mode staged gate (hop A4-iv, 2026-07-23):
//
//   - two_robot: the positionally-resolved evac (StagedOrderID slot) is the
//     gating leg and must be at staged. ReleaseStagedOrders fans this pair out
//     evac-first (Edge-ordered on a shared line node), so the button must not
//     appear while only the supply is parked — releasing the supply ahead of an
//     un-staged evac would race a fresh bin onto the line before the old one is
//     lifted. Order A's status stays irrelevant; the StagedOrderID leg is the
//     single gate — and must stay that way. See the DO NOT ADD A SUPPLY GATE
//     note at the gate itself: ReleaseStagedOrders defers and re-fires a leg
//     Core will not take yet, so gating on the supply only removes the
//     operator's ability to say "go".
//   - two_robot_press_index: the evac/supply labels above are POSITIONAL and
//     INVERTED (R1 is the evac, R2 the supply/index — see ResolveSwapPair's
//     "KNOWN WRONG" note), and the two legs are FLEET-sequenced on their shared
//     nodes rather than Edge-ordered. So the RELEASE affordance must track
//     "either leg of the pair is parked at staged", not the positional evac
//     alone: otherwise a legitimately-staged INDEX leg — the one that sourced
//     ~19 min late in the Hopkinsville hang — reads swap_ready=false and loses
//     its button. Showing the button on either-staged is safe because
//     ReleaseStagedOrders now gates each leg on ReleasableAtCore and defers the
//     rest (hop A4-i/-ii), so a leg Core would refuse is never released.
//
// Non-two-robot claims always return false — their single staged order
// is released via the per-order /api/orders/{id}/release endpoint.
func ComputeSwapReady(db *DB, claim *processes.NodeClaim, runtime *processes.RuntimeState, task *processes.NodeTask) bool {
	if claim == nil || !claim.SwapMode.IsTwoRobot() {
		return false
	}
	// ONE resolver, shared with the release path. This used to walk its own
	// ladder (resolveEvacOrderID) whose fallbacks differed from ResolveSwapPair's:
	// that one fell through to the task pointer on ANY miss, while ResolveSwapPair
	// reaches its task fallback only when BOTH runtime pointers are nil. With
	// StagedOrderID nil, ActiveOrderID set, and that order carrying no sibling,
	// the two disagreed — this function said "render RELEASE" and the click then
	// bounced on "no sibling — not a coordinated pair". That is the same
	// render-vs-click divergence the comment on ResolveSwapPair says was fixed on
	// 2026-05-12; the fix went into one ladder and this one kept its own.
	//
	// Answering from ResolveSwapPair makes render-implies-click-resolvable
	// STRUCTURAL rather than a property two functions have to keep agreeing on.
	// The explicit sibling check is gone because ResolveSwapPair already errors
	// on a single-leg pair, which is the same short-circuit.
	evacOrderID, supplyOrderID, err := ResolveSwapPair(db, runtime, task)
	if err != nil || evacOrderID == nil {
		return false
	}
	evac, gerr := db.GetOrder(*evacOrderID)
	if gerr != nil || evac == nil {
		return false
	}
	// two_robot gate: the positionally-resolved evac must be parked. The
	// supply's status is deliberately NOT consulted.
	//
	// DO NOT ADD A SUPPLY GATE HERE. The operator-station glow (isReleaseReady,
	// operator-render.js) carries one that looks like the missing half of this
	// predicate. It is not. The two front different machinery:
	//
	//   - This gates /release-staged -> ReleaseStagedOrders, which since hop
	//     A4-ii REMEMBERS a leg Core will not take yet
	//     (rememberDeferredSiblingRelease) and re-fires it when it reaches staged
	//     (wiring_status_changed.handleSiblingReleaseRefire). The operator's
	//     single click already means "go for the pair, defer the rest". Gating on
	//     the supply deletes that capability and converts "click now, machinery
	//     defers" into "wait, then click".
	//   - The glow gates the CHANGEOVER path, whose deferred supply release
	//     (HandleBinPickedUp) calls releaseIfReleasable and registers NO re-fire.
	//     There a skipped supply really is dropped, so waiting for it is right.
	//
	// The cost is concrete. On the ALN_003 2026-07-31 timeline the supply faulted
	// three times while the evac sat parked — a supply gate would have taken the
	// button away during each fault, in the very window the operator was trying
	// to use it.
	if evac.Status == "staged" {
		return true
	}
	// press-index: role labels are inverted and the legs are fleet-sequenced,
	// so accept the pair as release-ready when the SIBLING leg is parked too.
	if claim.SwapMode == protocol.SwapModeTwoRobotPressIndex && supplyOrderID != nil {
		if sib, serr := db.GetOrder(*supplyOrderID); serr == nil && sib != nil && sib.Status == "staged" {
			return true
		}
	}
	return false
}

// ResolveSwapPair returns the (evac, supply) order IDs for a two-robot
// swap, walking a three-fallback ladder
// (StagedOrderID → ActiveOrderID's sibling → task.OldMaterialReleaseOrderID)
// and then resolving the supply half via the durable sibling pointer.
//
// THE ONLY swap-pair resolver. Both the HMI render-side (ComputeSwapReady) and
// the engine release path (engine.ReleaseStagedOrders) call it, so the button
// can only render when the click's own resolution would succeed.
//
// It has taken two attempts to get there, in the same failure mode both times.
// Pre-2026-05-12 the two sides used different resolvers — the HMI had the task
// fallback, the engine didn't — so a node with both runtime pointers nil but a
// good task pointer would render RELEASE and then bounce with "no tracked
// orders to release" when clicked (plant SNF2 ALN_001, repeatedly, during the
// 2026-05-11 swap cycle). That fix gave the engine a task fallback but left the
// HMI's separate ladder in place, and the two still disagreed whenever
// StagedOrderID was nil, ActiveOrderID was set, and that order had no sibling:
// the HMI fell through to the task pointer, the engine did not (its fallback is
// gated on BOTH pointers being nil) and errored on the missing sibling. Since
// 2026-07-31 ComputeSwapReady answers from here, so there is no second ladder
// left to drift.
//
// Returns an error when no evac can be resolved or when the resolved
// pair is single-leg (one half has no sibling). Single-leg flows
// (drops, manual single, sequential) must use per-order release.
//
// ── KNOWN WRONG FOR PRESS-INDEX. Marked for the leg_role conversion. ──
//
// This maps role POSITIONALLY: staged→evac, active→supply. That mapping is a
// two_robot assumption (leg A is the supply, leg B is the evac) and it is
// INVERTED for two_robot_press_index, where R1 — the first leg — is the EVAC (it
// clears the press) and R2 is the SUPPLY (it indexes the fresh bin on).
//
// It is masked today: the live press-index claims are produce-role, and the
// produce release path returns before the evac/supply distinction is used. It is
// a live bug the moment a consume-role press-index claim exists.
//
// Deliberately NOT fixed here. This is the fourth site that infers a leg's role,
// after the Edge classifier and Core's two dispatch predicates — all three of
// which now read the leg's STEPS (legPlacesBinAt / legTakesLineBin). This one
// cannot: it resolves from runtime pointers and a node task, and never loads the
// orders' steps at all. Fixing it in place would mean a fourth independent
// re-derivation of the same fact; it is the case that earns the `leg_role` field
// on the order, which is the next phase. Until then it is wrong, contained, and
// written down.
//
// hop A4-iv (2026-07-23) did NOT un-invert this mapping — that still waits on
// leg_role. It only fixed the downstream symptom that bit an operator: the
// RELEASE button vanishing on a legitimately-staged index leg. ComputeSwapReady
// works around the inversion by accepting EITHER staged leg for press-index
// (see there), so the button survives even though this resolver still labels the
// legs positionally. The disposition/ordering the positional labels drive in
// ReleaseStagedOrders stays masked (live press-index claims are produce-role,
// whose release returns before the evac/supply split) and harmless (press-index
// is fleet-sequenced, so leg order at release doesn't gate physical safety).
func ResolveSwapPair(db *DB, runtime *processes.RuntimeState, task *processes.NodeTask) (evacID, supplyID *int64, err error) {
	if runtime != nil {
		if runtime.StagedOrderID != nil {
			id := *runtime.StagedOrderID
			evacID = &id
		}
		if runtime.ActiveOrderID != nil {
			id := *runtime.ActiveOrderID
			supplyID = &id
		}
	}
	// Task fallback when both runtime pointers are nil. The planner
	// stamps task.OldMaterialReleaseOrderID at order-creation time and
	// runtime mutations don't clear it.
	if evacID == nil && supplyID == nil && task != nil && task.OldMaterialReleaseOrderID != nil {
		id := *task.OldMaterialReleaseOrderID
		evacID = &id
	}
	if evacID == nil && supplyID == nil {
		return nil, nil, fmt.Errorf("no tracked orders to release")
	}
	// Walk the sibling pointer for the half we don't have.
	if evacID == nil {
		supply, err := db.GetOrder(*supplyID)
		if err != nil {
			return nil, nil, fmt.Errorf("get supply order %d: %w", *supplyID, err)
		}
		if supply.SiblingOrderID == nil {
			return nil, nil, fmt.Errorf("order %d has no sibling — not a coordinated pair", *supplyID)
		}
		id := *supply.SiblingOrderID
		evacID = &id
	} else if supplyID == nil {
		evac, err := db.GetOrder(*evacID)
		if err != nil {
			return nil, nil, fmt.Errorf("get evac order %d: %w", *evacID, err)
		}
		if evac.SiblingOrderID == nil {
			return nil, nil, fmt.Errorf("order %d has no sibling — not a coordinated pair (single-leg flow should use per-order release)", *evacID)
		}
		id := *evac.SiblingOrderID
		supplyID = &id
	} else {
		// BOTH pointers were populated, so neither branch above ran and no
		// sibling pointer has been read. Verify the linkage here, or this
		// function hands back "a pair" it never checked is one.
		//
		// IDENTITY, NOT JUST EXISTENCE. `evac.SiblingOrderID != nil` alone is
		// not enough: the reachable failure is a STALE ActiveOrderID pointing at
		// an unrelated live order while the evac is correctly linked to a third.
		// Existence passes that, and the callers then treat the stale order as
		// the supply half — ReleaseStagedOrders would release an order that has
		// nothing to do with this node. Requiring the evac's sibling pointer to
		// NAME the resolved supply closes it.
		//
		// Scope, precisely: this reads the evac's pointer, which is the one both
		// callers gate on. A pair whose SUPPLY-side back-pointer was cleared
		// while the evac's remains correct still passes — the linkage the
		// resolver actually used is intact, and asserting the reverse direction
		// too would cost a second GetOrder for a state no caller distinguishes.
		evac, gerr := db.GetOrder(*evacID)
		if gerr != nil {
			return nil, nil, fmt.Errorf("get evac order %d: %w", *evacID, gerr)
		}
		if evac.SiblingOrderID == nil || *evac.SiblingOrderID != *supplyID {
			return nil, nil, fmt.Errorf("order %d is not paired with %d — stale or missing sibling linkage (single-leg flow should use per-order release)", *evacID, *supplyID)
		}
	}
	return evacID, supplyID, nil
}
