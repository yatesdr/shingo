package counters

// line_counters.go — the station counters (line_counters): a tag that clicks
// once per assembly where the part leaves the cart, counting the positions
// bound to it in place of the production counter. A counter may read a second
// tag (tag_name_2, '' for none) and counts the sum of the two: a station with
// two fixtures, one per hand, of which one runs at a time. Each tag keeps its
// own reading (last_count / last_poll_at, last_count_2 / last_poll_at_2) and
// its own publishing (warlink_managed, warlink_managed_2).

import (
	"database/sql"
	"errors"
	"time"

	"shingoedge/store/internal/helpers"
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
	// The second tag ('' for none), with its own reading and publishing.
	TagName2        string     `json:"tag_name_2"`
	LastCount2      int64      `json:"last_count_2"`
	LastPollAt2     *time.Time `json:"last_poll_at_2,omitempty"`
	WarlinkManaged2 bool       `json:"warlink_managed_2"`

	// Poll-list only.
	ActiveStyleID int64      `json:"-"`
	PointPollAt   *time.Time `json:"-"`
}

// Tags is the counter's tags, the first first: one, or two when it reads a
// second.
func (c LineCounter) Tags() []string {
	if c.TagName2 == "" {
		return []string{c.TagName}
	}
	return []string{c.TagName, c.TagName2}
}

const lineCounterCols = `lc.id, lc.process_id, lc.label, lc.plc_name, lc.tag_name, lc.last_count,
	COALESCE(lc.last_poll_at, ''), lc.warlink_managed,
	lc.tag_name_2, lc.last_count_2, COALESCE(lc.last_poll_at_2, ''), lc.warlink_managed_2`

func scanLineCounter(sc interface{ Scan(...any) error }, extra ...any) (LineCounter, error) {
	var c LineCounter
	var polled, polled2 string
	dest := append([]any{&c.ID, &c.ProcessID, &c.Label, &c.PLCName, &c.TagName, &c.LastCount, &polled, &c.WarlinkManaged,
		&c.TagName2, &c.LastCount2, &polled2, &c.WarlinkManaged2}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return c, err
	}
	c.LastPollAt = helpers.ScanTimePtr(sql.NullString{String: polled, Valid: polled != ""})
	c.LastPollAt2 = helpers.ScanTimePtr(sql.NullString{String: polled2, Valid: polled2 != ""})
	return c, nil
}

