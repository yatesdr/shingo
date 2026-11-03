package orders

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/protocol"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
	"shingocore/store/reservations"
)

// OrderBin is the order-bin junction domain type. The struct lives in
// shingocore/domain (Stage 2A); this alias keeps the orders.OrderBin
// name used by ListOrderBins and the outer store/
// order_bins.go re-export (store.OrderBin). BinArrivalInstruction
// stays local — it is a persistence-layer intent (a batched staging
// update passed into ApplyMultiBinArrival), not a domain entity.
type OrderBin = domain.OrderBin

// BinArrivalInstruction describes how to move and unclaim a single bin atomically.
// The caller computes staging/expiry per destination node; the store executes all
// instructions in one transaction. Consumed by the outer store/ composition
// method ApplyMultiBinArrival, which needs to mutate both the orders/order_bins
// tables and the bins aggregate in a single transaction.
type BinArrivalInstruction struct {
	BinID     int64
	ToNodeID  int64
	Staged    bool
	ExpiresAt *time.Time
}

// OrderBinRow is one lift an allocation records: the cart, the plan step that
// lifts it and the plan step that sets it down (OpenLift for none yet).
type OrderBinRow struct {
	BinID    int64
	PickStep int
	DropStep int
}

// ReplaceOrderBins makes an order's carts say exactly what THIS allocation
// claimed, in ONE statement: the owner test, the rows this allocation no
// longer lifts deleted, every lift written (one per row, upserted on its pick
// step).
//
// ── THE SET IS REPLACED, NEVER MERGED ─────────────────────────────────────
//
// A retry that claims a DIFFERENT cart must not leave the old cart's row
// behind: the settle refuses any cart the order does not hold, and the refusal
// fails the order loud (cargo_ledger_mismatch). Measured in the sim 2027-11-09:
// order 45 refused ~40 times at Lane_01, re-allocated each time, dispatched
// holding bin 7, and failed on the junction row for bin 24 that an early
// attempt had left. The set is a fact about the CURRENT allocation, so writing
// it replaces it. The fleet-demote door keeps the rows (§8): its re-dispatch
// does not re-allocate.
//
// THE OWNER TEST IS IN THE STATEMENT, ahead of every write: a terminalize holds
// the order row, so a write that took the rows first and then waited on the
// order would be a cycle (reservations.OwnerLiveSQL locks the order). An ended
// owner writes nothing and is ErrOwnerEnded.
//
// An error is returned, never logged and dropped: an allocation whose carts
// were not recorded does not go on to the fleet.
func ReplaceOrderBins(db helpers.QueryRower, orderID int64, rows []OrderBinRow) error {
	bins, picks, drops := cartArrays(rows)
	var live bool
	if err := db.QueryRow(`WITH live AS (SELECT `+reservations.OwnerLiveSQL(1)+` AS ok),
		del AS (DELETE FROM order_bins WHERE order_id = $1 AND (SELECT ok FROM live)
		         AND (pick_step IS NULL OR pick_step <> ALL($3::bigint[])) RETURNING 1),
		ins AS (INSERT INTO order_bins (order_id, bin_id, pick_step, drop_step)
		         SELECT $1, r.bin, r.pick, NULLIF(r.drop, -1)
		           FROM unnest($2::bigint[], $3::bigint[], $4::bigint[]) AS r(bin, pick, drop)
		          WHERE (SELECT ok FROM live)
		         ON CONFLICT (order_id, pick_step) DO UPDATE
		            SET bin_id = EXCLUDED.bin_id, drop_step = EXCLUDED.drop_step
		         RETURNING 1)
		SELECT (SELECT ok FROM live), (SELECT count(*) FROM del) + (SELECT count(*) FROM ins)`,
		orderID, bins, picks, drops).Scan(&live, new(int)); err != nil {
		return fmt.Errorf("record the carts of order %d: %w", orderID, err)
	}
	if !live {
		return reservations.ErrOwnerEnded
	}
	return nil
}

// cartArrays is rows as the column arrays a single INSERT … SELECT FROM
// unnest(…) writes.
func cartArrays(rows []OrderBinRow) (bins, picks, drops []int64) {
	bins = make([]int64, len(rows))
	picks = make([]int64, len(rows))
	drops = make([]int64, len(rows))
	for i, r := range rows {
		bins[i], picks[i], drops[i] = r.BinID, int64(r.PickStep), int64(r.DropStep)
	}
	return bins, picks, drops
}

// CartRows is the rows a cart-carrying order writes for the carts it holds:
// the plan walk (domain.Lifts) over the order's plan (RowsOf).
func CartRows(o *Order, claimedAt map[int]int64) []OrderBinRow {
	return RowsOf(domain.Lifts(domain.OrderPlan(o), claimedAt))
}

// RowsOf is lifts as the rows the order writes.
func RowsOf(lifts []domain.Cart) []OrderBinRow {
	out := make([]OrderBinRow, 0, len(lifts))
	for _, c := range lifts {
		if c.PickStep == nil {
			continue // a carried-over row (v173) is never written again
		}
		drop := OpenLift
		if c.DropStep != nil {
			drop = *c.DropStep
		}
		out = append(out, OrderBinRow{BinID: c.BinID, PickStep: *c.PickStep, DropStep: drop})
	}
	return out
}

