package store

// catid_holds.go — the ledger of cart draws no cart took (engine
// catid_holds.go has the rules): a stroke Shingo could not place by the PLC's
// part, and a stroke with no cart of its part at its position (decision 55).
// One open row per (process, CATID, node, part, last cart off), grown stroke
// by stroke; a person's Charge or Write off latches resolved_at before
// anything moves.

import (
	"database/sql"
	"errors"
	"time"

	"shingoedge/store/internal/helpers"
	"shingoedge/store/processes"
)

// CatidHold is one catid_holds row. LastBinID, LastDeclared and LastLeftAt are
// the last cart released at the position as the row's first stroke found it
// (a "no cart" row's; 0 and nil when none had been, and on every other row).
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
	LastBinID     int64      `json:"last_bin_id,omitempty"`
	LastDeclared  int64      `json:"last_declared"`
	LastLeftAt    *time.Time `json:"last_left_at,omitempty"`
}

const catidHoldCols = `id, process_id, catid, process_node_id, payload_code, reason, qty, strokes,
	first_tick_at, last_tick_at, COALESCE(resolved_at, ''), resolution, resolved_by, last_bin_id, last_declared, last_left_at`

func scanCatidHold(sc interface{ Scan(...any) error }) (CatidHold, error) {
	var h CatidHold
	var first, last, resolved, left string
	err := sc.Scan(&h.ID, &h.ProcessID, &h.CATID, &h.ProcessNodeID, &h.PayloadCode, &h.Reason, &h.Qty, &h.Strokes,
		&first, &last, &resolved, &h.Resolution, &h.ResolvedBy, &h.LastBinID, &h.LastDeclared, &left)
	if err != nil {
		return h, err
	}
	h.FirstTickAt = helpers.ScanTime(first)
	h.LastTickAt = helpers.ScanTime(last)
	if resolved != "" {
		t := helpers.ScanTime(resolved)
		h.ResolvedAt = &t
	}
	if left != "" {
		t := helpers.ScanTime(left)
		h.LastLeftAt = &t
	}
	return h, nil
}

// HoldCatidDraw adds qty (one stroke) to the open hold for (process, CATID,
// node, part), opening it when there is none. Reports whether this stroke
// opened it. Its rows carry no last cart (0, ”), so a held or off-plan share
// is one row however many carts come and go.
func (db *DB) HoldCatidDraw(processID int64, catid string, nodeID int64, payload, reason string, qty int64, at time.Time) (bool, error) {
	ts := at.UTC().Format(helpers.TimeLayout)
	var strokes int64
	err := db.QueryRow(`INSERT INTO catid_holds
		(process_id, catid, process_node_id, payload_code, reason, qty, strokes, first_tick_at, last_tick_at,
		 last_bin_id, last_declared, last_left_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, 0, 0, '')
		ON CONFLICT (process_id, catid, process_node_id, payload_code, last_bin_id, last_left_at) WHERE resolved_at IS NULL DO UPDATE SET
			qty = qty + excluded.qty, strokes = strokes + 1, last_tick_at = excluded.last_tick_at
		RETURNING strokes`,
		processID, catid, nodeID, payload, reason, qty, ts, ts).Scan(&strokes)
	return strokes == 1, err
}

// LogNoCartStroke adds qty (one stroke) to the open "no cart" row for
// (process, CATID, node, part) and the seat's last release, opening it with
// that release as its last cart off (released_bin_id / released_count /
// released_at, read in the same statement), on the seat as the tick read it
// (processes.SeatUnchanged): a seat changed since — a cart landed, left or was
// released under the count lock — refuses the write, and the tick re-reads
// the seat. written reports whether the row took the stroke, opened whether
// this stroke opened it. The hold write it replaces was one statement on the
// tick path; so is this.
func (db *DB) LogNoCartStroke(processID int64, catid string, nodeID int64, payload, reason string, qty int64, at time.Time,
	seat processes.SeatAsRead) (written, opened bool, err error) {
	ts := at.UTC().Format(helpers.TimeLayout)
	where, seatArgs := processes.SeatUnchanged(nodeID, seat)
	args := append([]any{processID, catid, nodeID, payload, reason, qty, ts, ts}, seatArgs...)
	var strokes int64
	err = db.QueryRow(`INSERT INTO catid_holds
		(process_id, catid, process_node_id, payload_code, reason, qty, strokes, first_tick_at, last_tick_at,
		 last_bin_id, last_declared, last_left_at)
		SELECT ?, ?, ?, ?, ?, ?, 1, ?, ?, COALESCE(released_bin_id, 0), released_count, COALESCE(released_at, '')
		FROM process_node_runtime_states WHERE `+where+`
		ON CONFLICT (process_id, catid, process_node_id, payload_code, last_bin_id, last_left_at) WHERE resolved_at IS NULL DO UPDATE SET
			qty = qty + excluded.qty, strokes = strokes + 1, last_tick_at = excluded.last_tick_at
		RETURNING strokes`, args...).Scan(&strokes)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, strokes == 1, nil
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
// Returns the quantity it latched — a stroke may have grown the row since the
// release read it, and the release charges what it resolved — and whether
// this call resolved it; a second release writes nothing.
func (db *DB) ResolveCatidHold(id int64, resolution, by string, at time.Time) (int64, bool, error) {
	var qty int64
	err := db.QueryRow(`UPDATE catid_holds SET resolved_at = ?, resolution = ?, resolved_by = ?
		WHERE id = ? AND resolved_at IS NULL RETURNING qty`, at.UTC().Format(helpers.TimeLayout), resolution, by, id).Scan(&qty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return qty, true, nil
}