// CreateLineCounter inserts a station counter, unread, reading tagName and
// tagName2 ("" for none). plc_name is the process's counter PLC: a station
// counter reads the cell's own PLC.
func CreateLineCounter(db *sql.DB, processID int64, label, plcName, tagName, tagName2 string) (int64, error) {
	res, err := db.Exec(`INSERT INTO line_counters (process_id, label, plc_name, tag_name, tag_name_2) VALUES (?, ?, ?, ?, ?)`,
		processID, label, plcName, tagName, tagName2)
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

// UpdateLineCounterRead writes a read the poll counted (or seeded) from, of
// the counter's first tag (second false) or its second: that tag's count and
// poll time, only while the row still reads that tag where the pass listed it.
// Reports whether it wrote; no write, no emit.
func UpdateLineCounterRead(db *sql.DB, id int64, plcName, tagName string, second bool, count int64, at time.Time) (bool, error) {
	q := `UPDATE line_counters SET last_count = ?, last_poll_at = ?
		WHERE id = ? AND plc_name = ? AND tag_name = ?`
	if second {
		q = `UPDATE line_counters SET last_count_2 = ?, last_poll_at_2 = ?
		WHERE id = ? AND plc_name = ? AND tag_name_2 = ? AND tag_name_2 <> ''`
	}
	res, err := db.Exec(q, count, at.UTC().Format(helpers.TimeLayout), id, plcName, tagName)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// LineCounterByTag returns the station counter reading plc/tag, as its first
// tag or its second, or nil.
func LineCounterByTag(db *sql.DB, plcName, tagName string) (*LineCounter, error) {
	c, err := scanLineCounter(db.QueryRow(`SELECT `+lineCounterCols+` FROM line_counters lc
		WHERE lc.plc_name = ? AND (lc.tag_name = ? OR (lc.tag_name_2 = ? AND lc.tag_name_2 <> ''))
		ORDER BY lc.id LIMIT 1`, plcName, tagName, tagName))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ReportingPointOnTag reports whether a reporting point reads plc/tag.
func ReportingPointOnTag(db *sql.DB, plcName, tagName string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM reporting_points WHERE plc_name = ? AND tag_name = ?`, plcName, tagName).Scan(&n)
	return n > 0, err
}

// RenameLineCounter sets a station counter's label.
func RenameLineCounter(db *sql.DB, id int64, label string) error {
	_, err := db.Exec(`UPDATE line_counters SET label = ? WHERE id = ?`, label, id)
	return err
}

// RepointLineCounter sets a station counter's label and its two tags
// (tagName2 "" for none). A tag that changes is unread again (its last poll
// NULL) and seeds from its new tag's first read; a tag that stays keeps its
// reading. Each tag's WarLink publishing is recorded as managed or not.
// SQLite evaluates every SET right-hand side against the row as it was, so
// each CASE compares the old tag.
func RepointLineCounter(db *sql.DB, id int64, label, tagName, tagName2 string, managed, managed2 bool) error {
	_, err := db.Exec(`UPDATE line_counters SET label = ?,
		last_count = CASE WHEN tag_name = ? THEN last_count ELSE 0 END,
		last_poll_at = CASE WHEN tag_name = ? THEN last_poll_at ELSE NULL END,
		tag_name = ?, warlink_managed = ?,
		last_count_2 = CASE WHEN tag_name_2 = ? AND tag_name_2 <> '' THEN last_count_2 ELSE 0 END,
		last_poll_at_2 = CASE WHEN tag_name_2 = ? AND tag_name_2 <> '' THEN last_poll_at_2 ELSE NULL END,
		tag_name_2 = ?, warlink_managed_2 = ?
		WHERE id = ?`,
		label, tagName, tagName, tagName, managed, tagName2, tagName2, tagName2, managed2 && tagName2 != "", id)
	return err
}

// SetLineCounterManaged records whether the Edge enabled the publishing of
// the counter's first tag (second false) or its second.
func SetLineCounterManaged(db *sql.DB, id int64, second, managed bool) error {
	q := `UPDATE line_counters SET warlink_managed = ? WHERE id = ?`
	if second {
		q = `UPDATE line_counters SET warlink_managed_2 = ? WHERE id = ?`
	}
	_, err := db.Exec(q, managed, id)
	return err
}

// DeleteLineCounter deletes a station counter and unbinds its positions, in
// one transaction: no binding is left to read as unbound by accident. It
// reports how many positions it unbound; the store's wrapper publishes the
// node generation (processes.BumpNodeGeneration) when that is any.
func DeleteLineCounter(db *sql.DB, id int64) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE process_nodes SET line_counter_id = NULL WHERE line_counter_id = ?`, id)
	if err != nil {
		return 0, err
	}
	unbound, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM line_counters WHERE id = ?`, id); err != nil {
		return 0, err
	}
	return unbound, tx.Commit()
}

// SetNodeLineCounter binds a position to a station counter (nil unbinds it).
// The rules are the engine's (SetCountedAt); this is the write. It reports
// whether the binding changed; the store's wrapper publishes the node
// generation (processes.BumpNodeGeneration) when it did.
func SetNodeLineCounter(db *sql.DB, nodeID int64, counterID *int64) (bool, error) {
	res, err := db.Exec(`UPDATE process_nodes SET line_counter_id = ? WHERE id = ? AND line_counter_id IS NOT ?`,
		counterID, nodeID, counterID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
