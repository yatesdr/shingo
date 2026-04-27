// Package shifts holds shift-table persistence for shingo-edge.
//
// Phase 5b of the architecture plan moved the shifts CRUD out of the
// flat store/ package and into this sub-package. The outer store/
// keeps a type alias (`store.Shift = shifts.Shift`) and one-line
// delegate methods on *store.DB so external callers see no API change.
package shifts

import (
	"database/sql"

	"shingoedge/domain"
)

// Shift represents a work shift with start/end times. The struct
// lives in shingoedge/domain (Stage 2A.2); this alias keeps the
// shifts.Shift name used by every scan helper, Upsert call site,
// and the outer store/ re-export.
type Shift = domain.Shift

// List returns all shifts ordered by shift_number.
func List(db *sql.DB) ([]Shift, error) {
	rows, err := db.Query("SELECT id, name, shift_number, start_time, end_time FROM shifts ORDER BY shift_number")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ss []Shift
	for rows.Next() {
		var s Shift
		if err := rows.Scan(&s.ID, &s.Name, &s.ShiftNumber, &s.StartTime, &s.EndTime); err != nil {
			return nil, err
		}
		ss = append(ss, s)
	}
	return ss, rows.Err()
}

// Upsert inserts or replaces a shift by shift_number.
func Upsert(db *sql.DB, shiftNumber int, name, startTime, endTime string) error {
	_, err := db.Exec(
		`INSERT INTO shifts (shift_number, name, start_time, end_time)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(shift_number) DO UPDATE SET name=excluded.name, start_time=excluded.start_time, end_time=excluded.end_time`,
		shiftNumber, name, startTime, endTime,
	)
	return err
}

// Delete removes a shift by shift_number.
func Delete(db *sql.DB, shiftNumber int) error {
	_, err := db.Exec("DELETE FROM shifts WHERE shift_number = ?", shiftNumber)
	return err
}
