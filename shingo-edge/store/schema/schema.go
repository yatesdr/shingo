// Package schema holds the Edge's schema: the generated baseline a new file is
// born on (baseline.sql), the generated snapshot of a born database
// (schema.snapshot.sql), and the reader of a schema's shape (shape.go).
//
// The baseline is the one home of the shape, and a numbered migration above
// it (store/migrations.go) the one way to change it.
//
// All helpers take *sql.DB so they're usable without the outer
// *store.DB type, keeping the dependency graph one-way:
// store -> store/schema, never the reverse.
package schema

import "database/sql"

// Column is one column of a table as pragma_table_info reports it: the name and
// the DECLARED type, in ordinal order.
type Column struct{ Name, Type string }

// Columns returns every column of the named table, in ordinal order. Empty for
// a table that does not exist,
// answering "no such thing" rather than erroring.
//
// It exists so one table can be kept a MIRROR of another without either column
// list being written down twice. style_node_claims_quarantine is that mirror:
// its shape has to follow style_node_claims across every vintage, and a
// hand-maintained copy of a 40-column list is a drift bug waiting for the next
// ALTER (nine of that table's columns arrive by ALTER already, so the copy would
// be wrong on the day it was written).
func Columns(db *sql.DB, tableName string) ([]Column, error) {
	rows, err := db.Query(`SELECT name, COALESCE(type, '') FROM pragma_table_info('` + tableName + `') ORDER BY cid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.Type); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
