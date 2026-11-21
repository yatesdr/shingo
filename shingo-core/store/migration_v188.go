package store

import (
	"database/sql"
	"fmt"
	"strings"

	"shingocore/store/schema"
)

// v188 — cms_transactions.notes dropped, and the edge_registry comments that
// said Core mints the uid corrected.
//
// cms_transactions.notes had no writer (store/cms CreateInTx never inserted
// it) and one reader, the Notes column of Diagnostics' CMS table, which goes
// with it. Rows written before the writer stopped may carry a note: down
// brings the column back with its default, not those values (the pre-deploy
// read counts them: SELECT count(*) FROM cms_transactions WHERE notes <> '').
//
// edge_registry's table comment said "Core mints the uid rather than the Pi",
// and its station_uid column "Core mints it at enrollment". An Edge mints its
// own uid at first boot (shingoedge ensureIdentity, config.NewStationUID); Core
// mints one only when a person enrolls a station (registry.Enroll). Up applies
// the true text; down puts v185's back (store/schema/comments.sql).
//
// One transaction (the runner's): the table's lock, the drop, the comments.
// Idempotent (DROP COLUMN IF EXISTS; a comment set twice is set).

const (
	v188RegistryComment   = `COMMENT ON TABLE edge_registry IS 'The identity table, one row per Edge, keyed by station_uid (total and never blank since v185): what Core correlates all history to, and what an envelope''s Address.Station carries. display_name is the operator''s label, set on Core. The uid is minted once, by the Edge at its first boot (stn- and 16 hex) or by Core when a person enrolls a station, and never changes: replacement hardware keeps a station''s uid by restoring the old Edge''s backup (shingoedge --restore) or by an engineer setting it in the Edge''s yaml, so history does not move because identity did not move (v188). Not the BIGSERIAL id either: it is real and it is read (registry.List scans it), but it is allocated by an INSERT ... ON CONFLICT driven by an Edge-supplied string, a consequence of the name, not independent of it. bound_hostname and conflict_* are the duplicate-edge detector (v64); bound_instance, prev_instance and bound_at are the binding lease (v66), for the case the hostname detector is blind to: two Pis flashed from one SD image share a hostname. See registry.Register for why recurrence, an instance coming back after being displaced, is the signature a restarting single Pi can never produce. The conflict_* columns are the alarm''s persisted state, not a derived view: the question has this ever happened here has to be answerable in SQL months later.'`
	v188StationUIDComment = `COMMENT ON COLUMN edge_registry.station_uid IS 'What Core correlates all history to. Minted once, by the Edge at its first boot or by Core when a person enrolls a station, and it never changes (v188). Opaque on purpose: the moment an identifier is legible it acquires an editor.'`
)

// v188Up is migration 188's apply function.
func v188Up(tx *sql.Tx) error {
	for _, q := range []string{
		`LOCK TABLE cms_transactions, edge_registry IN ACCESS EXCLUSIVE MODE`,
		`ALTER TABLE cms_transactions DROP COLUMN IF EXISTS notes`,
		v188RegistryComment,
		v188StationUIDComment,
	} {
		if err := migrationExec(tx, "v188", q); err != nil {
			return err
		}
	}
	return nil
}

// v188Verify is v188's post-condition: the column is gone. Catalog reads
// only; a comment is not a post-condition (a hand-edited one is the drift
// report's to name).
func v188Verify(q schema.Querier) bool {
	return schema.ColumnAbsent(q, "cms_transactions", "notes")
}

// v188Down brings cms_transactions.notes back as v185 declared it (its values
// come back as the default) and edge_registry's comments as v185 wrote them.
func v188Down(tx *sql.Tx) error {
	qs := []string{
		`LOCK TABLE cms_transactions, edge_registry IN ACCESS EXCLUSIVE MODE`,
		`ALTER TABLE cms_transactions ADD COLUMN notes text DEFAULT ''::text NOT NULL`,
	}
	for _, obj := range []string{"COMMENT ON TABLE edge_registry IS ", "COMMENT ON COLUMN edge_registry.station_uid IS "} {
		c, err := v185Comment(obj)
		if err != nil {
			return fmt.Errorf("v188 down: %w", err)
		}
		qs = append(qs, c)
	}
	for _, q := range qs {
		if err := migrationExec(tx, "v188 down", q); err != nil {
			return err
		}
	}
	return nil
}

// v185Comment is the statement of store/schema/comments.sql (the comments v185
// applies) that opens with prefix: the text a later migration's down puts back.
func v185Comment(prefix string) (string, error) {
	for _, line := range strings.Split(schema.Comments, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line, nil
		}
	}
	return "", fmt.Errorf("comments.sql has no statement opening %q", prefix)
}
