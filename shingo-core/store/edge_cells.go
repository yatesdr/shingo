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
	StationUID string          `json:"station"`
	CellLabel  string          `json:"cell_label"`
	Bindings   json.RawMessage `json:"bindings"`
	FirstSeen  time.Time       `json:"first_seen"`
	LastSeen   time.Time       `json:"last_seen"`
	Stale      bool            `json:"stale"`
}

// UpsertEdgeCells applies a station's whole catalog, and the digest it
// carried, in one transaction: mark every existing cell for the station stale,
// then upsert each cell in the catalog (refreshing bindings, last_seen and
// catalog_digest, and clearing stale). So every live cell of the station
// carries the digest of the catalog it last applied (EdgeCellsDigest), and an
// empty catalog leaves the station no live cell. Cells absent from the new
// catalog stay stale — never deleted, so a momentarily-incomplete catalog or a
// retired PLC keeps its history visible (the scenesync ghost lesson). The
// stale-first / clear-on-upsert ordering avoids binding a Go slice into a SQL
// array (pgx stdlib has no native []string array param).
func (db *DB) UpsertEdgeCells(station string, cells []EdgeCell, digest string) error {
	tx, err := db.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.Exec(`UPDATE edge_cells SET stale = TRUE WHERE station_uid = $1`, station); err != nil {
		return err
	}
	for _, c := range cells {
		b := c.Bindings
		if len(b) == 0 {
			b = json.RawMessage("[]")
		}
		if _, err := tx.Exec(`
			INSERT INTO edge_cells (station_uid, cell_label, bindings, last_seen, stale, catalog_digest)
			VALUES ($1, $2, $3, NOW(), FALSE, $4)
			ON CONFLICT (station_uid, cell_label) DO UPDATE
			   SET bindings = EXCLUDED.bindings, last_seen = NOW(), stale = FALSE, catalog_digest = EXCLUDED.catalog_digest`,
			station, c.CellLabel, []byte(b), digest); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EdgeCellsDigest is the digest of the catalog Core holds for a station: the
// one its live cells carry (UpsertEdgeCells writes it on every cell it makes
// live). held is false when the station has no live cell: Core holds no
// catalog for it, which is also what an empty catalog leaves. Live cells that
// carry different digests, or none (made live before v187, or by a catalog
// that carried no digest), read as held with digest "": it matches no Edge's,
// so the Edge sends its catalog, and the apply writes one digest on every
// live cell. One statement, on the station's index.
func (db *DB) EdgeCellsDigest(station string) (digest string, held bool, err error) {
	var n int
	var lo, hi string
	if err := db.DB.QueryRow(`SELECT count(*), COALESCE(min(catalog_digest), ''), COALESCE(max(catalog_digest), '')
		  FROM edge_cells WHERE station_uid = $1 AND NOT stale`, station).Scan(&n, &lo, &hi); err != nil {
		return "", false, err
	}
	if n == 0 {
		return "", false, nil
	}
	if lo != hi {
		return "", true, nil
	}
	return lo, true, nil
}

// ListEdgeCells returns catalog cells, newest-bindings-wins. station=="" returns
// every station's cells; otherwise just that station. Stale cells are included
// (callers decide whether to show them) — ordered station, then label.
func (db *DB) ListEdgeCells(station string) ([]EdgeCell, error) {
	q := `SELECT station_uid, cell_label, bindings, first_seen, last_seen, stale FROM edge_cells`
	var args []any
	if station != "" {
		q += ` WHERE station_uid = $1`
		args = append(args, station)
	}
	q += ` ORDER BY station_uid, cell_label`

	rows, err := db.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []EdgeCell{}
	for rows.Next() {
		var c EdgeCell
		var bindings []byte
		if err := rows.Scan(&c.StationUID, &c.CellLabel, &bindings, &c.FirstSeen, &c.LastSeen, &c.Stale); err != nil {
			return nil, err
		}
		c.Bindings = json.RawMessage(bindings)
		out = append(out, c)
	}
	return out, rows.Err()
}
