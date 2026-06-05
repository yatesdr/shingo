package store

// loader_payload_thresholds — Edge persistence for the per-(loader,
// payload) UOP threshold the C-push monitor compares against at Core.
// See shingo/docs/uop-threshold-replenishment.md for the design
// overview.
//
//   - PK is (core_node_name, payload_code). core_node_name is the
//     canonical cross-system identifier already used by
//     style_node_claims, process_nodes, the protocol, and Core's
//     demand_registry. Multi-cell plants sharing a loader (a real
//     near-term scenario) end up with one threshold row per
//     (core_node_name, payload_code), not per-Edge variants.
//   - A row with replenish_uop_threshold = 0 is treated identically to
//     no row at all. Legacy bin-count fallback at Edge; Core never
//     monitors. Engineers can record `source = 'manual'` with a 0
//     value to mark "we considered this and chose 0" without engaging
//     the C-push path.
//   - SendClaimSync reads this table when assembling the ClaimSync
//     PayloadThresholds map so Core's demand_registry stays in sync.

import (
	"database/sql"
	"time"

	"shingoedge/store/internal/helpers"
)

// LoaderPayloadThreshold mirrors the loader_payload_thresholds table.
// safety_factor / lookback_days / threshold_calculated* are knobs for
// the unified calculator. ThresholdConfidence is the HIGH/MEDIUM/LOW
// label produced by the last calculate run (empty when no calculate
// has been run for this binding yet). OverriddenInputs is a comma-
// separated list of input field names the engineer overrode in the
// Calculate that produced the current threshold (empty when nothing
// was overridden or when source != 'calculated').
type LoaderPayloadThreshold struct {
	CoreNodeName          string
	PayloadCode           string
	ReplenishUOPThreshold int
	Source                string // legacy | manual | calculated
	SafetyFactor          float64
	LookbackDays          int
	ThresholdCalculated   int
	ThresholdCalculatedAt sql.NullString
	ThresholdConfidence   string // HIGH | MEDIUM | LOW | ''
	OverriddenInputs      string // comma-separated snake_case field names
	UpdatedAt             time.Time
	UpdatedBy             string
}

const loaderThresholdSelectCols = `
	core_node_name, payload_code, replenish_uop_threshold, source,
	safety_factor, lookback_days, threshold_calculated, threshold_calculated_at,
	threshold_confidence, overridden_inputs, updated_at, updated_by`

func scanLoaderPayloadThreshold(row interface{ Scan(...any) error }) (LoaderPayloadThreshold, error) {
	var t LoaderPayloadThreshold
	var updatedAt string
	err := row.Scan(
		&t.CoreNodeName, &t.PayloadCode, &t.ReplenishUOPThreshold, &t.Source,
		&t.SafetyFactor, &t.LookbackDays, &t.ThresholdCalculated, &t.ThresholdCalculatedAt,
		&t.ThresholdConfidence, &t.OverriddenInputs, &updatedAt, &t.UpdatedBy,
	)
	if err == nil {
		t.UpdatedAt = helpers.ScanTime(updatedAt)
	}
	return t, err
}

// ListLoaderPayloadThresholds returns every threshold row.
func (db *DB) ListLoaderPayloadThresholds() ([]LoaderPayloadThreshold, error) {
	rows, err := db.Query(`SELECT ` + loaderThresholdSelectCols + ` FROM loader_payload_thresholds ORDER BY core_node_name, payload_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LoaderPayloadThreshold
	for rows.Next() {
		t, err := scanLoaderPayloadThreshold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListLoaderPayloadThresholdsForLoader returns every threshold for a
// single loader — what the replenishment UI lists per loader.
func (db *DB) ListLoaderPayloadThresholdsForLoader(coreNodeName string) ([]LoaderPayloadThreshold, error) {
	rows, err := db.Query(`SELECT `+loaderThresholdSelectCols+` FROM loader_payload_thresholds WHERE core_node_name = ? ORDER BY payload_code`, coreNodeName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LoaderPayloadThreshold
	for rows.Next() {
		t, err := scanLoaderPayloadThreshold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetLoaderPayloadThreshold returns the single binding by composite
// key, or (nil, nil) when there's no row.
func (db *DB) GetLoaderPayloadThreshold(coreNodeName, payloadCode string) (*LoaderPayloadThreshold, error) {
	row := db.QueryRow(`SELECT `+loaderThresholdSelectCols+` FROM loader_payload_thresholds WHERE core_node_name = ? AND payload_code = ?`, coreNodeName, payloadCode)
	t, err := scanLoaderPayloadThreshold(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// UpsertLoaderPayloadThreshold writes a binding row. Only the
// engineer-editable fields are written; updated_at / updated_by
// track the change. Threshold value 0 is permitted and means
// "considered, opted out" — the row exists so the UI can show
// source='manual' without engaging the C-push path at Core.
func (db *DB) UpsertLoaderPayloadThreshold(t LoaderPayloadThreshold) error {
	_, err := db.Exec(`
		INSERT INTO loader_payload_thresholds (
			core_node_name, payload_code, replenish_uop_threshold, source,
			safety_factor, lookback_days, threshold_calculated, threshold_calculated_at,
			threshold_confidence, overridden_inputs, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), ?)
		ON CONFLICT(core_node_name, payload_code) DO UPDATE SET
			replenish_uop_threshold = excluded.replenish_uop_threshold,
			source                  = excluded.source,
			safety_factor           = excluded.safety_factor,
			lookback_days           = excluded.lookback_days,
			threshold_calculated    = excluded.threshold_calculated,
			threshold_calculated_at = excluded.threshold_calculated_at,
			threshold_confidence    = excluded.threshold_confidence,
			overridden_inputs       = excluded.overridden_inputs,
			updated_at              = datetime('now'),
			updated_by              = excluded.updated_by`,
		t.CoreNodeName, t.PayloadCode, t.ReplenishUOPThreshold, t.Source,
		t.SafetyFactor, t.LookbackDays, t.ThresholdCalculated, t.ThresholdCalculatedAt,
		t.ThresholdConfidence, t.OverriddenInputs, t.UpdatedBy,
	)
	return err
}

// DeleteLoaderPayloadThreshold removes a binding row. Returning to the
// "no row" state is semantically equivalent to threshold=0 — Edge falls
// back to legacy bin-count for the pair.
func (db *DB) DeleteLoaderPayloadThreshold(coreNodeName, payloadCode string) error {
	_, err := db.Exec(`DELETE FROM loader_payload_thresholds WHERE core_node_name = ? AND payload_code = ?`, coreNodeName, payloadCode)
	return err
}

// ThresholdsByPayloadForLoader returns a map[payload_code]threshold for
// the given loader — convenience for ClaimSync assembly. Zero-valued
// rows are intentionally included; the SendClaimSync caller is
// responsible for omitting zeros from the wire (opt-in default).
func (db *DB) ThresholdsByPayloadForLoader(coreNodeName string) (map[string]int, error) {
	rows, err := db.Query(`SELECT payload_code, replenish_uop_threshold FROM loader_payload_thresholds WHERE core_node_name = ?`, coreNodeName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var p string
		var v int
		if err := rows.Scan(&p, &v); err != nil {
			return nil, err
		}
		out[p] = v
	}
	return out, rows.Err()
}
