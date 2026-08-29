package store

// Stage 2D delegate file: order_bins junction CRUD lives in store/orders/.
// ApplyMultiBinArrival stays here as cross-aggregate composition because it
// mutates both the bins and order_bins tables in a single transaction.

import (
	"database/sql"
	"fmt"

	"shingocore/store/internal/helpers"
	"shingocore/store/orders"
)

// InsertOrderBin records a claimed bin and its resolved destination for a
// complex order.
func (db *DB) InsertOrderBin(orderID, binID int64, stepIndex int, action, nodeName, destNode string) error {
	return orders.InsertOrderBin(db.DB, orderID, binID, stepIndex, action, nodeName, destNode)
}

// ReplaceOrderBins makes an order's junction rows say exactly what the current
// allocation claimed — the set is replaced, not merged into. See the orders-package
// doc for the stale-row failure this closes.
func (db *DB) ReplaceOrderBins(orderID int64, rows []orders.OrderBinRow) error {
	return orders.ReplaceOrderBins(db.DB, orderID, rows)
}

// ListOrderBins returns all junction rows for an order, ordered by step_index.
func (db *DB) ListOrderBins(orderID int64) ([]*orders.OrderBin, error) {
	return orders.ListOrderBins(db.DB, orderID)
}

// UpdateOrderBinDestNode re-points one bin's recorded destination, keyed by bin
// because step_index names the pickup. Returns rows changed (0 or 1); zero is the
// ordinary answer for a single-bin order, which has no junction rows.
func (db *DB) UpdateOrderBinDestNode(orderID, binID int64, destNode string) (int64, error) {
	return orders.UpdateOrderBinDestNode(db.DB, orderID, binID, destNode)
}

// DeleteOrderBins removes all junction rows for an order. Called alongside
// UnclaimOrderBins on cancel/fail paths to keep the junction table clean.
func (db *DB) DeleteOrderBins(orderID int64) { orders.DeleteOrderBins(db.DB, orderID) }

// ShiftOrderBinSteps rewrites an order's junction step_index values after a
// transform inserted steps into its plan.
func (db *DB) ShiftOrderBinSteps(orderID int64, shift map[int]int) error {
	return orders.ShiftOrderBinSteps(db.DB, orderID, shift)
}

// BinPlacement re-exports helpers.BinPlacement so the service layer can name the
// primitive's arguments. service/ cannot import store/internal.
type BinPlacement = helpers.BinPlacement

// PlaceBinTx is the *store.DB entry point to THE ONE PLACEMENT
// (helpers.PlaceBinTx), for service.BinService.applyArrival.
//
// A thin delegate on purpose: the whole value of the primitive is that the three
// writers run the SAME code, so this must add nothing and decide nothing.
func (db *DB) PlaceBinTx(tx *sql.Tx, p BinPlacement) ([]int64, error) {
	return helpers.PlaceBinTx(tx, p)
}

// ApplyMultiBinArrival moves multiple bins to their per-step destinations and
// unclaims them atomically in a single transaction. The caller provides
// pre-computed arrival instructions (destination node, staging, expiry) for
// each bin. Cross-aggregate (bins ↔ order_bins).
//
// Each instruction is one helpers.PlaceBinTx — the same placement the single-bin
// and repair writers now perform, so the three cannot drift. Returns the ids of
// any evicted ghosts so the caller can alert.
//
// THE UNCLAIM DEFECT THIS PATH CARRIED is worth keeping the note for, because it
// is what the primitive exists to make unrepeatable. The loop spelled its own
// `claimed_by=NULL` with no scope at all — the pre-445f79eb shape, fixed in
// applyArrival and never carried here. The round split on whether it mattered;
// it was settled by running the caller (engine.TestMultiBinSettle_CompoundChild-
// ScopesItsUnclaim), not by argument. refuseArrival IS a complete guard for an
// ORDINARY order, but its first line waves compound children through the
// ownership question entirely, so a dig leg reached this writer holding a
// stranger's claim.
//
// placedByOrder is the order whose placement this is — every instruction in one
// call belongs to it, which is why it is a parameter rather than a field on
// BinArrivalInstruction.
func (db *DB) ApplyMultiBinArrival(placedByOrder int64, instructions []orders.BinArrivalInstruction) ([]int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var evictedGhosts []int64
	for _, inst := range instructions {
		// ONE PLACEMENT. This loop used to spell the whole arrival itself —
		// eviction, the node_id write, the unclaim, the reservation, the slot,
		// the staging state — and every drift between it and the single-bin
		// writer lived in that duplication. See helpers.PlaceBinTx.
		ghosts, err := helpers.PlaceBinTx(tx, helpers.BinPlacement{
			BinID:                  inst.BinID,
			ToNodeID:               inst.ToNodeID,
			PlacedByOrder:          placedByOrder,
			ReleaseClaim:           true, // a settle is a handoff
			ReleaseDestinationSlot: true,
			Staged:                 inst.Staged,
			ExpiresAt:              inst.ExpiresAt,
		})
		if err != nil {
			return nil, err
		}
		evictedGhosts = append(evictedGhosts, ghosts...)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit multi-bin arrival: %w", err)
	}
	return evictedGhosts, nil
}

// OrderOwnsNoCargo answers "does this order move a bin of its own?" — the one
// question, through the one spelling (helpers.OwnsNoCargoSQL).
//
// TRUE means the order is a COORDINATOR: it owns legs, and its NULL bin_id is
// permanent and correct. FALSE means it is an ordinary order, and a NULL bin_id
// on it is a real fault worth reporting.
//
// `order.BinID == nil` cannot tell those apart, which is what made the bin-state
// strip read "Core degraded" for a whole rig run against twelve coordinators and
// zero actual defects. See helpers.OwnsNoCargoSQL for the measurement.
//
// IT COSTS A READ, and that is the honest price of asking the right question:
// coordinator-ness lives in the child rows, so it cannot be answered from the
// order row alone the way the bin-id spelling could. The callers that ask it are
// per-order decisions, not hot loops.
//
// A READ FAILURE ANSWERS FALSE, with the error returned so a caller can log it.
// False is "treat it as an ordinary order", which preserves every existing
// guard's behaviour — the fail-safe direction while this is still shadowed.
func (db *DB) OrderOwnsNoCargo(orderID int64) (bool, error) {
	var owns bool
	if err := db.QueryRow(
		`SELECT `+helpers.OwnsNoCargoSQL("o")+` FROM orders o WHERE o.id = $1`, orderID,
	).Scan(&owns); err != nil {
		return false, fmt.Errorf("order %d owns-no-cargo: %w", orderID, err)
	}
	return owns, nil
}
