package store

import (
	"database/sql"

	"shingocore/store/schema"
)

// v187 — edge_cells.catalog_digest: the digest of the cell catalog that made a
// row live, kept with the rows (the info-flow build's B4, edge.cells).
//
// An Edge sends its whole cell catalog as authoritative, with the catalog's
// digest; Core marks every cell of the station stale and makes each listed one
// live, all in one transaction (DB.UpsertEdgeCells), so every live row of a
// station carries the digest of the catalog it last applied. That is the
// digest Core quotes back (DB.EdgeCellsDigest): no live row is "Core holds no
// catalog", and an empty catalog leaves none.
//
// Rows made live before v187 carry the default '': a station with such rows
// reads as holding a catalog whose digest matches no Edge's, so its Edge sends
// the catalog once and the apply rewrites the rows with one digest.
//
// One transaction (the runner's): the table's lock, the column, its comment.
// Idempotent (ADD COLUMN IF NOT EXISTS). Down drops the column.

const v187Comment = `COMMENT ON COLUMN edge_cells.catalog_digest IS 'The digest the cell catalog that made this row live carried (v187): an apply marks every cell of the station stale and makes each listed one live in one transaction, so every live row of a station holds the digest of the catalog it last applied, which Core quotes back to that Edge. No live row is no catalog held. Empty: made live before v187, or by a catalog that carried no digest; it matches no Edge''s, so the Edge re-sends. A stale row keeps the digest it was last live under; nothing reads it.'`

// v187Up is migration 187's apply function.
func v187Up(tx *sql.Tx) error {
	for _, q := range []string{
		`LOCK TABLE edge_cells IN ACCESS EXCLUSIVE MODE`,
		`ALTER TABLE edge_cells ADD COLUMN IF NOT EXISTS catalog_digest text DEFAULT ''::text NOT NULL`,
		v187Comment,
	} {
		if err := migrationExec(tx, "v187", q); err != nil {
			return err
		}
	}
	return nil
}

// v187Verify is v187's post-condition: the column is there. Catalog reads
// only.
func v187Verify(q schema.Querier) bool {
	return schema.ColumnExists(q, "edge_cells", "catalog_digest")
}

// v187Down drops the column; its values go with it (the next catalog an Edge
// sends after up again writes them back).
func v187Down(tx *sql.Tx) error {
	for _, q := range []string{
		`LOCK TABLE edge_cells IN ACCESS EXCLUSIVE MODE`,
		`ALTER TABLE edge_cells DROP COLUMN catalog_digest`,
	} {
		if err := migrationExec(tx, "v187 down", q); err != nil {
			return err
		}
	}
	return nil
}
