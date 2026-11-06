package counters

// run_out.go — a changeover's run-out, measured from the production record:
// the strokes on the part of the style it left that the counter counted after
// the landing and before its next reset. Where a cell drains before it
// changes program, that is the parts that sat between the cart and the
// counter at the landing. Read on the setup page and the changeover history;
// it decides nothing.

import (
	"database/sql"
	"strings"

	"shingo/protocol/clock"
)

// Landing is a completed changeover's landing: its id, when (unix ms), and
// the CATIDs of the style it left.
type Landing struct {
	ID     int64
	AtMs   int64
	CATIDs []string
}

// resetRow is a reset row of the process's production record.
type resetRow struct {
	id, ms int64
}

// RunOuts measures each landing's run-out on a process's production record,
// keyed by changeover id: the strokes counted on one of its CATIDs from the
// landing's second (completed_at is stored to the second) until the first
// reset after it. A landing is measured only when it is inside the record's
// retention (every row since it is still kept, so the first reset kept after
// it is the first), a reset has followed it, and at least one such stroke was
// counted between the two: a program that changed before the landing leaves
// none (the line had already run out).
//
// Statements: the process's resets (the anomaly index), and one sum per
// measurable landing over the rows between the reset before its reset and
// that reset (a rowid range). None for no landing in the retention.
func RunOuts(db *sql.DB, processID int64, landings []Landing) (map[int64]int64, error) {
	out := map[int64]int64{}
	kept := clock.Now().Add(-SnapshotRetention).UnixMilli()
	var measurable []Landing
	for _, l := range landings {
		if len(l.CATIDs) > 0 && l.AtMs >= kept {
			measurable = append(measurable, l)
		}
	}
	if len(measurable) == 0 {
		return out, nil
	}
	resets, err := processResets(db, processID)
	if err != nil {
		return nil, err
	}
	for _, l := range measurable {
		lo, hi, ok := resetAfter(resets, l.AtMs)
		if !ok {
			continue
		}
		args := []any{lo, hi, processID, l.AtMs}
		for _, c := range l.CATIDs {
			args = append(args, c)
		}
		var parts, rows int64
		err := db.QueryRow(`SELECT COALESCE(SUM(delta), 0), COUNT(*) FROM counter_snapshots
			WHERE id > ? AND id < ? AND process_id = ? AND recorded_ms >= ? AND delta > 0
			  AND catid IN (?`+strings.Repeat(", ?", len(l.CATIDs)-1)+`)`, args...).Scan(&parts, &rows)
		if err != nil {
			return nil, err
		}
		if rows > 0 {
			out[l.ID] = parts
		}
	}
	return out, nil
}

// processResets is the process's reset rows, oldest first.
func processResets(db *sql.DB, processID int64) ([]resetRow, error) {
	rows, err := db.Query(`SELECT id, COALESCE(recorded_ms, 0) FROM counter_snapshots
		WHERE anomaly = 'reset' AND process_id = ? ORDER BY id`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []resetRow
	for rows.Next() {
		var r resetRow
		if err := rows.Scan(&r.id, &r.ms); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resetAfter finds the first reset at or after atMs, and bounds the rows
// before it to those after the reset before it (0 when none).
func resetAfter(resets []resetRow, atMs int64) (lo, hi int64, ok bool) {
	for i, r := range resets {
		if r.ms < atMs {
			continue
		}
		if i > 0 {
			lo = resets[i-1].id
		}
		return lo, r.id, true
	}
	return 0, 0, false
}
