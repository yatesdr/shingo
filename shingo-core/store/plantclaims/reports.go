package plantclaims

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// reports.go — plant_claims_reports, one row per process: the station whose
// report the mirror last took for it and the digest that report carried. Core
// quotes the digests back on each station's heartbeat ack so the Edge can
// re-send a report Core lost, and drop a process Core still holds that the
// Edge no longer has. The digest is stored as it arrived, never recomputed
// from the mirror rows, which are a projection of the report.

// RecordReport writes a process's report row and returns the station that held
// it before ("" when none), in one statement: the CTE reads the row as it was
// before the upsert, so a second station taking a process over is seen without
// a separate read.
func RecordReport(db *sql.DB, processID, stationID, digest string, at time.Time) (string, error) {
	var prev string
	err := db.QueryRow(`
		WITH prev AS (SELECT station_id FROM plant_claims_reports WHERE process_id = $1)
		INSERT INTO plant_claims_reports (process_id, station_id, digest, received_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (process_id) DO UPDATE
		   SET station_id = EXCLUDED.station_id, digest = EXCLUDED.digest, received_at = EXCLUDED.received_at
		RETURNING COALESCE((SELECT station_id FROM prev), '')`,
		processID, stationID, digest, at).Scan(&prev)
	if err != nil {
		return "", fmt.Errorf("plantclaims record report %s: %w", processID, err)
	}
	return prev, nil
}

// DeleteReport removes a process's report row and returns the station that
// held it ("" when there was none).
func DeleteReport(db *sql.DB, processID string) (string, error) {
	var prev string
	err := db.QueryRow(`DELETE FROM plant_claims_reports WHERE process_id = $1 RETURNING station_id`,
		processID).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("plantclaims delete report %s: %w", processID, err)
	}
	return prev, nil
}

// ReportDigests returns process → digest for every report row one station
// holds. Never nil on success: an empty map is "Core holds none".
//
// AND EVERY MIRRORED PROCESS WITH NO REPORT ROW, quoted with an empty digest:
// those whose mirror names this station, and those whose mirror names none (rows
// written before the station was recorded). The ack quoted report rows only, so
// a mirror with no report row was never quoted and never reconciled — a process
// deleted on its Edge kept its claims in Core for good (SPR: "Test Route",
// "Bin Unloader"). Quoted, the Edge answers it like any other name: it re-sends a
// process it has (the empty digest never matches) and sends an empty report for
// one it does not, which clears the mirror. A no-station process quoted to an
// Edge that does not own it is emptied and then re-sent by the Edge that does on
// its next heartbeat (it re-publishes any process whose digest Core lacks), so
// the cost of the guess is one heartbeat, never a lost mirror.
func ReportDigests(db *sql.DB, stationID string) (map[string]string, error) {
	rows, err := db.Query(`SELECT process_id, digest FROM plant_claims_reports WHERE station_id = $1
		UNION
		SELECT DISTINCT ps.process_id, '' FROM process_styles ps
		 WHERE (ps.station_id = $1 OR ps.station_id IS NULL)
		   AND NOT EXISTS (SELECT 1 FROM plant_claims_reports r WHERE r.process_id = ps.process_id)`, stationID)
	if err != nil {
		return nil, fmt.Errorf("plantclaims report digests %s: %w", stationID, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var process, digest string
		if err := rows.Scan(&process, &digest); err != nil {
			return nil, fmt.Errorf("plantclaims report digests %s: scan: %w", stationID, err)
		}
		out[process] = digest
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plantclaims report digests %s: %w", stationID, err)
	}
	return out, nil
}
