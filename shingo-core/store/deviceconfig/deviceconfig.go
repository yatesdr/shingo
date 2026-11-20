// Package deviceconfigstore persists the Device Config tab's pulls
// (migration v181): one append-only row per robot per pull.
//
// GRAIN: robot per pull, forever. The tab reads the latest row per robot;
// v3's between-pull diff reads the history. params is the flattened leaf
// set the matrix reads — a tab open never re-parses 14 × 330 KB — and
// config is the SANITIZED whole model (credential values are fingerprints
// before either column is written; the pull handler runs deviceconfig.
// Sanitize first). model_md5 is the hash of the RAW bytes at pull time,
// kept as provenance: measured live 2026-11-19 it matches no robot's file
// on the wire, so it is carried for the record, not as a staleness signal.
//
// Pure SQL on *sql.DB; the read side reaches it through
// store/deviceconfig.go (the robothours pattern).
package deviceconfigstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Pull is one stored pull. Params is the JSONB leaf set verbatim; Config is
// deliberately NOT loaded by LatestPulls — the matrix never needs the whole
// model, and not selecting it keeps a tab open off the big column (v3 loads
// it explicitly).
type Pull struct {
	Vehicle  string
	PulledAt time.Time
	ModelMD5 string
	Params   json.RawMessage
}

// SavePull appends one row. Append-only by design: no upsert, no conflict
// target — history is the point. PulledAt is the caller's stamp verbatim
// (both callers set it; a default here would silently date a pull at the
// store's clock instead of the pull's).
func SavePull(ctx context.Context, db *sql.DB, p Pull, config json.RawMessage) error {
	_, err := db.ExecContext(ctx, `INSERT INTO robot_deviceconfig_pulls
			(vehicle_id, pulled_at, model_md5, params, config)
		VALUES ($1, $2, $3, $4, $5)`,
		p.Vehicle, p.PulledAt.UTC(), p.ModelMD5, []byte(p.Params), []byte(config))
	if err != nil {
		return fmt.Errorf("deviceconfig pull save: %w", err)
	}
	return nil
}

// LatestPulls reads the newest row per robot, ordered by vehicle. The
// DISTINCT ON keeps this one index-backed statement rather than N queries
// (SYNTH-round1: latest-row-per-robot reads are one DISTINCT ON, always).
func LatestPulls(ctx context.Context, db *sql.DB) ([]Pull, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT ON (vehicle_id)
			vehicle_id, pulled_at, model_md5, params
		FROM robot_deviceconfig_pulls
		ORDER BY vehicle_id, pulled_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("deviceconfig latest pulls: %w", err)
	}
	defer rows.Close()
	var out []Pull
	for rows.Next() {
		var p Pull
		if err := rows.Scan(&p.Vehicle, &p.PulledAt, &p.ModelMD5, &p.Params); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
