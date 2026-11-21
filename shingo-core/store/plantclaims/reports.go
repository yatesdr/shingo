package plantclaims

import (
	"database/sql"
	"fmt"
	"time"
)

// reports.go — plant_claims_reports, one row per (station, process): the
// digest of the last report each station sent for the process. Core quotes a
// station its own rows back on its heartbeat ack, so the Edge can re-send a
// report Core lost and drop a process Core still holds that the Edge no longer
// has. The digest is stored as it arrived, never recomputed from the mirror
// rows, which are a projection of the report.
//
// The mirror (process_styles, style_claims) is keyed by process alone and holds
// the last writer's report. A process two stations report has a row for each
// here, so each is quoted its own digest and neither re-sends, while the mirror
// holds whichever wrote last (TestScenario_TwoStationsOneProcess_EachSendsOnce);
// SharedReports names the stations the Edges page flags.

// RecordReport writes one station's report row for a process.
func RecordReport(db *sql.DB, processID, stationUID, digest string, at time.Time) error {
	if _, err := db.Exec(`
		INSERT INTO plant_claims_reports (station_uid, process_id, digest, received_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (station_uid, process_id) DO UPDATE
		   SET digest = EXCLUDED.digest, received_at = EXCLUDED.received_at`,
		stationUID, processID, digest, at); err != nil {
		return fmt.Errorf("plantclaims record report %s from %s: %w", processID, stationUID, err)
	}
	return nil
}

// DeleteReport removes every station's report row for a process and returns
// those stations, sorted. It follows an empty report, which replaced the
// process's mirror whole (ReplaceProcessFromStation): no row may go on quoting
// a digest the mirror no longer holds, so every other station that still has
// the process is no longer quoted it and re-sends it.
func DeleteReport(db *sql.DB, processID string) ([]string, error) {
	rows, err := db.Query(`
		WITH gone AS (DELETE FROM plant_claims_reports WHERE process_id = $1 RETURNING station_uid)
		SELECT station_uid FROM gone ORDER BY station_uid`, processID)
	if err != nil {
		return nil, fmt.Errorf("plantclaims delete report %s: %w", processID, err)
	}
	defer rows.Close()
	var gone []string
	for rows.Next() {
		var station string
		if err := rows.Scan(&station); err != nil {
			return nil, fmt.Errorf("plantclaims delete report %s: scan: %w", processID, err)
		}
		gone = append(gone, station)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plantclaims delete report %s: %w", processID, err)
	}
	return gone, nil
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
	rows, err := db.Query(`SELECT process_id, digest FROM plant_claims_reports WHERE station_uid = $1
		UNION
		SELECT DISTINCT ps.process_id, '' FROM process_styles ps
		 WHERE (ps.station_uid = $1 OR ps.station_uid IS NULL)
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

// SharedReports returns, for each process this station holds a report row for
// that another station holds one for too, every station that holds one,
// sorted. Never nil on success: an empty map is "no process shared".
func SharedReports(db *sql.DB, stationUID string) (map[string][]string, error) {
	rows, err := db.Query(`SELECT o.process_id, o.station_uid FROM plant_claims_reports o
		 WHERE o.process_id IN (SELECT m.process_id FROM plant_claims_reports m
		                         JOIN plant_claims_reports x ON x.process_id = m.process_id AND x.station_uid <> m.station_uid
		                        WHERE m.station_uid = $1)
		 ORDER BY o.process_id, o.station_uid`, stationUID)
	if err != nil {
		return nil, fmt.Errorf("plantclaims shared reports %s: %w", stationUID, err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var process, station string
		if err := rows.Scan(&process, &station); err != nil {
			return nil, fmt.Errorf("plantclaims shared reports %s: scan: %w", stationUID, err)
		}
		out[process] = append(out[process], station)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plantclaims shared reports %s: %w", stationUID, err)
	}
	return out, nil
}
