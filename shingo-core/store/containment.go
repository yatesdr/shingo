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
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"shingocore/store/internal/nodetree"
	"shingocore/store/nodes"
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

// ProducerRoute is one process that PRODUCES a payload, with whether its
// claims carry a containment route.
type ProducerRoute struct {
	ProcessID string `json:"process_id"`
	Routed    bool   `json:"routed"`
}

// ListProducersForPayload names every process in the mirror whose PRODUCE
// claims cover a payload (the primary code or the claim's allowed set), and
// whether that process's claims route containment for it. This is the
// PARTIAL-CONTAINMENT read: a payload flagged while some of its producers
// lack a route keeps flowing to FG from exactly those processes, and the
// contain-confirmation names them — a warning the floor can act on (enable
// the hold on those processes) instead of a hole they discover by a shipped
// bin.
func (db *DB) ListProducersForPayload(payloadCode string) ([]ProducerRoute, error) {
	rows, err := db.DB.Query(`SELECT process_id, payload_code, allowed_payload_codes, containment_destination
		FROM style_claims WHERE role = 'produce'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	covered := map[string]bool{} // process → produces this payload
	routed := map[string]bool{}  // process → any covering claim carries a route
	for rows.Next() {
		var processID, primary, allowedJSON, dest string
		if err := rows.Scan(&processID, &primary, &allowedJSON, &dest); err != nil {
			return nil, err
		}
		covers := primary == payloadCode
		if !covers && allowedJSON != "" {
			var allowed []string
			if json.Unmarshal([]byte(allowedJSON), &allowed) == nil {
				covers = slices.Contains(allowed, payloadCode)
			}
		}
		if !covers {
			continue
		}
		covered[processID] = true
		if dest != "" {
			routed[processID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ProducerRoute, 0, len(covered))
	for processID := range covered {
		out = append(out, ProducerRoute{ProcessID: processID, Routed: routed[processID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProcessID < out[j].ProcessID })
	return out, nil
}

// StampContainmentArrival marks a just-landed bin with the quality-hold
// marker when the place it landed is (inside) a containment destination for
// its payload and that payload's containment is still active. Returns
// whether THIS call wrote the marker.
//
// WHY THE MARKER, AND NOT LOCATION. The divert (dispatch's
// placeForContainment) re-points an FG delivery to the containment
// destination, but arrival is where the bin becomes an ordinary
// available, unclaimed row again — and every anti-resourcing rule Core
// owns (the finder's candidate skip, the loader pool read, the
// empty-carrier fragment, the plant-wide FindSourceFIFO) keys on the
// marker, not on where the bin stands. An unstamped contained bin is, to
// the sourcing engine, just another full bin of its payload, and the
// plant-wide retrieve fallback — which scans every node in the plant —
// would hand it straight back into production. The station-hold path
// stamps at hold time; this is the divert path stamping at arrival, so
// both mechanisms read identically downstream (the recall already stamps
// the same way).
//
// THE MATCH IS SUBTREE-AWARE on purpose. A group destination is the
// expected shape for a containment area, and the NGRP resolution spreads
// arrivals across the group's children — the bin lands on a child while
// the claim names the group. The landed node's ancestor chain (self at
// depth 0) is matched against the nodes the claims name, so a child
// landing finds the group's claim. The destination NAMES are resolved
// through GetByDotName — the exact resolver the divert itself used to
// re-point the delivery — so the two ends of the trip can never disagree
// about what a destination name means.
//
// THE FLAG IS RE-CHECKED at arrival. Containment deactivated while the
// diverted order was in flight leaves the bin ordinary stock that happens
// to stand in the containment area: sourcing it back out is then correct,
// and stamping it would strand it behind a manual release for nothing.
//
// The stamp is conditional (WHERE NOT quality_hold): an operator's hold —
// hold_by names them — is never overwritten by the mechanism, and a bin
// stamped twice stamps once.
func (db *DB) StampContainmentArrival(binID, landedNodeID int64, payloadCode, by string) (bool, error) {
	if binID <= 0 || landedNodeID <= 0 || payloadCode == "" {
		return false, nil
	}
	active, err := db.PayloadContainmentActive(payloadCode)
	if err != nil || !active {
		return false, err
	}
	rows, err := db.DB.Query(`SELECT DISTINCT containment_destination FROM style_claims
		WHERE payload_code = $1 AND containment_destination <> ''`, payloadCode)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var destIDs []int64
	for rows.Next() {
		var dest string
		if err := rows.Scan(&dest); err != nil {
			return false, err
		}
		dn, err := nodes.GetByDotName(db.DB, dest)
		if err != nil || dn == nil {
			continue // a destination that resolves nowhere holds nothing
		}
		destIDs = append(destIDs, dn.ID)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(destIDs) == 0 {
		return false, nil
	}
	var holds bool
	err = db.DB.QueryRow(nodetree.AncestorsOf(1)+`
		SELECT EXISTS (
			SELECT 1 FROM ancestors a
			WHERE a.id = ANY($2)
		)`, landedNodeID, destIDs).Scan(&holds)
	if err != nil || !holds {
		return false, err
	}
	res, err := db.DB.Exec(`UPDATE bins
		SET quality_hold = TRUE, hold_by = $2, hold_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND NOT COALESCE(quality_hold, false)`, binID, by)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
