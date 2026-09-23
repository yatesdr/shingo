package store

// containment.go — the quality-containment state (v100): the per-payload
// containment flag and the per-bin hold marker, plus the reads dispatch and
// the UIs consume.
//
// OWNERSHIP MAP, because this state is read from three places:
//
//	payload_containment.active  — dispatch's divert reads it at resolution
//	                              time (placeForContainment); the Core
//	                              payloads screen writes it (a quality alert
//	                              on a part turns it on; clearing turns it
//	                              off, which RELEASES NOTHING — held bins are
//	                              individually verified).
//	bins.quality_hold           — the station's "Send to Quality Hold" sets
//	                              it (via the Core API) at the same moment
//	                              the containment move order is created; the
//	                              source finder refuses held bins so a held
//	                              bin cannot be quietly sourced back into
//	                              the ordinary flow while it waits.
//
// Every write stamps who and when. Containment nobody can attribute is
// containment nobody can defend — these are not optional columns.

import (
	"database/sql"
	"fmt"
	"time"
)

// PayloadContainmentRow is one payload's containment state as the UI reads it.
// The row persists after deactivation so the alert's history survives it.
type PayloadContainmentRow struct {
	PayloadCode   string     `json:"payload_code"`
	Active        bool       `json:"active"`
	Reason        string     `json:"reason"`
	ActivatedBy   string     `json:"activated_by"`
	ActivatedAt   *time.Time `json:"activated_at"`
	DeactivatedBy string     `json:"deactivated_by"`
	DeactivatedAt *time.Time `json:"deactivated_at"`
}

// SetPayloadContainment activates or deactivates containment for one payload,
// upserting the row and stamping the audit columns for THIS transition only —
// activating clears the deactivation pair and vice versa, so the row always
// names the most recent action on each side of its history.
func (db *DB) SetPayloadContainment(payloadCode, reason, by string, active bool) error {
	if payloadCode == "" {
		return fmt.Errorf("payload code is required")
	}
	now := time.Now().UTC()
	if active {
		_, err := db.DB.Exec(`INSERT INTO payload_containment
			(payload_code, active, reason, activated_by, activated_at, deactivated_by, deactivated_at, updated_at)
			VALUES ($1, TRUE, $2, $3, $4, '', NULL, $4)
			ON CONFLICT (payload_code) DO UPDATE SET
				active = TRUE, reason = $2, activated_by = $3, activated_at = $4,
				deactivated_by = '', deactivated_at = NULL, updated_at = $4`,
			payloadCode, reason, by, now)
		return err
	}
	_, err := db.DB.Exec(`INSERT INTO payload_containment
		(payload_code, active, reason, activated_by, activated_at, deactivated_by, deactivated_at, updated_at)
		VALUES ($1, FALSE, $2, '', NULL, $3, $4, $4)
		ON CONFLICT (payload_code) DO UPDATE SET
			active = FALSE, reason = $2, deactivated_by = $3, deactivated_at = $4, updated_at = $4`,
		payloadCode, reason, by, now)
	return err
}

// PayloadContainmentActive reports whether containment is ACTIVE for a payload.
// The dispatch divert reads this on the hot path; an unknown payload reads as
// not-contained, which is the standing default.
func (db *DB) PayloadContainmentActive(payloadCode string) (bool, error) {
	if payloadCode == "" {
		return false, nil
	}
	var active bool
	err := db.DB.QueryRow(
		`SELECT active FROM payload_containment WHERE payload_code = $1`, payloadCode,
	).Scan(&active)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return active, nil
}

