package processes

// line_depth.go — the parts a person says sit between the cart and an
// end-of-line counter: shown in the end-of-line sentence where nothing is
// measured, labelled with who typed it and when. Nothing reads it to decide.

import (
	"database/sql"
	"time"

	"shingoedge/store/internal/helpers"
)

// LineDepth is a process's typed line depth; N nil is none typed.
type LineDepth struct {
	N  *int64     `json:"n"`
	By string     `json:"by"`
	At *time.Time `json:"at,omitempty"`
}

// GetLineDepth reads a process's typed line depth.
func GetLineDepth(db *sql.DB, processID int64) (LineDepth, error) {
	var d LineDepth
	var n sql.NullInt64
	var at sql.NullString
	err := db.QueryRow(`SELECT line_depth_typed, line_depth_typed_by, line_depth_typed_at FROM processes WHERE id = ?`,
		processID).Scan(&n, &d.By, &at)
	if err != nil {
		return d, err
	}
	if n.Valid {
		d.N = &n.Int64
	}
	d.At = helpers.ScanTimePtr(at)
	return d, nil
}

// SetLineDepth types a process's line depth, by whom and when; nil clears it
// (and who and when with it).
func SetLineDepth(db *sql.DB, processID int64, n *int64, by string, at time.Time) error {
	if n == nil {
		_, err := db.Exec(`UPDATE processes SET line_depth_typed = NULL, line_depth_typed_by = '', line_depth_typed_at = NULL
			WHERE id = ?`, processID)
		return err
	}
	_, err := db.Exec(`UPDATE processes SET line_depth_typed = ?, line_depth_typed_by = ?, line_depth_typed_at = ? WHERE id = ?`,
		*n, by, at.UTC().Format(helpers.TimeLayout), processID)
	return err
}
