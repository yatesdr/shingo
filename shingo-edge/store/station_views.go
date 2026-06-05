package store

import (
	"fmt"

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
// Pre-staged-status semantic preserved: Order A's status is irrelevant.
// ReleaseStagedOrders fans out to both legs unconditionally regardless
// of where each leg is in its choreography. See the 2026-04-27
// retrospective for why the symmetric "both at staged" check was
// replaced with this single-check shape.
//
// Non-two-robot claims always return false — their single staged order
// is released via the per-order /api/orders/{id}/release endpoint.
func ComputeSwapReady(db *DB, claim *processes.NodeClaim, runtime *processes.RuntimeState, task *processes.NodeTask) bool {
	if claim == nil || !claim.SwapMode.IsTwoRobot() {
		return false
	}
	evacOrderID := resolveEvacOrderID(db, runtime, task)
	if evacOrderID == nil {
		return false
	}
	evac, err := db.GetOrder(*evacOrderID)
	if err != nil || evac == nil {
		return false
	}
	// Order graph check: a sibling pointer proves this is a coordinated
	// pair (set at order-creation time by every site that creates one —
	// changeover_applier.go, operator_stations.go, operator_bin_ops.go,
	// operator_produce.go, wiring_status_changed.go). Single-leg flows
	// (drops, manual single, etc.) have no sibling and short-circuit.
	if evac.SiblingOrderID == nil {
		return false
	}
	// Lineside leg parked at staged → release-ready.
	return evac.Status == "staged"
}

// resolveEvacOrderID locates the evac (lineside) order's ID via three
// fallbacks in order of canonicality:
//
//  1. runtime.StagedOrderID — the canonical evac slot.
//  2. runtime.ActiveOrderID's sibling pointer — when StagedOrderID got
//     nulled but ActiveOrderID survived. Mirrors ResolveSwapPair's
//     fallback for the missing supply half.
//  3. task.OldMaterialReleaseOrderID — when BOTH runtime pointers are
//     nil. The planner stamps this pointer at order-creation time and
//     runtime mutations don't clear it, so it survives
//     handler_bin_picked_up and other clears that strip the runtime
//     pointers. Plant 2026-05-11 (SNF2 ALN_001): both runtime pointers
//     were nil at release time despite Core showing the evac at staged.
//
// The caller checks SiblingOrderID on the resolved order — if the task
// pointer happens to identify a single-leg drop, the sibling check
// returns false naturally. No Situation guard needed here.
func resolveEvacOrderID(db *DB, runtime *processes.RuntimeState, task *processes.NodeTask) *int64 {
	if runtime != nil {
		if runtime.StagedOrderID != nil {
			return runtime.StagedOrderID
		}
		if runtime.ActiveOrderID != nil {
			supply, err := db.GetOrder(*runtime.ActiveOrderID)
			if err == nil && supply != nil && supply.SiblingOrderID != nil {
				return supply.SiblingOrderID
			}
		}
	}
	if task != nil && task.OldMaterialReleaseOrderID != nil {
		return task.OldMaterialReleaseOrderID
	}
	return nil
}

// ResolveSwapPair returns the (evac, supply) order IDs for a two-robot
// swap, walking the same three-fallback ladder as resolveEvacOrderID
// (StagedOrderID → ActiveOrderID's sibling → task.OldMaterialReleaseOrderID)
// and then resolving the supply half via the durable sibling pointer.
//
// This is the canonical resolver used by both the HMI render-side
// (ComputeSwapReady, indirectly via resolveEvacOrderID) and the
// engine release path (engine.ReleaseStagedOrders). Pre-2026-05-12 the
// two sides used different resolvers — the HMI had the task fallback,
// the engine didn't — so a node with both runtime pointers nil but a
// good task pointer would render RELEASE in the HMI and then bounce
// with "no tracked orders to release" when clicked. Plant SNF2 ALN_001
// hit this repeatedly during the 2026-05-11 swap cycle.
//
// Returns an error when no evac can be resolved or when the resolved
// pair is single-leg (one half has no sibling). Single-leg flows
// (drops, manual single, sequential) must use per-order release.
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
	}
	return evacID, supplyID, nil
}