// OpenLift is OrderBinRow.DropStep for a lift whose set-down the plan has not
// placed yet; it is written as NULL.
const OpenLift = -1

// SetOpenLiftDrop gives the order's open lift its set-down: the plan step drop
// a writer has just appended (a dwelling dig leg's tail). The plan writer owns
// the row's steps, so it sets this in the write that places the drop. Zero rows
// is an order with no open lift picked before drop, and not an error.
func SetOpenLiftDrop(db *sql.DB, orderID int64, drop int) error {
	if _, err := db.Exec(`UPDATE order_bins SET drop_step = $2
		WHERE order_id = $1 AND drop_step IS NULL AND pick_step IS NOT NULL AND pick_step < $2`,
		orderID, drop); err != nil {
		return fmt.Errorf("set the open lift's drop of order %d to step %d: %w", orderID, drop, err)
	}
	return nil
}

// ListOrderBins returns an order's rows in plan order (a carried-over row, with
// no steps, first): -1 for a step a row does not know.
func ListOrderBins(db *sql.DB, orderID int64) ([]*OrderBin, error) {
	rows, err := db.Query(`SELECT id, order_id, bin_id, COALESCE(pick_step, -1), COALESCE(drop_step, -1), created_at
		FROM order_bins WHERE order_id = $1 ORDER BY pick_step NULLS FIRST, id`, orderID)
	if err != nil {
		return nil, fmt.Errorf("list order_bins: %w", err)
	}
	defer rows.Close()

	var result []*OrderBin
	for rows.Next() {
		ob := &OrderBin{}
		if err := rows.Scan(&ob.ID, &ob.OrderID, &ob.BinID, &ob.PickStep, &ob.DropStep, &ob.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan order_bin: %w", err)
		}
		result = append(result, ob)
	}
	return result, rows.Err()
}

// ShiftOrderBinSteps moves an order's rows with its plan: shift[old] = new is
// applied to the pick step and the drop step of every row. A step with no entry
// stays where it is.
//
// The steps are POSITIONS IN THE ORDER'S PLAN, so a transform that inserts steps
// moves every row after the insertion; the transform owns the repair and this is
// the write it makes. A shift moves positions UPWARD into positions other rows
// still hold, and (order_id, pick_step) and (order_id, drop_step) are unique, so
// the rows the shift touches are first parked far above any real position (one
// offset on both steps keeps pick < drop), then landed: two statements, in one
// transaction, for any number of rows. A cart in hand from the start (pick step
// domain.InHand, -1) parks one below the offset and lands with the rest.
func ShiftOrderBinSteps(db *sql.DB, orderID int64, shift map[int]int) error {
	if len(shift) == 0 {
		return nil
	}
	const parked = 1_000_000
	olds := make([]int64, 0, len(shift))
	news := make([]int64, 0, len(shift))
	for o, n := range shift {
		olds, news = append(olds, int64(o)), append(news, int64(n))
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("shift order_bins: begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE order_bins SET pick_step = pick_step + $3, drop_step = drop_step + $3
		 WHERE order_id = $1 AND (pick_step = ANY($2::bigint[]) OR drop_step = ANY($2::bigint[]))`,
		orderID, olds, parked); err != nil {
		return fmt.Errorf("shift order_bins: park order %d's rows: %w", orderID, err)
	}
	if _, err := tx.Exec(`UPDATE order_bins SET
		   pick_step = COALESCE((SELECT m.n FROM unnest($2::bigint[], $3::bigint[]) AS m(o, n) WHERE m.o = pick_step - $4), pick_step - $4),
		   drop_step = COALESCE((SELECT m.n FROM unnest($2::bigint[], $3::bigint[]) AS m(o, n) WHERE m.o = drop_step - $4), drop_step - $4)
		 WHERE order_id = $1 AND pick_step >= $4 - 1`,
		orderID, olds, news, parked); err != nil {
		return fmt.Errorf("shift order_bins: land order %d's rows: %w", orderID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("shift order_bins: commit: %w", err)
	}
	return nil
}

// RecordAppendedLift writes the row of a lift the order's plan grew after its
// rows were written (an appended pickup): binID lifted at plan step pick, set
// down at the plan's next dropoff after it, open when there is none. The lift
// that finds it writes it, in this one statement; a row already at that pick
// stands.
func RecordAppendedLift(db *sql.DB, o *Order, binID int64, pick int) error {
	steps := domain.OrderPlan(o)
	drop := OpenLift
	for i := pick + 1; i < len(steps); i++ {
		if steps[i].Action == protocol.ActionDropoff {
			drop = i
			break
		}
	}
	if _, err := db.Exec(`INSERT INTO order_bins (order_id, bin_id, pick_step, drop_step)
		VALUES ($1, $2, $3, NULLIF($4::int, -1))
		ON CONFLICT (order_id, pick_step) DO NOTHING`,
		o.ID, binID, pick, drop); err != nil {
		return fmt.Errorf("record order %d's appended lift of bin %d at step %d: %w", o.ID, binID, pick, err)
	}
	return nil
}
