package inventory

import (
	"database/sql"
	"fmt"
	"strconv"

	"shingo/protocol"
)

// RowsQuerier is what BinCursors reads through: *sql.DB and *sql.Tx.
type RowsQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// BinCursors returns Core's cursor for this carrier in this generation for
// every station that has count messages applied there, by station: the dedup
// rows of scope ('bin', binID, epoch). A station with no row has had none of
// its count messages applied in this generation. It is what a count Core
// states is fenced on, each addressed station at its own cursor
// (service.cursorFences; SYNTH-round2 S7).
//
// WHY NOT THE NODE'S STATION. node_stations is many-to-many (a node lists the
// stations allowed to order against it, with "all" and inherit modes), and it
// says nothing about which station counts the carrier sitting there. The dedup
// row is written by exactly the stream the fence has to splice into, so it is
// the answer rather than a guess at it.
//
// One statement. The table's primary key leads with station, so this is a
// scan of inventory_delta_dedup rather than a key lookup. It runs on a count
// Core states (a person's count, a late draw), not per message.
func BinCursors(q RowsQuerier, binID, epoch int64) (map[string]DeltaCursor, error) {
	rs, err := q.Query(`SELECT station_uid, last_seq, applied_net, applied_window_end
		FROM inventory_delta_dedup
		WHERE scope_kind=$1 AND scope_key=$2 AND epoch=$3`,
		protocol.InvDeltaScopeBin, strconv.FormatInt(binID, 10), epoch)
	if err != nil {
		return nil, fmt.Errorf("read delta cursors bin=%d epoch=%d: %w", binID, epoch, err)
	}
	defer rs.Close()
	out := map[string]DeltaCursor{}
	for rs.Next() {
		var (
			st  string
			cur DeltaCursor
			net sql.NullInt64
			end sql.NullTime
		)
		if err := rs.Scan(&st, &cur.LastSeq, &net, &end); err != nil {
			return nil, fmt.Errorf("scan delta cursor bin=%d epoch=%d: %w", binID, epoch, err)
		}
		if net.Valid {
			v := net.Int64
			cur.AppliedNet = &v
		}
		if end.Valid {
			t := end.Time
			cur.AppliedWindowEnd = &t
		}
		out[st] = cur
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("read delta cursors bin=%d epoch=%d: %w", binID, epoch, err)
	}
	return out, nil
}
