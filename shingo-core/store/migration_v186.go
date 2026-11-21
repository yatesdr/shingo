package store

import (
	"database/sql"
	"fmt"
	"log"

	"shingocore/store/schema"
)

// v186 — plant_claims_reports keyed by (station_uid, process_id), not by
// process alone (the info-flow build's C5, two stations on one process).
//
// Until v186 a process had one report row, the last writer's, and Core quoted
// a station only the rows it held: two stations reporting one process each took
// the row from the other, so each was quoted nothing for it and re-sent every 2
// minutes, forever. Keyed by both, each station is quoted its own digest and
// stops; the mirror (process_styles, style_claims) stays keyed by process and
// keeps the last writer's report (store/plantclaims reports.go).
//
// Up holds every row: under the old key no two rows share a process, so none
// collides under the new one. Down cannot: it keeps one row per process, the
// one of the station the mirror names (the last writer, which is what v185's
// row meant), else the newest, and deletes and logs the rest. A station whose
// row down deletes is not quoted that process by the v0.18.0 Core and re-sends
// it, which is that release's own two-station behaviour.
//
// One transaction (the runner's): the table's lock first, so a reader holding
// it makes the migration fail on lock_timeout before anything changes, as v185
// does; then the key, then the table's comment. Idempotent: the key is swapped
// only while it is not the new one.

const (
	v186Key    = "PRIMARY KEY (station_uid, process_id)"
	v186OldKey = "PRIMARY KEY (process_id)"

	v186Comment    = `COMMENT ON TABLE plant_claims_reports IS 'One row per station and process: the digest of the last plant-claims report that station sent for the process (v143; keyed by station and process since v186). Each station is quoted its own rows on its heartbeat ack, so two stations reporting one process each stop re-sending; the mirror (process_styles, style_claims) holds the last writer''s report. Two rows for one process are the reported-by-both flag on the Edges page. An empty report deletes every row of its process, with the mirror.'`
	v186OldComment = `COMMENT ON TABLE plant_claims_reports IS 'Per process, the station whose plant-claims report the mirror holds and the digest it carried (v143); read back onto each station''s heartbeat ack.'`
)

// migrationExec runs one step of a migration, naming it in the error.
func migrationExec(tx *sql.Tx, step, q string) error {
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	return nil
}

// v186Up is migration 186's apply function.
func v186Up(tx *sql.Tx) error {
	if err := migrationExec(tx, "v186 lock", `LOCK TABLE plant_claims_reports IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	if schema.ConstraintDef(tx, "plant_claims_reports", "plant_claims_reports_pkey") != v186Key {
		for _, q := range []string{
			`ALTER TABLE plant_claims_reports DROP CONSTRAINT IF EXISTS plant_claims_reports_pkey`,
			`ALTER TABLE plant_claims_reports ADD CONSTRAINT plant_claims_reports_pkey ` + v186Key,
		} {
			if err := migrationExec(tx, "v186 key", q); err != nil {
				return err
			}
		}
	}
	return migrationExec(tx, "v186 comment", v186Comment)
}

// v186Verify is v186's post-condition: the table is keyed by station and
// process. Catalog reads only.
func v186Verify(q schema.Querier) bool {
	return schema.ConstraintDef(q, "plant_claims_reports", "plant_claims_reports_pkey") == v186Key
}

// v186Down puts v185's key back. It keeps, per process, the row of the station
// the mirror names, else the newest (then the greatest station uid, so the
// choice is total), and deletes and logs every other.
func v186Down(tx *sql.Tx) error {
	if err := migrationExec(tx, "v186 down lock", `LOCK TABLE plant_claims_reports IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	rows, err := tx.Query(`
		WITH ranked AS (
			SELECT r.station_uid, r.process_id, row_number() OVER (
				PARTITION BY r.process_id
				ORDER BY (r.station_uid = (SELECT max(ps.station_uid) FROM process_styles ps WHERE ps.process_id = r.process_id)) IS TRUE DESC,
				         r.received_at DESC, r.station_uid DESC) AS n
			  FROM plant_claims_reports r),
		gone AS (
			DELETE FROM plant_claims_reports p USING ranked k
			 WHERE p.station_uid = k.station_uid AND p.process_id = k.process_id AND k.n > 1
			RETURNING p.process_id, p.station_uid)
		SELECT process_id, station_uid FROM gone ORDER BY process_id, station_uid`)
	if err != nil {
		return fmt.Errorf("v186 down: keep one report row per process: %w", err)
	}
	for rows.Next() {
		var process, station string
		if err := rows.Scan(&process, &station); err != nil {
			rows.Close()
			return err
		}
		log.Printf("v186 down: deletes plant_claims_reports row (%s, %s): v185 keeps one row per process, the mirror's station's", station, process)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, q := range []string{
		`ALTER TABLE plant_claims_reports DROP CONSTRAINT plant_claims_reports_pkey`,
		`ALTER TABLE plant_claims_reports ADD CONSTRAINT plant_claims_reports_pkey ` + v186OldKey,
		v186OldComment,
	} {
		if err := migrationExec(tx, "v186 down", q); err != nil {
			return err
		}
	}
	return nil
}
