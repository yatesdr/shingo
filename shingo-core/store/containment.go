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
	"sort"
	"time"

	"shingo/protocol"

	"shingocore/domain"
	"shingocore/store/internal/nodetree"
	"shingocore/store/nodes"
)

// PayloadContainmentRow is one payload's containment state as the UI reads it.
// Domain-owned so the www handlers can name it without importing store; store
// callers compile unchanged through the alias (the demand_origins.go pattern).
type PayloadContainmentRow = domain.PayloadContainmentRow

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
	rows, err := db.DB.Query(`SELECT b.id, b.label, b.payload_code,
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
		if err := rows.Scan(&r.BinID, &r.Label, &r.PayloadCode, &r.NodeName, &r.HoldBy, &holdAt); err != nil {
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

// HeldBinRow is one held bin as the containment screens read it. Domain-owned;
// see PayloadContainmentRow.
type HeldBinRow = domain.HeldBinRow

// containmentDestinationsSQL reads every containment destination the claim
// mirror names, with what stands there, in one statement — so the snapshot
// costs the same whatever the number of destinations.
//
// It answers exactly what the Edge's containment page used to ask Core per
// destination over HTTP: the name resolves as GetByDotName resolves it
// ("PARENT.CHILD" is the child under that parent); a GROUP destination's
// members are its direct, non-synthetic, non-group children, named
// "<group>.<child>" as the node-children read names them; a destination with
// no such children is its own single member; and each member shows the bin the
// node-bins read showed — its newest non-retired bin, if any. An unresolved
// name is a member with no node and so no bin.
const containmentDestinationsSQL = `
WITH dests AS (
	SELECT DISTINCT containment_destination AS dest FROM style_claims
	WHERE containment_destination <> ''
), resolved AS (
	SELECT d.dest, n.id, n.name, COALESCE(nt.code, '') AS type_code
	FROM dests d
	LEFT JOIN LATERAL (
		SELECT x.id, x.name, x.node_type_id FROM nodes x
		WHERE CASE WHEN strpos(d.dest, '.') = 0 THEN x.name = d.dest
			ELSE x.name = substr(d.dest, strpos(d.dest, '.') + 1)
				AND x.parent_id IN (SELECT p.id FROM nodes p WHERE p.name = split_part(d.dest, '.', 1))
			END
		ORDER BY x.id LIMIT 1
	) n ON TRUE
	LEFT JOIN node_types nt ON nt.id = n.node_type_id
), children AS (
	SELECT r.dest, c.id, r.name || '.' || c.name AS member
	FROM resolved r
	JOIN nodes c ON c.parent_id = r.id
	LEFT JOIN node_types ct ON ct.id = c.node_type_id
	WHERE r.type_code = 'NGRP' AND NOT c.is_synthetic AND COALESCE(ct.code, '') <> 'NGRP'
		AND r.name || '.' || c.name <> r.dest
), members AS (
	SELECT dest, id, member, TRUE AS is_child FROM children
	UNION ALL
	SELECT r.dest, r.id, r.dest, FALSE FROM resolved r
	WHERE NOT EXISTS (SELECT 1 FROM children ch WHERE ch.dest = r.dest)
)
SELECT m.dest, m.member, m.is_child, b.id, COALESCE(b.label, ''), COALESCE(b.payload_code, ''),
	COALESCE(b.uop_remaining, 0)
FROM members m
LEFT JOIN LATERAL (
	SELECT id, label, payload_code, uop_remaining FROM bins
	WHERE node_id = m.id AND status <> 'retired'
	ORDER BY id DESC LIMIT 1
) b ON TRUE
ORDER BY m.dest, m.member`

// ListContainmentDestinations returns every containment destination the claim
// mirror names, ordered by name, each with its group children and the occupied
// bins at it or at its children (containmentDestinationsSQL says how each part
// is read). Children and Bins are never nil, so a destination with none sends
// [] like every other empty list in the snapshot.
func (db *DB) ListContainmentDestinations() ([]protocol.ContainmentDestination, error) {
	rows, err := db.DB.Query(containmentDestinationsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.ContainmentDestination{}
	for rows.Next() {
		var dest, member, label, payload string
		var isChild bool
		var binID sql.NullInt64
		var uop int
		if err := rows.Scan(&dest, &member, &isChild, &binID, &label, &payload, &uop); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Node != dest {
			out = append(out, protocol.ContainmentDestination{
				Node: dest, Children: []string{}, Bins: []protocol.ContainmentBin{},
			})
		}
		d := &out[len(out)-1]
		if isChild {
			d.Children = append(d.Children, member)
		}
		if binID.Valid {
			d.Bins = append(d.Bins, protocol.ContainmentBin{
				Node: member, BinID: binID.Int64, Label: label, PayloadCode: payload, UOP: uop,
			})
		}
	}
	return out, rows.Err()
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

// ListProducersByPayload names, for every payload, each process in the mirror
// whose PRODUCE claims cover it (the primary code or the claim's allowed set),
// and whether that process's covering claims route containment for it. This is
// the PARTIAL-CONTAINMENT read: a payload flagged while some of its producers
// lack a route keeps flowing to FG from exactly those processes, and the
// contain-confirmation names them — a warning the floor can act on (enable the
// hold on those processes) instead of a hole they discover by a shipped bin.
//
// One read of the produce claims for the whole Payloads table. It replaced a
// per-payload read that scanned every produce claim once per row.
func (db *DB) ListProducersByPayload() (map[string][]ProducerRoute, error) {
	rows, err := db.DB.Query(`SELECT process_id, payload_code, allowed_payload_codes, containment_destination
		FROM style_claims WHERE role = 'produce'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	covered := map[string]map[string]bool{} // payload → process → produces it
	routed := map[string]map[string]bool{}  // payload → process → a covering claim carries a route
	for rows.Next() {
		var processID, primary, allowedJSON, dest string
		if err := rows.Scan(&processID, &primary, &allowedJSON, &dest); err != nil {
			return nil, err
		}
		codes := []string{primary}
		if allowedJSON != "" {
			var allowed []string
			if json.Unmarshal([]byte(allowedJSON), &allowed) == nil {
				codes = append(codes, allowed...)
			}
		}
		for _, code := range codes {
			if covered[code] == nil {
				covered[code], routed[code] = map[string]bool{}, map[string]bool{}
			}
			covered[code][processID] = true
			if dest != "" {
				routed[code][processID] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string][]ProducerRoute, len(covered))
	for code, procs := range covered {
		list := make([]ProducerRoute, 0, len(procs))
		for processID := range procs {
			list = append(list, ProducerRoute{ProcessID: processID, Routed: routed[code][processID]})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].ProcessID < list[j].ProcessID })
		out[code] = list
	}
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
