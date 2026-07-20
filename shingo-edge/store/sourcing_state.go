package store

// Edge persistent cache of Core's sourceability verdict per (process, style),
// pushed down on SubjectSourcingState. Persistent so an HMI reload or an Edge
// reboot during a Core partition still shows the last-known changeover picture
// with no Core round-trip at click time. A full snapshot replaces the whole
// cache (stale styles drop); a change delta upserts the styles that moved.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"shingo/protocol"
)

// ReplaceSourcingState fully replaces the cache (full snapshot: delete all,
// re-insert) atomically, so (process, style) rows Core no longer reports drop
// out. On any error the tx rolls back and the last-known-good cache survives.
func (db *DB) ReplaceSourcingState(states []protocol.SourcingState) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM sourcing_state`); err != nil {
		return fmt.Errorf("clear sourcing_state: %w", err)
	}
	for _, s := range states {
		if err := upsertSourcingRow(tx, s); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertSourcingState applies a change delta: each listed (process, style) is
// inserted or updated in place, leaving untouched styles as they were.
func (db *DB) UpsertSourcingState(states []protocol.SourcingState) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range states {
		if err := upsertSourcingRow(tx, s); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func upsertSourcingRow(tx *sql.Tx, s protocol.SourcingState) error {
	missing, err := json.Marshal(nonEmptyStrings(s.Missing))
	if err != nil {
		return fmt.Errorf("marshal missing %s/%s: %w", s.ProcessID, s.StyleID, err)
	}
	atRisk, err := json.Marshal(nonEmptyAtRisk(s.AtRisk))
	if err != nil {
		return fmt.Errorf("marshal at_risk %s/%s: %w", s.ProcessID, s.StyleID, err)
	}
	computedAt := ""
	if !s.ComputedAt.IsZero() {
		computedAt = s.ComputedAt.UTC().Format(time.RFC3339)
	}
	_, err = tx.Exec(`
		INSERT INTO sourcing_state (process_id, style_id, status, missing, at_risk, reason, computed_at, synced_at)
		VALUES (?,?,?,?,?,?,?,datetime('now'))
		ON CONFLICT(process_id, style_id) DO UPDATE SET
			status=excluded.status, missing=excluded.missing, at_risk=excluded.at_risk,
			reason=excluded.reason, computed_at=excluded.computed_at, synced_at=datetime('now')`,
		s.ProcessID, s.StyleID, s.Status, string(missing), string(atRisk), s.Reason, computedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert sourcing_state %s/%s: %w", s.ProcessID, s.StyleID, err)
	}
	return nil
}

// ListSourcingState returns every cached verdict, ordered by (process, style).
func (db *DB) ListSourcingState() ([]protocol.SourcingState, error) {
	return db.querySourcingState(`SELECT process_id, style_id, status, missing, at_risk, reason, computed_at
		FROM sourcing_state ORDER BY process_id, style_id`)
}

// ListSourcingStateForProcess returns the verdicts for one process — the HMI
// changeover picker's source.
func (db *DB) ListSourcingStateForProcess(processID string) ([]protocol.SourcingState, error) {
	return db.querySourcingState(`SELECT process_id, style_id, status, missing, at_risk, reason, computed_at
		FROM sourcing_state WHERE process_id=? ORDER BY style_id`, processID)
}

func (db *DB) querySourcingState(query string, args ...any) ([]protocol.SourcingState, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sourcing_state: %w", err)
	}
	defer rows.Close()
	var out []protocol.SourcingState
	for rows.Next() {
		var (
			s                    protocol.SourcingState
			missing, atRisk, cat string
		)
		if err := rows.Scan(&s.ProcessID, &s.StyleID, &s.Status, &missing, &atRisk, &s.Reason, &cat); err != nil {
			return nil, fmt.Errorf("scan sourcing_state: %w", err)
		}
		_ = json.Unmarshal([]byte(missing), &s.Missing)
		_ = json.Unmarshal([]byte(atRisk), &s.AtRisk)
		if cat != "" {
			if t, err := time.Parse(time.RFC3339, cat); err == nil {
				s.ComputedAt = t
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// nonEmptyStrings normalizes a nil slice to an empty one so it marshals as [].
func nonEmptyStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonEmptyAtRisk(in []protocol.SourcingAtRisk) []protocol.SourcingAtRisk {
	if in == nil {
		return []protocol.SourcingAtRisk{}
	}
	return in
}
