package inventory

import (
	"database/sql"
	"fmt"
)

// Holder is one bin or active lineside pile of a payload whose count is not 0:
// what it is ("BIN-7", "STN-1 · ALN_001") and the count it holds, in the
// payload's count (parts for one line, kits for a kit).
type Holder struct {
	What  string `json:"what"`
	Count int    `json:"count"`
	Pile  bool   `json:"pile,omitempty"`
}

// CountHolders lists the bins and active lineside piles of payload code whose
// count is not 0, bins first, at most limit of each, with the totals. The
// payload guards read it (service PayloadService: a ratio change warns, a
// one-line/kit change is refused, while any stand). Four statements, on a
// payload save only.
func CountHolders(db *sql.DB, code string, limit int) (holders []Holder, bins, piles int, err error) {
	if err = db.QueryRow(`SELECT count(*) FROM bins WHERE payload_code = $1 AND uop_remaining <> 0`, code).Scan(&bins); err != nil {
		return nil, 0, 0, fmt.Errorf("count the bins of %s: %w", code, err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM lineside_buckets WHERE payload_code = $1 AND state = 'active' AND qty <> 0`,
		code).Scan(&piles); err != nil {
		return nil, 0, 0, fmt.Errorf("count the piles of %s: %w", code, err)
	}
	rows, err := db.Query(`SELECT label, uop_remaining FROM bins
		WHERE payload_code = $1 AND uop_remaining <> 0 ORDER BY label LIMIT $2`, code, limit)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("list the bins of %s: %w", code, err)
	}
	for rows.Next() {
		var h Holder
		if err = rows.Scan(&h.What, &h.Count); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		holders = append(holders, h)
	}
	rows.Close()
	prows, err := db.Query(`SELECT station_uid || ' · ' || core_node_name, qty FROM lineside_buckets
		WHERE payload_code = $1 AND state = 'active' AND qty <> 0 ORDER BY station_uid, core_node_name LIMIT $2`, code, limit)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("list the piles of %s: %w", code, err)
	}
	defer prows.Close()
	for prows.Next() {
		h := Holder{Pile: true}
		if err = prows.Scan(&h.What, &h.Count); err != nil {
			return nil, 0, 0, err
		}
		holders = append(holders, h)
	}
	return holders, bins, piles, prows.Err()
}
