package inventory

import (
	"database/sql"
	"fmt"
	"strconv"

	"shingo/protocol"
)

// RowsQuerier is what SoleBinCursor reads through: *sql.DB and *sql.Tx.
type RowsQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// SoleBinCursor returns the one station that has count messages applied for
// this carrier in this generation, and Core's cursor for it: the dedup rows of
// scope ('bin', binID, epoch), whatever the station. It is the station the
// record-count fence measures (SYNTH-round2 S7).
//
// WHY NOT THE NODE'S STATION. node_stations is many-to-many (a node lists the
// stations allowed to order against it, with "all" and inherit modes), and it
// says nothing about which station counts the carrier sitting there. The dedup
// row is written by exactly the stream the fence has to splice into, so it is
// the answer rather than a guess at it.
//
// rows reports how many stations were found, capped at 2. The caller fences
// only on exactly 1: none means no station has counted this carrier in this
// generation, and two means two stations are counting one carrier, whose two
// nets one fence cannot describe.
//
// One statement. The table's primary key leads with station, so this is a
// scan of inventory_delta_dedup rather than a key lookup. It runs once per
// count (tens a month per plant), not per message.
func SoleBinCursor(q RowsQuerier, binID, epoch int64) (station string, c DeltaCursor, rows int, err error) {
	rs, err := q.Query(`SELECT station, last_seq, applied_net, applied_window_end
		FROM inventory_delta_dedup
		WHERE scope_kind=$1 AND scope_key=$2 AND epoch=$3
		ORDER BY station LIMIT 2`,
		protocol.InvDeltaScopeBin, strconv.FormatInt(binID, 10), epoch)
	if err != nil {
		return "", DeltaCursor{}, 0, fmt.Errorf("read delta cursors bin=%d epoch=%d: %w", binID, epoch, err)
	}
	defer rs.Close()
	for rs.Next() {
		var (
			st  string
			cur DeltaCursor
			net sql.NullInt64
			end sql.NullTime
		)
		if err := rs.Scan(&st, &cur.LastSeq, &net, &end); err != nil {
			return "", DeltaCursor{}, 0, fmt.Errorf("scan delta cursor bin=%d epoch=%d: %w", binID, epoch, err)
		}
		if net.Valid {
			v := net.Int64
			cur.AppliedNet = &v
		}
		if end.Valid {
			t := end.Time
			cur.AppliedWindowEnd = &t
		}
		rows++
		if rows == 1 {
			station, c = st, cur
		}
	}
	if err := rs.Err(); err != nil {
		return "", DeltaCursor{}, 0, fmt.Errorf("read delta cursors bin=%d epoch=%d: %w", binID, epoch, err)
	}
	if rows != 1 {
		return "", DeltaCursor{}, rows, nil
	}
	return station, c, rows, nil
}
