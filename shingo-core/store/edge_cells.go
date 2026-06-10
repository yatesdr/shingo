package store

import (
	"encoding/json"
	"time"
)

// EdgeCell is one auto-derived catalog cell (Q-034): a PLC an edge reported,
// with its process bindings as raw JSON (each {process_id, style_id, plc_name,
// tag_name}). Bindings stays opaque here so the store layer needn't know the
// wire protocol's binding shape — the messaging handler marshals it in, the API
// serves it back out.
type EdgeCell struct {
	Station   string          `json:"station"`
	CellLabel string          `json:"cell_label"`
	Bindings  json.RawMessage `json:"bindings"`
	FirstSeen time.Time       `json:"first_seen"`
	LastSeen  time.Time       `json:"last_seen"`
	Stale     bool            `json:"stale"`
}

// UpsertEdgeCells reconciles a station's full catalog in one transaction: mark
// every existing cell for the station stale, then upsert each cell in the new
// catalog (refreshing bindings + last_seen and clearing stale). Cells absent
// from the new catalog stay stale — never deleted, so a momentarily-incomplete
// catalog or a retired PLC keeps its history visible (the scenesync ghost
// lesson). The stale-first / clear-on-upsert ordering avoids binding a Go slice
// into a SQL array (pgx stdlib has no native []string array param).
func (db *DB) UpsertEdgeCells(station string, cells []EdgeCell) error {
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.Exec(`UPDATE edge_cells SET stale = TRUE WHERE station = $1`, station); err != nil {
		return err
	}
	for _, c := range cells {
		b := c.Bindings
		if len(b) == 0 {
			b = json.RawMessage("[]")
		}
		if _, err := tx.Exec(`
			INSERT INTO edge_cells (station, cell_label, bindings, last_seen, stale)
			VALUES ($1, $2, $3, NOW(), FALSE)
			ON CONFLICT (station, cell_label) DO UPDATE
			   SET bindings = EXCLUDED.bindings, last_seen = NOW(), stale = FALSE`,
			station, c.CellLabel, []byte(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListEdgeCells returns catalog cells, newest-bindings-wins. station=="" returns
// every station's cells; otherwise just that station. Stale cells are included
// (callers decide whether to show them) — ordered station, then label.
func (db *DB) ListEdgeCells(station string) ([]EdgeCell, error) {
	q := `SELECT station, cell_label, bindings, first_seen, last_seen, stale FROM edge_cells`
	var args []any
	if station != "" {
		q += ` WHERE station = $1`
		args = append(args, station)
	}
	q += ` ORDER BY station, cell_label`

	rows, err := db.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []EdgeCell{}
	for rows.Next() {
		var c EdgeCell
		var bindings []byte
		if err := rows.Scan(&c.Station, &c.CellLabel, &bindings, &c.FirstSeen, &c.LastSeen, &c.Stale); err != nil {
			return nil, err
		}
		c.Bindings = json.RawMessage(bindings)
		out = append(out, c)
	}
	return out, rows.Err()
}
