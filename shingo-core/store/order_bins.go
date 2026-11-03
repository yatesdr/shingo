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

// ReplaceOrderBins makes an order's carts say exactly what the current
// allocation claimed, and points orders.bin_id at primary, in one statement —
// the set is replaced, not merged into. See the orders-package doc for the
// stale-row failure this closes.
func (db *DB) ReplaceOrderBins(orderID, primary int64, rows []orders.OrderBinRow) error {
	return orders.ReplaceOrderBins(db.DB, orderID, primary, rows)
}

// ListOrderBins returns an order's rows in plan order.
func (db *DB) ListOrderBins(orderID int64) ([]*orders.OrderBin, error) {
	return orders.ListOrderBins(db.DB, orderID)
}

// ShiftOrderBinSteps moves an order's rows with its plan after a transform
// inserted steps into it.
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
func (db *DB) PlaceBinTx(tx *sql.Tx, p BinPlacement) (Placed, error) {
	return helpers.PlaceBinTx(tx, p)
}

// Placed and OnPlaced re-export the placement's result and the landing hook
// (helpers.Placed, helpers.OnPlaced) for the service layer, which owns the
// landing verb (service.BinService.announceLanding) and cannot import
// store/internal.
type (
	Placed   = helpers.Placed
	OnPlaced = helpers.OnPlaced
)

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
// it was settled by running the caller
// (engine.TestMultiBinSettle_CompoundChildScopesItsUnclaim), not by argument.
// refuseArrival IS a complete guard for an ORDINARY order, but its first line waves compound children through the
// ownership question entirely, so a dig leg reached this writer holding a
// stranger's claim.
//
// placedByOrder is the order whose placement this is — every instruction in one
// call belongs to it, which is why it is a parameter rather than a field on
// BinArrivalInstruction.
//
// onPlaced is the landing verb (Lane H, H.2), called once per bin in this
// transaction, so every bin a settle sets down at a seat is announced, not
// only the one order.delivered named (DESIGN-H §3). Nil announces nothing.
func (db *DB) ApplyMultiBinArrival(placedByOrder int64, instructions []orders.BinArrivalInstruction, onPlaced OnPlaced) ([]int64, error) {
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
		placed, err := helpers.PlaceBinTx(tx, helpers.BinPlacement{
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
		if onPlaced != nil {
			if err := onPlaced(tx, placed); err != nil {
				return nil, err
			}
		}
		evictedGhosts = append(evictedGhosts, placed.Evicted...)
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

// SetOpenLiftDrop gives the order's open lift its set-down at plan step drop
// (orders.SetOpenLiftDrop): a dwelling dig leg's tail, bound at its release.
func (db *DB) SetOpenLiftDrop(orderID int64, drop int) error {
	return orders.SetOpenLiftDrop(db.DB, orderID, drop)
}

// RecordAppendedLift writes the row of a lift the order's plan grew after its
// rows were written (orders.RecordAppendedLift).
func (db *DB) RecordAppendedLift(o *orders.Order, binID int64, pick int) error {
	return orders.RecordAppendedLift(db.DB, o, binID, pick)
}

// StepNodeSQL is where plan step i of order o happens, in SQL
// (helpers.StepNodeSQL, the twin of domain.StepNode): for readers outside the
// store that read an order's rows as nodes.
func StepNodeSQL(o, i string) string { return helpers.StepNodeSQL(o, i) }
