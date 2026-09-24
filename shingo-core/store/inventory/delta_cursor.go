package inventory

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"shingo/protocol"
)

// DeltaCursor is how far Core has applied one count-message scope: the
// highest seq applied (the order guard) and the running net that message
// carried (applied_net), with the Edge-time end of its window.
//
// AppliedNet is nil when the scope has only ever been applied from messages
// that carried no net (an Edge built before the running net), and on every
// row that existed before v126. AppliedWindowEnd is nil on rows not applied
// since v126.
type DeltaCursor struct {
	LastSeq          int64
	AppliedNet       *int64
	AppliedWindowEnd *time.Time
}

// RowQuerier is what BinDeltaCursor reads through: *sql.DB, *sql.Tx and
// *store.DB all satisfy it, so a caller can read inside its own transaction.
type RowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// BinDeltaCursor returns Core's cursor for one bin scope — (station, bin,
// epoch) — and whether Core has a dedup row for it at all. One primary-key
// read.
//
// This is the read a record-count fence needs (SYNTH-round2 S7): the
// applied_net and last_seq Core had when it wrote an absolute count, so the
// station can rebase its own number on the same point in its stream. Pass the
// record-count transaction as q to read them consistently with the write.
func BinDeltaCursor(q RowQuerier, station string, binID, epoch int64) (DeltaCursor, bool, error) {
	var (
		c   DeltaCursor
		net sql.NullInt64
		end sql.NullTime
	)
	err := q.QueryRow(`SELECT last_seq, applied_net, applied_window_end
		FROM inventory_delta_dedup
		WHERE station=$1 AND scope_kind=$2 AND scope_key=$3 AND epoch=$4`,
		station, protocol.InvDeltaScopeBin, strconv.FormatInt(binID, 10), epoch).Scan(&c.LastSeq, &net, &end)
	if errors.Is(err, sql.ErrNoRows) {
		return DeltaCursor{}, false, nil
	}
	if err != nil {
		return DeltaCursor{}, false, fmt.Errorf("read delta cursor station=%s bin=%d epoch=%d: %w", station, binID, epoch, err)
	}
	if net.Valid {
		v := net.Int64
		c.AppliedNet = &v
	}
	if end.Valid {
		t := end.Time
		c.AppliedWindowEnd = &t
	}
	return c, true, nil
}
