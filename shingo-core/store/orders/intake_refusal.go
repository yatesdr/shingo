package orders

import (
	"database/sql"
	"errors"
	"time"

	"shingo/protocol/clock"
)

// IntakeRefusal is Core's record that it refused one leg of a pair at intake.
//
// ── WHY A REFUSAL NEEDS A ROW OF ITS OWN ──────────────────────────────────
//
// A refused request leaves no order row. The refusal is a reply to the Edge and
// nothing else, on purpose: a refusal must leave nothing behind for dispatch to
// act on. For a solo order that is the whole story. For one leg of a pair it is
// not, because the other leg names it — its sibling pointer points at a row that
// will never exist — and without a record Core cannot tell "refused" from "not
// received yet". The first is a failure with a reason; the second is a wait the
// partner's own intake ends. Read as the second, the surviving leg waited for
// ever.
//
// Written only for a refused request that names a sibling, which is the one
// population anything asks about. Read only by the pair rule, and only when the
// partner's row is absent: a row under the same uuid, if a later retry was
// accepted, outranks any refusal on record.
type IntakeRefusal struct {
	EdgeUUID  string
	StationID string
	ErrorCode string
	Detail    string
	RefusedAt time.Time
}

// RecordIntakeRefusal stores a refusal. A repeat refusal of the same uuid
// replaces the earlier one, so the partner's failure quotes the latest reason.
func RecordIntakeRefusal(db *sql.DB, edgeUUID, stationID, code, detail string) error {
	_, err := db.Exec(`
		INSERT INTO order_intake_refusals (edge_uuid, station_id, error_code, detail, refused_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (edge_uuid) DO UPDATE SET
			station_id = EXCLUDED.station_id,
			error_code = EXCLUDED.error_code,
			detail     = EXCLUDED.detail,
			refused_at = EXCLUDED.refused_at`,
		edgeUUID, stationID, code, detail, clock.Now().UTC())
	return err
}

// GetIntakeRefusal returns the refusal on record for edgeUUID, or nil when Core
// has not refused it.
func GetIntakeRefusal(db *sql.DB, edgeUUID string) (*IntakeRefusal, error) {
	r := &IntakeRefusal{}
	err := db.QueryRow(`
		SELECT edge_uuid, station_id, error_code, detail, refused_at
		FROM order_intake_refusals WHERE edge_uuid = $1`, edgeUUID).
		Scan(&r.EdgeUUID, &r.StationID, &r.ErrorCode, &r.Detail, &r.RefusedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}