// ListPayloadContainment returns every containment row, active first, then by
// most recently touched. The payloads screen's containment section and the
// containment admin page both read this.
func (db *DB) ListPayloadContainment() ([]PayloadContainmentRow, error) {
	rows, err := db.DB.Query(`SELECT payload_code, active, reason, activated_by, activated_at,
		deactivated_by, deactivated_at FROM payload_containment
		ORDER BY active DESC, updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PayloadContainmentRow
	for rows.Next() {
		var r PayloadContainmentRow
		var activatedAt, deactivatedAt sql.NullTime
		if err := rows.Scan(&r.PayloadCode, &r.Active, &r.Reason, &r.ActivatedBy, &activatedAt,
			&r.DeactivatedBy, &deactivatedAt); err != nil {
			return nil, err
		}
		if activatedAt.Valid {
			t := activatedAt.Time
			r.ActivatedAt = &t
		}
		if deactivatedAt.Valid {
			t := deactivatedAt.Time
			r.DeactivatedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetBinQualityHold sets or clears a bin's hold marker. `by` is station-level
// attribution (the same convention every operator action keeps). Clearing
// wipes the attribution with it — an unheld bin carries no stale hold history.
func (db *DB) SetBinQualityHold(binID int64, hold bool, by string) error {
	if hold && by == "" {
		return fmt.Errorf("quality hold requires an attribution (station)")
	}
	if hold {
		_, err := db.DB.Exec(`UPDATE bins SET quality_hold = TRUE, hold_by = $2, hold_at = NOW(), updated_at = NOW()
			WHERE id = $1`, binID, by)
		return err
	}
	_, err := db.DB.Exec(`UPDATE bins SET quality_hold = FALSE, hold_by = '', hold_at = NULL, updated_at = NOW()
		WHERE id = $1`, binID)
	return err
}

// BinQualityHold reports a bin's hold marker (false for an unknown bin).
func (db *DB) BinQualityHold(binID int64) (bool, string, error) {
	var hold bool
	var by string
	err := db.DB.QueryRow(`SELECT quality_hold, hold_by FROM bins WHERE id = $1`, binID).Scan(&hold, &by)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return hold, by, nil
}

// ListHeldBins returns every bin carrying the hold marker, oldest hold first —
// the containment screen's queue of bins an operator parked that have not yet
// been picked up.
func (db *DB) ListHeldBins() ([]HeldBinRow, error) {
	rows, err := db.DB.Query(`SELECT b.id, b.label, b.payload_code, b.node_id,
		COALESCE(n.name, ''), b.hold_by, b.hold_at
		FROM bins b LEFT JOIN nodes n ON n.id = b.node_id
		WHERE b.quality_hold = TRUE ORDER BY b.hold_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeldBinRow
	for rows.Next() {
		var r HeldBinRow
		var holdAt sql.NullTime
		if err := rows.Scan(&r.BinID, &r.Label, &r.PayloadCode, &r.NodeID, &r.NodeName, &r.HoldBy, &holdAt); err != nil {
			return nil, err
		}
		if holdAt.Valid {
			t := holdAt.Time
			r.HoldAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HeldBinRow is one held bin as the containment screens read it.
type HeldBinRow struct {
	BinID       int64         `json:"bin_id"`
	Label       string        `json:"label"`
	PayloadCode string        `json:"payload_code"`
	NodeID      sql.NullInt64 `json:"node_id"`
	NodeName    string        `json:"node_name"`
	HoldBy      string        `json:"hold_by"`
	HoldAt      *time.Time    `json:"hold_at"`
}

// ContainmentRouteForNode resolves the quality-containment route the dispatch
// divert reads: the claim rows Core mirrors for one core node, narrowed to
// rows that actually declare a containment destination. Returns the claim's
// ordinary outbound destination (so the divert can scope itself to the
// claim's own FG flow and leave swap returns / evac legs alone) and the
// containment destination itself.
//
// ambiguous reports that MORE THAN ONE DISTINCT containment destination is
// mirrored for this node — several processes (or styles) claiming the same
// node and disagreeing about where containment is. The caller refuses to
// divert on ambiguity (a wrong-parked bin is worse than an unparked one) and
// the disagreement is a config problem the operator fixes, not one dispatch
// guesses its way out of.
func (db *DB) ContainmentRouteForNode(coreNodeName string) (outbound, containment string, ambiguous bool, err error) {
	if coreNodeName == "" {
		return "", "", false, nil
	}
	rows, err := db.DB.Query(`SELECT DISTINCT outbound_destination, containment_destination
		FROM style_claims
		WHERE core_node_name = $1 AND containment_destination <> ''`, coreNodeName)
	if err != nil {
		return "", "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var ob, ct string
		if err := rows.Scan(&ob, &ct); err != nil {
			return "", "", false, err
		}
		if containment == "" {
			outbound, containment = ob, ct
			continue
		}
		if ct != containment || (ob != "" && outbound != "" && ob != outbound) {
			return outbound, containment, true, rows.Err()
		}
	}
	return outbound, containment, false, rows.Err()
}

// CountContainmentRoutedPayloads reports how many claims declaring a
// containment destination exist for one payload code. The containment
// toggle's guard: activating containment for a payload whose claims name no
// route would silently do nothing — the FG deliveries would keep flowing —
// and a containment toggle that does nothing is the worst kind, because the
// floor believes the part is held. Zero → the caller refuses the toggle.
func (db *DB) CountContainmentRoutedPayloads(payloadCode string) (int, error) {
	var n int
	err := db.DB.QueryRow(`SELECT COUNT(*) FROM style_claims
		WHERE payload_code = $1 AND containment_destination <> ''`, payloadCode).Scan(&n)
	return n, err
}
