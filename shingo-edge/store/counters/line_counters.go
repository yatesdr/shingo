package counters

// line_counters.go — the station counters (line_counters): a tag that clicks
// once per assembly where the part leaves the cart, counting the positions
// bound to it in place of the production counter.

import (
	"database/sql"
	"errors"
	"time"

	"shingoedge/store/internal/helpers"
	"shingoedge/store/processes"
)

// LineCounter is one line_counters row. The poll's list also carries the
// process's active style and its production point's last poll time.
type LineCounter struct {
	ID             int64      `json:"id"`
	ProcessID      int64      `json:"process_id"`
	Label          string     `json:"label"`
	PLCName        string     `json:"plc_name"`
	TagName        string     `json:"tag_name"`
	LastCount      int64      `json:"last_count"`
	LastPollAt     *time.Time `json:"last_poll_at,omitempty"`
	WarlinkManaged bool       `json:"warlink_managed"`

	// Poll-list only.
	ActiveStyleID int64      `json:"-"`
	PointPollAt   *time.Time `json:"-"`
}

const lineCounterCols = `lc.id, lc.process_id, lc.label, lc.plc_name, lc.tag_name, lc.last_count,
	COALESCE(lc.last_poll_at, ''), lc.warlink_managed`

func scanLineCounter(sc interface{ Scan(...any) error }, extra ...any) (LineCounter, error) {
	var c LineCounter
	var polled string
	dest := append([]any{&c.ID, &c.ProcessID, &c.Label, &c.PLCName, &c.TagName, &c.LastCount, &polled, &c.WarlinkManaged}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return c, err
	}
	c.LastPollAt = helpers.ScanTimePtr(sql.NullString{String: polled, Valid: polled != ""})
	return c, nil
}

// CreateLineCounter inserts a station counter, unread. plc_name is the
// process's counter PLC: a station counter reads the cell's own PLC.
func CreateLineCounter(db *sql.DB, processID int64, label, plcName, tagName string) (int64, error) {
	res, err := db.Exec(`INSERT INTO line_counters (process_id, label, plc_name, tag_name) VALUES (?, ?, ?, ?)`,
		processID, label, plcName, tagName)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetLineCounter returns one station counter, or (nil, nil) when there is none.
func GetLineCounter(db *sql.DB, id int64) (*LineCounter, error) {
	c, err := scanLineCounter(db.QueryRow(`SELECT `+lineCounterCols+` FROM line_counters lc WHERE lc.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListLineCounters returns a process's station counters, by id.
func ListLineCounters(db *sql.DB, processID int64) ([]LineCounter, error) {
	rows, err := db.Query(`SELECT `+lineCounterCols+` FROM line_counters lc WHERE lc.process_id = ? ORDER BY lc.id`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LineCounter
	for rows.Next() {
		c, err := scanLineCounter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListPolledLineCounters returns the station counters the poll reads: those
// of a live process whose production counter is wired and enabled, with an
// active style, each with that style and its production point's last poll.
func ListPolledLineCounters(db *sql.DB) ([]LineCounter, error) {
	rows, err := db.Query(`SELECT ` + lineCounterCols + `, p.active_style_id,
		COALESCE((SELECT MAX(rp.last_poll_at) FROM reporting_points rp
			WHERE rp.plc_name = p.counter_plc_name AND rp.tag_name = p.counter_tag_name), '')
		FROM line_counters lc JOIN processes p ON p.id = lc.process_id
		WHERE p.counter_plc_name <> '' AND p.counter_tag_name <> '' AND p.counter_enabled = 1
		  AND p.active_style_id IS NOT NULL
		ORDER BY lc.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LineCounter
	for rows.Next() {
		var point string
		var c0 int64
		c, err := scanLineCounter(rows, &c0, &point)
		if err != nil {
			return nil, err
		}
		c.ActiveStyleID = c0
		c.PointPollAt = helpers.ScanTimePtr(sql.NullString{String: point, Valid: point != ""})
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateLineCounterRead writes a read the poll counted (or seeded) from: the
// count and the poll time, only while the row still reads the tag the pass
// listed. Reports whether it wrote; no write, no emit.
func UpdateLineCounterRead(db *sql.DB, id int64, plcName, tagName string, count int64, at time.Time) (bool, error) {
	res, err := db.Exec(`UPDATE line_counters SET last_count = ?, last_poll_at = ?
		WHERE id = ? AND plc_name = ? AND tag_name = ?`,
		count, at.UTC().Format(helpers.TimeLayout), id, plcName, tagName)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeleteLineCounter deletes a station counter and unbinds its positions, in
// one transaction: no binding is left to read as unbound by accident.
func DeleteLineCounter(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE process_nodes SET line_counter_id = NULL WHERE line_counter_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM line_counters WHERE id = ?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	processes.BumpNodeGeneration()
	return nil
}

// SetNodeLineCounter binds a position to a station counter (nil unbinds it).
// The setter's rules (commit 11) are the engine's; this is the write.
func SetNodeLineCounter(db *sql.DB, nodeID int64, counterID *int64) error {
	if _, err := db.Exec(`UPDATE process_nodes SET line_counter_id = ? WHERE id = ?`, counterID, nodeID); err != nil {
		return err
	}
	processes.BumpNodeGeneration()
	return nil
}
