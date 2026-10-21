package store

import (
	"database/sql"
	"fmt"
	"strings"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// GetSetDownNode is the planner's destination read: the node an empty is set
// down on, by name (dot-notation as nodes.GetByDotName reads it), and in the
// same statement the height, in layers, an empty of payloadCode is wanted at
// there (bins.ToteWantSQL): nil wants none. The want rides the read the empty
// type decider already makes, so it costs no statement.
// Pins: TestToteWant_TheShape, TestToteWant_NoNumbersAddNoStatement.
func (db *DB) GetSetDownNode(name, payloadCode string) (*nodes.Node, *int, error) {
	match, args := `n.name=$1`, []any{name, payloadCode}
	if parent, child, dotted := strings.Cut(name, "."); dotted {
		match, args = `n.name=$1 AND n.parent_id IN (SELECT id FROM nodes WHERE name=$3)`,
			[]any{child, payloadCode, parent}
	}
	var want sql.NullInt64
	n, err := nodes.ScanNode(scanPlus{db.QueryRow(fmt.Sprintf(`SELECT %s, %s %s WHERE %s`,
		nodes.SelectCols, bins.ToteWantSQL(2), nodes.FromClause, match), args...), &want})
	if err != nil {
		return nil, nil, err
	}
	if !want.Valid {
		return n, nil, nil
	}
	layers := int(want.Int64)
	return n, &layers, nil
}

// scanPlus scans a row's leading columns into a caller's destinations and its
// last into extra, so a shared scanner can read a row with one more column.
type scanPlus struct {
	row   *sql.Row
	extra any
}

func (s scanPlus) Scan(dest ...any) error { return s.row.Scan(append(dest, s.extra)...) }
