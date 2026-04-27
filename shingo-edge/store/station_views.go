package store

import (
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

// ComputeSwapReady returns true when a two-robot swap can be released via the
// consolidated single-click path. Both tracked orders must exist; at least one
// must be in "staged" status; the other must be in a pre-staged active status
// (dispatched or in_transit) — meaning it's en route and will reach staged
// soon. The companion auto-release-on-staged hook in wiring then picks up the
// second order when it arrives, so the operator's single click covers both.
//
// Pre-2026-04-25 semantic: required BOTH orders simultaneously staged. In
// practice the two robots arrive at their wait points seconds apart, so the
// simultaneous-staged window often did not exist when the operator looked.
// Operators fell back to the admin orders page (which has its own bug — see
// kanbans.js:32 fix), losing the coordinated B-then-A ordering and the proper
// disposition handling.
//
// Non-two-robot claims always return false — their single staged order is
// still released via the per-order /api/orders/{id}/release endpoint.
func ComputeSwapReady(db *DB, claim *processes.NodeClaim, runtime *processes.RuntimeState) bool {
	if claim == nil || claim.SwapMode != "two_robot" {
		return false
	}
	if runtime == nil || runtime.ActiveOrderID == nil || runtime.StagedOrderID == nil {
		return false
	}
	active, err := db.GetOrder(*runtime.ActiveOrderID)
	if err != nil || active == nil {
		return false
	}
	staged, err := db.GetOrder(*runtime.StagedOrderID)
	if err != nil || staged == nil {
		return false
	}
	// At least one staged + the other in a pre-staged active status. Both
	// orders must be in non-terminal statuses — if either is confirmed/failed/
	// cancelled, the swap is over and the consolidated release shouldn't fire.
	atLeastOneStaged := active.Status == "staged" || staged.Status == "staged"
	bothNonTerminal := isNonTerminalForSwap(active.Status) && isNonTerminalForSwap(staged.Status)
	return atLeastOneStaged && bothNonTerminal
}

// isNonTerminalForSwap reports whether an order status indicates the order is
// still part of an active two-robot swap — i.e., not yet completed, failed, or
// cancelled. Statuses "dispatched", "in_transit", "staged", "delivered" all
// count as still-active for the swap-readiness check.
func isNonTerminalForSwap(status string) bool {
	switch status {
	case "confirmed", "failed", "cancelled":
		return false
	}
	return true
}

