package audit

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// CarrierRow is one bin_uop_ledger row as the confirm's FIFO-date rule reads it
// (service.keepFIFODate): what happened to the carrier, its count before, its
// part, the node it stood on and when. LoadedAt is the date a
// manifest_confirmed row stamped (its detail), nil for every other row and for
// a confirm written before the detail carried it.
type CarrierRow struct {
	Op        string
	BeforeUOP *int
	Payload   string
	NodeID    *int64
	At        time.Time
	LoadedAt  *time.Time
}

// partsOnlyOps are the rows about parts alone: an applied or refused count, a
// person's count, a tote count. They say nothing about what the carrier holds
// or where it stands, so the FIFO-date rule reads past them; every other op
// ends the sequence it looks for.
var partsOnlyOps = []string{OpBinUOPDelta, OpCycleCount, OpCycleCountOverCapacity,
	OpPayloadMismatchDropped, OpStaleEpochDropped, OpTotesDeclared, OpTotesEdited}

// RecentCarrierRows returns a bin's newest ledger rows past the parts-only ones,
// newest first, at most limit of them, on the caller's transaction.
//
// NOTHING IN THE STATEMENT CAN FAIL ON THE DATA. The confirm runs this inside
// its own transaction, and a failed statement aborts a Postgres transaction:
// so the confirm row's date comes back as text and is parsed here, where an
// unreadable value is only an absent one.
func RecentCarrierRows(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}, binID int64, limit int) ([]CarrierRow, error) {
	quoted := make([]string, len(partsOnlyOps))
	for i, op := range partsOnlyOps {
		quoted[i] = "'" + op + "'"
	}
	rows, err := q.Query(`SELECT op, before_uop, payload_code, node_id, applied_at,
			COALESCE(CASE WHEN op = '`+OpManifestConfirmed+`' THEN detail->>'loaded_at' END, '')
		FROM bin_uop_ledger
		WHERE bin_id = $1 AND op NOT IN (`+strings.Join(quoted, ", ")+`)
		ORDER BY id DESC
		LIMIT $2`, binID, limit)
	if err != nil {
		return nil, fmt.Errorf("recent carrier rows for bin %d: %w", binID, err)
	}
	defer rows.Close()
	var out []CarrierRow
	for rows.Next() {
		var (
			r       CarrierRow
			before  sql.NullInt64
			node    sql.NullInt64
			stamped string
		)
		if err := rows.Scan(&r.Op, &before, &r.Payload, &node, &r.At, &stamped); err != nil {
			return nil, fmt.Errorf("scan carrier row for bin %d: %w", binID, err)
		}
		if before.Valid {
			v := int(before.Int64)
			r.BeforeUOP = &v
		}
		if node.Valid {
			v := node.Int64
			r.NodeID = &v
		}
		if t, perr := time.Parse(time.RFC3339Nano, stamped); stamped != "" && perr == nil {
			r.LoadedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
