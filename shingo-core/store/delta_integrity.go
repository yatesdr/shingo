// delta_integrity.go — the delta-drop panel's read.
//
// AT THE OUTER store/ LEVEL, not in store/bins, and that placement is the
// depguard rule working as intended: this query spans two aggregates. It reads
// bin_uop_audit (the audit aggregate — the ops, and the dropped quantity in
// metadata) and bins (the ledger total each row is set against), and it names
// store/audit's op constants rather than re-spelling them as string literals
// that would drift the first time one is renamed. Cross-aggregate orchestration
// belongs here, for the same reason ListOrdersByBin does.

package store

import (
	"database/sql"
	"fmt"
	"time"

	"shingocore/domain"
	"shingocore/store/audit"
)

// deltaIntegrityByPayload reports, per payload over a window, how much count
// was DROPPED and how many bins were rebound onto mixed contents — set beside
// that payload's current plant-wide ledger total.
//
// WHY THIS IS READ-ONLY. Every drop is already recorded. The applier writes an
// observation row into bin_uop_audit (before_uop == after_uop, because the
// count did not move) with the dropped quantity in the metadata JSONB, and
// flags the bin via anomaly_at. So the whole panel is a query; there is nothing
// new to write.
//
// WHY THE COMPARISON IS THE POINT. The ledger-exceptions panel beside this one
// says "this payload is negative". This says "and here is the mechanism that
// probably did it". If a payload reads -443 and shows ~443 UOP of dropped
// credits over the same window, the leading hypothesis for the negative counts
// is confirmed on sight instead of argued.
//
// WHY THE REBIND IS NOT SUMMED IN. payload_rebound_with_inventory is NOT a
// drop. The applier rebinds the payload and then APPLIES the delta — its own
// comment says "counting CONTINUES (the tote's unit total stays correct), and
// the bin is anomaly-flagged for a later cycle count of the mixed contents".
// It appears in the discrepancy ledger because it is worth seeing, not because
// units were lost. Summing it into UOP lost inflates the figure and corrupts
// the -443-vs-~443 comparison that is the entire point. It is reported as a
// count with no UOP total.
//
// SIGN CONVENTION. uop_lost is the NET effect on the ledger: dropped credits
// (positive deltas that never landed) minus dropped consumes (negative deltas
// that never landed). Positive means the count reads BELOW reality by that
// much, which is the shape that produces a negative in-loop total — so the
// number can be read directly against the ledger total beside it.
//
// Payloads with no drops in the window are omitted. Blank on a good day, like
// its neighbour.
func deltaIntegrityByPayload(db *sql.DB, since time.Time) ([]domain.DeltaIntegrity, error) {
	// The delta lives in metadata as a JSON number; all three ops write it
	// under the same key. COALESCE guards a row whose metadata failed to
	// marshal — the applier logs and carries on in that case, so the row can
	// exist without one.
	rows, err := db.Query(`
		WITH drops AS (
		  SELECT payload_code,
		         bin_id,
		         op,
		         applied_at,
		         COALESCE((metadata->>'delta')::INTEGER, 0) AS delta
		    FROM bin_uop_audit
		   WHERE op IN ($1, $2, $3)
		     AND applied_at >= $4
		     AND payload_code <> ''
		)
		SELECT payload_code,
		       COALESCE(SUM(delta)      FILTER (WHERE op <> $3 AND delta > 0), 0)::INTEGER AS credits_dropped,
		       COALESCE(SUM(-delta)     FILTER (WHERE op <> $3 AND delta < 0), 0)::INTEGER AS consumes_dropped,
		       COUNT(*)                 FILTER (WHERE op <> $3)::INTEGER                   AS drop_rows,
		       COUNT(*)                 FILTER (WHERE op = $1)::INTEGER                    AS stale_epoch_rows,
		       COUNT(*)                 FILTER (WHERE op = $2)::INTEGER                    AS payload_mismatch_rows,
		       COUNT(*)                 FILTER (WHERE op = $3)::INTEGER                    AS mixed_contents,
		       COUNT(DISTINCT bin_id)   FILTER (WHERE op <> $3)::INTEGER                   AS bins,
		       MIN(applied_at),
		       MAX(applied_at)
		  FROM drops
		 GROUP BY payload_code
		 ORDER BY payload_code`,
		audit.OpStaleEpochDropped, audit.OpPayloadMismatchDropped, audit.OpPayloadReboundWithInventory,
		since.UTC())
	if err != nil {
		return nil, fmt.Errorf("delta integrity by payload: %w", err)
	}
	defer rows.Close()

	var out []domain.DeltaIntegrity
	for rows.Next() {
		var d domain.DeltaIntegrity
		var first, last sql.NullTime
		if err := rows.Scan(&d.PayloadCode,
			&d.CreditsDropped, &d.ConsumesDropped,
			&d.DropRows, &d.StaleEpochRows, &d.PayloadMismatchRows,
			&d.MixedContents, &d.Bins, &first, &last); err != nil {
			return nil, fmt.Errorf("scan delta integrity: %w", err)
		}
		d.UOPLost = d.CreditsDropped - d.ConsumesDropped
		if first.Valid {
			t := first.Time
			d.FirstAt = &t
		}
		if last.Valid {
			t := last.Time
			d.LastAt = &t
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The ledger total each row is meant to be read against. One grouped read
	// over the same payloads rather than one query per row.
	totals, err := payloadLedgerTotals(db)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].LedgerTotal = totals[out[i].PayloadCode]
	}
	return out, nil
}

// payloadLedgerTotals is every payload's plant-wide in-loop bin total,
// including the non-negative ones — deltaIntegrityByPayload needs the total
// whatever its sign, because "drops happened and the ledger is FINE" is also
// worth being able to see.
func payloadLedgerTotals(db *sql.DB) (map[string]int, error) {
	rows, err := db.Query(`
		SELECT payload_code, SUM(uop_remaining)::INTEGER
		FROM bins
		WHERE payload_code <> ''
		GROUP BY payload_code`)
	if err != nil {
		return nil, fmt.Errorf("payload ledger totals: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var code string
		var total int
		if err := rows.Scan(&code, &total); err != nil {
			return nil, fmt.Errorf("scan payload ledger total: %w", err)
		}
		out[code] = total
	}
	return out, rows.Err()
}

// DeltaIntegrityByPayload reports dropped deltas per payload since `since`,
// set beside that payload's current ledger total — the mechanism panel that
// sits next to the ledger-exception list.
func (db *DB) DeltaIntegrityByPayload(since time.Time) ([]domain.DeltaIntegrity, error) {
	return deltaIntegrityByPayload(db.DB, since)
}
