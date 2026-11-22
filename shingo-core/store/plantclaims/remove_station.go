package plantclaims

import (
	"database/sql"
	"fmt"
	"slices"
)

// RemoveStation clears what the mirror holds of one station, on tx: the styles,
// claims and positions of every process whose mirror names it, and the report
// rows Core quotes it. It returns those processes, sorted.
//
// A process it clears loses every station's report row with its mirror, by
// DeleteReport's rule: no row may go on quoting a digest the mirror no longer
// holds, so another station that still has the process re-sends it. A process
// another station reported last is that station's in the mirror and stays.
// Four statements.
func RemoveStation(tx *sql.Tx, station string) ([]string, error) {
	processes, err := deleteStyles(tx, station)
	if err != nil {
		return nil, fmt.Errorf("plantclaims remove %s: styles: %w", station, err)
	}
	if _, err := tx.Exec(`DELETE FROM style_claims WHERE process_id = ANY($1::text[])`, processes); err != nil {
		return nil, fmt.Errorf("plantclaims remove %s: claims: %w", station, err)
	}
	if _, err := tx.Exec(`DELETE FROM process_positions WHERE process_id = ANY($1::text[])`, processes); err != nil {
		return nil, fmt.Errorf("plantclaims remove %s: positions: %w", station, err)
	}
	if _, err := tx.Exec(`DELETE FROM plant_claims_reports WHERE station_uid = $1 OR process_id = ANY($2::text[])`,
		station, processes); err != nil {
		return nil, fmt.Errorf("plantclaims remove %s: reports: %w", station, err)
	}
	return processes, nil
}

// deleteStyles deletes the styles whose process one station reported last and
// returns those processes, sorted, each once; never nil on success.
func deleteStyles(tx *sql.Tx, station string) ([]string, error) {
	rows, err := tx.Query(`DELETE FROM process_styles WHERE station_uid = $1 RETURNING process_id`, station)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var process string
		if err := rows.Scan(&process); err != nil {
			return nil, err
		}
		out = append(out, process)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
