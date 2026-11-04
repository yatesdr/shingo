package store

// catid_holds.go — the ledger of cart draws held per CATID (engine
// catid_holds.go has the rules). One open row per (process, CATID, node,
// part), grown stroke by stroke; a release latches resolved_at before anything
// moves.

import (
	"database/sql"
	"errors"
	"time"

	"shingoedge/store/internal/helpers"
)

// CatidHold is one catid_holds row.
type CatidHold struct {
	ID            int64      `json:"id"`
	ProcessID     int64      `json:"process_id"`
	CATID         string     `json:"catid"`
	ProcessNodeID int64      `json:"process_node_id"`
	PayloadCode   string     `json:"payload_code"`
	Reason        string     `json:"reason"`
	Qty           int64      `json:"qty"`
	Strokes       int64      `json:"strokes"`
	FirstTickAt   time.Time  `json:"first_tick_at"`
	LastTickAt    time.Time  `json:"last_tick_at"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	Resolution    string     `json:"resolution,omitempty"`
	ResolvedBy    string     `json:"resolved_by,omitempty"`
}

const catidHoldCols = `id, process_id, catid, process_node_id, payload_code, reason, qty, strokes,
	first_tick_at, last_tick_at, COALESCE(resolved_at, ''), resolution, resolved_by`

func scanCatidHold(sc interface{ Scan(...any) error }) (CatidHold, error) {
	var h CatidHold
	var first, last, resolved string
	err := sc.Scan(&h.ID, &h.ProcessID, &h.CATID, &h.ProcessNodeID, &h.PayloadCode, &h.Reason, &h.Qty, &h.Strokes,
		&first, &last, &resolved, &h.Resolution, &h.ResolvedBy)
	if err != nil {
		return h, err
	}
	h.FirstTickAt = helpers.ScanTime(first)
	h.LastTickAt = helpers.ScanTime(last)
	if resolved != "" {
		t := helpers.ScanTime(resolved)
		h.ResolvedAt = &t
	}
	return h, nil
}

// HoldCatidDraw adds qty (one stroke) to the open hold for (process, CATID,
// node, part), opening it when there is none. Reports whether this stroke
// opened it.
func (db *DB) HoldCatidDraw(processID int64, catid string, nodeID int64, payload, reason string, qty int64, at time.Time) (bool, error) {
	ts := at.UTC().Format(helpers.TimeLayout)
	var strokes int64
	err := db.QueryRow(`INSERT INTO catid_holds
		(process_id, catid, process_node_id, payload_code, reason, qty, strokes, first_tick_at, last_tick_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (process_id, catid, process_node_id, payload_code) WHERE resolved_at IS NULL DO UPDATE SET
			qty = qty + excluded.qty, strokes = strokes + 1, last_tick_at = excluded.last_tick_at
		RETURNING strokes`,
		processID, catid, nodeID, payload, reason, qty, ts, ts).Scan(&strokes)
	return strokes == 1, err
}

// ListOpenCatidHolds returns a process's unresolved holds, oldest first.
func (db *DB) ListOpenCatidHolds(processID int64) ([]CatidHold, error) {
	rows, err := db.Query(`SELECT `+catidHoldCols+` FROM catid_holds
		WHERE process_id = ? AND resolved_at IS NULL ORDER BY id`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatidHold
	for rows.Next() {
		h, err := scanCatidHold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetCatidHold returns one hold, or (nil, nil) when there is none.
func (db *DB) GetCatidHold(id int64) (*CatidHold, error) {
	h, err := scanCatidHold(db.QueryRow(`SELECT `+catidHoldCols+` FROM catid_holds WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// ResolveCatidHold latches a hold's release: written only while it is open.
// Reports whether this call resolved it; a second release writes nothing.
func (db *DB) ResolveCatidHold(id int64, resolution, by string, at time.Time) (bool, error) {
	res, err := db.Exec(`UPDATE catid_holds SET resolved_at = ?, resolution = ?, resolved_by = ?
		WHERE id = ? AND resolved_at IS NULL`, at.UTC().Format(helpers.TimeLayout), resolution, by, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CountChangeoversStartedSince counts the process's changeovers started at or
// after since, other than exceptID.
func (db *DB) CountChangeoversStartedSince(processID int64, since time.Time, exceptID int64) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM process_changeovers
		WHERE process_id = ? AND started_at >= ? AND id <> ?`,
		processID, since.UTC().Format(helpers.TimeLayout), exceptID).Scan(&n)
	return n, err
}
