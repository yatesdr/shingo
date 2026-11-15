package processes

import (
	"database/sql"
	"fmt"
	"time"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// auto_reorder_switch.go — a line's auto-reorder switch: a person's off at
// every position of the line, the setting that shows the switch on the station,
// and the record of every flip.

// SetAutoReorderSwitchHMI shows or hides the line's auto-reorder switch on the
// station board. A focused setter, like SetFlowComposerEnabled.
func SetAutoReorderSwitchHMI(db *sql.DB, processID int64, allowed bool) error {
	_, err := db.Exec(`UPDATE processes SET auto_reorder_switch_hmi=? WHERE id=?`, allowed, processID)
	return err
}

// FlipAutoReorder is a person flipping a line's auto-reorder, in one
// transaction: one UPDATE over every live position of the process, and the
// flip's row in auto_reorder_flips. Off writes a person's off at each position
// (the top rank, so it lands over any hold), with by as its note. On clears
// every hold there, a person's off included, which nothing else clears, and
// stamps the position's go: turning it on is a person saying go. at is the
// engine's clock. It reports how many positions it wrote.
func FlipAutoReorder(db *sql.DB, processID int64, on bool, by, source string, at time.Time) (int64, error) {
	stamp := at.UTC().Format(helpers.TimeLayout)
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	set := `reorder_hold=?, reorder_hold_at=?, reorder_hold_note=?`
	args := []any{string(domain.ReorderHoldPersonOff), stamp, by}
	if on {
		set = `reorder_hold='', reorder_hold_at='', reorder_hold_note='', reorder_go_at=?`
		args = []any{stamp}
	}
	args = append(args, processID)
	res, err := tx.Exec(`UPDATE process_node_runtime_states SET `+set+`
		WHERE process_node_id IN (SELECT id FROM process_nodes WHERE process_id=? AND deleted_at IS NULL)`, args...)
	if err != nil {
		return 0, fmt.Errorf("flip auto-reorder on process %d: %w", processID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO auto_reorder_flips (process_id, turned_on, by, source, at) VALUES (?, ?, ?, ?, ?)`,
		processID, on, by, source, stamp); err != nil {
		return 0, fmt.Errorf("record the flip on process %d: %w", processID, err)
	}
	return n, tx.Commit()
}

// ListAutoReorderFlips is the line's last limit flips, newest first.
func ListAutoReorderFlips(db *sql.DB, processID int64, limit int) ([]domain.AutoReorderFlip, error) {
	rows, err := db.Query(`SELECT id, process_id, turned_on, by, source, at FROM auto_reorder_flips
		WHERE process_id=? ORDER BY id DESC LIMIT ?`, processID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AutoReorderFlip
	for rows.Next() {
		var f domain.AutoReorderFlip
		var at string
		if err := rows.Scan(&f.ID, &f.ProcessID, &f.TurnedOn, &f.By, &f.Source, &at); err != nil {
			return nil, err
		}
		f.At = helpers.ScanTime(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// SwitchedOffPositions counts the live positions of a process and those
// holding a person's off, in one statement: Processes' read of the switch.
func SwitchedOffPositions(db *sql.DB, processID int64) (off, positions int, err error) {
	err = db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(r.reorder_hold = ?), 0)
		FROM process_nodes pn JOIN process_node_runtime_states r ON r.process_node_id = pn.id
		WHERE pn.process_id = ? AND pn.deleted_at IS NULL`, string(domain.ReorderHoldPersonOff), processID).Scan(&positions, &off)
	return off, positions, err
}
