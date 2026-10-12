package store

import (
	"database/sql"
	"encoding/json"

	"fmt"
	"time"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
)

// demand_forecast.go — the forecaster's reads and writes on the demand record
// (D2, DESIGN-D2 §2–§4).
//
// A FORECAST IS THE RECORD OF A PLACE BEFORE ITS DEMAND OPENS: opened_at and
// closed_at both NULL (helpers.DemandForecastSQL). The forecaster births one per
// place with no unsettled record, keeps its plan and projection current on the
// two-minute pass, and closes it when it will not open. Opening is the record
// changing tense at the door (OpenCoreEpisode's conflict arm, SupersedeOpenEpisode's
// re-key), never a forecaster write. The forecast holds nothing: its plan is a
// belief, re-checked by predicate (PlannedPickStands) and never a reservation.

// ListUnsettledDemandRecords is every unsettled threshold or cell record, forecast
// or open, with the forecaster's columns. One read a pass, the forecaster's only
// read of the table: the one-unsettled-row-per-key index keeps it to one row per
// place.
func (db *DB) ListUnsettledDemandRecords() ([]domain.ForecastRecord, error) {
	rows, err := db.Query(`SELECT origin_id, episode_key, kind, ` + helpers.DemandOpenSQL("") + `,
		       plan, plan_verdict, need_at, predicted_need_at
		FROM demand_origins
		WHERE ` + helpers.DemandUnsettledSQL("") + ` AND kind IN ('threshold', 'cell')`)
	if err != nil {
		return nil, fmt.Errorf("list unsettled demand records: %w", err)
	}
	defer rows.Close()
	var out []domain.ForecastRecord
	for rows.Next() {
		var r domain.ForecastRecord
		var plan string
		var needAt, predicted sql.NullTime
		if err := rows.Scan(&r.OriginID, &r.EpisodeKey, &r.Kind, &r.Open, &plan, &r.PlanVerdict, &needAt, &predicted); err != nil {
			return nil, fmt.Errorf("scan unsettled demand record: %w", err)
		}
		r.Plan = unmarshalPlannedPick(plan)
		if needAt.Valid {
			t := needAt.Time
			r.NeedAt = &t
		}
		if predicted.Valid {
			t := predicted.Time
			r.PredictedNeedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BirthForecast writes a place's forecast record: the place's first projection is
// both need_at and predicted_need_at, and the second never moves. revision 0, so
// an Edge's first message for the place (revision 1) wins every Edge-authored
// column when SupersedeOpenEpisode re-keys the forecast to its origin_id. A place
// that already has an unsettled record (an open raced in) writes nothing.
func (db *DB) BirthForecast(f domain.ForecastBirth) (bool, error) {
	res, err := db.Exec(`
		INSERT INTO demand_origins (
		    origin_id, revision, episode_key, kind, direction, station_id, process_id,
		    core_node_name, payload_code, threshold, opened_at,
		    plan, plan_verdict, need_at, predicted_need_at, provenance
		) VALUES ($1, 0, $2, $3, $4, $5, $6, $7, $8, $9, NULL, $10, $11, $12, $12, 'arithmetic')
		ON CONFLICT (episode_key) WHERE `+helpers.DemandUnsettledSQL("")+` DO NOTHING`,
		f.OriginID, f.EpisodeKey, f.Kind, string(f.Direction), f.StationID, f.ProcessID,
		f.CoreNodeName, f.PayloadCode, f.Threshold,
		marshalPlannedPick(f.Plan), f.PlanVerdict, f.NeedAt)
	if err != nil {
		return false, fmt.Errorf("birth forecast %s: %w", f.EpisodeKey, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateForecast writes a forecast's plan, verdict and projection. Only a
// forecast: a record that opened since the pass read it is the door's now.
func (db *DB) UpdateForecast(originID string, plan *domain.PlannedPick, verdict string, needAt *time.Time) error {
	if _, err := db.Exec(`UPDATE demand_origins SET plan = $2, plan_verdict = $3, need_at = $4
		WHERE origin_id = $1 AND `+helpers.DemandForecastSQL(""),
		originID, marshalPlannedPick(plan), verdict, needAt); err != nil {
		return fmt.Errorf("update forecast %s: %w", originID, err)
	}
	return nil
}

// CloseForecast settles a forecast that will not open, with the forecaster's
// reason (domain.CloseReasonExpired, …RecoveredUnattributed, …Withdrawn) and
// closed_by sweep. Only a forecast: a record that opened is closed by its demand.
func (db *DB) CloseForecast(originID, reason, closedBy string, at time.Time) (bool, error) {
	res, err := db.Exec(`UPDATE demand_origins SET closed_at = $2, close_reason = $3, closed_by = $4
		WHERE origin_id = $1 AND `+helpers.DemandForecastSQL(""),
		originID, at, reason, closedBy)
	if err != nil {
		return false, fmt.Errorf("close forecast %s: %w", originID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// marshalPlannedPick is orders.plan's encoding: domain.PlannedPick JSON, ” for none.
func marshalPlannedPick(p *domain.PlannedPick) string {
	if p == nil {
		return ""
	}
	data, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(data)
}

func unmarshalPlannedPick(s string) *domain.PlannedPick {
	if s == "" {
		return nil
	}
	var p domain.PlannedPick
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil
	}
	return &p
}

// ListForecasts is every forecast, soonest need first (unprojectable last): the
// ahead view on the sourcing page. One read per page load.
func (db *DB) ListForecasts() ([]domain.ForecastView, error) {
	rows, err := db.Query(`SELECT episode_key, kind, station_id, core_node_name, payload_code,
		       plan, plan_verdict, need_at, predicted_need_at
		FROM demand_origins
		WHERE ` + helpers.DemandForecastSQL("") + `
		ORDER BY need_at NULLS LAST, episode_key`)
	if err != nil {
		return nil, fmt.Errorf("list forecasts: %w", err)
	}
	defer rows.Close()
	var out []domain.ForecastView
	for rows.Next() {
		var v domain.ForecastView
		var plan string
		var needAt, predicted sql.NullTime
		if err := rows.Scan(&v.EpisodeKey, &v.Kind, &v.StationID, &v.CoreNodeName, &v.PayloadCode,
			&plan, &v.PlanVerdict, &needAt, &predicted); err != nil {
			return nil, fmt.Errorf("scan forecast: %w", err)
		}
		v.Plan = unmarshalPlannedPick(plan)
		if needAt.Valid {
			t := needAt.Time
			v.NeedAt = &t
		}
		if predicted.Valid {
			t := predicted.Time
			v.PredictedNeedAt = &t
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ForecastScore is the forecast's record against what happened (D2.3): every
// record the forecaster birthed (provenance <> ”) that has since opened or
// been closed by it, since a moment. One read.
func (db *DB) ForecastScore(since time.Time) (domain.ForecastScore, error) {
	var s domain.ForecastScore
	var lateness sql.NullFloat64
	opened, never := helpers.DemandOpenedSQL(""), helpers.DemandNeverOpenedSQL("")
	recent := ` AND COALESCE(opened_at, closed_at) >= $1`
	err := db.QueryRow(`SELECT
		    COUNT(*),
		    COUNT(*) FILTER (WHERE `+helpers.DemandForecastSQL("")+`),
		    COUNT(*) FILTER (WHERE `+opened+recent+`),
		    COUNT(*) FILTER (WHERE `+opened+recent+` AND predicted_need_at IS NOT NULL),
		    COUNT(*) FILTER (WHERE `+never+recent+` AND close_reason = $2),
		    COUNT(*) FILTER (WHERE `+never+recent+` AND close_reason = $3),
		    COUNT(*) FILTER (WHERE `+never+recent+` AND close_reason = $4),
		    percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM opened_at - predicted_need_at))
		        FILTER (WHERE `+opened+recent+` AND predicted_need_at IS NOT NULL)
		FROM demand_origins
		WHERE provenance <> ''`,
		since, domain.CloseReasonExpired, domain.CloseReasonRecoveredUnattributed, domain.CloseReasonWithdrawn).
		Scan(&s.Born, &s.Forecasting, &s.Opened, &s.OpenedProjected, &s.Expired, &s.Recovered, &s.Withdrawn, &lateness)
	if err != nil {
		return s, fmt.Errorf("forecast score: %w", err)
	}
	if lateness.Valid {
		d := time.Duration(lateness.Float64 * float64(time.Second))
		s.MedianLateness = &d
	}
	return s, nil
}
