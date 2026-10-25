package bins

import (
	"database/sql"
	"errors"
)

// fed_claim.go — the press cell fed from behind: its shape, and where it draws
// from.

// FedClaimSQL is the shape test of a press cell that is fed from behind: a
// style_claims row (alias) whose paired position is set and is NOT itself a
// core node of the same process and style. Its core node is the press; its
// paired position is the outside position the totes are drawn from.
//
// A SEQUENTIAL PAIR IS NOT ONE. Two press positions that alternate name each
// other as paired, and each is a core node of the same process and style, so
// neither reads as the other's outside position. The shape decides this, never
// a swap-mode string (Core reads none). A second paired position
// (second_paired_core_node) is never the outside position here.
// Pins: TestToteWant_TheShape, TestToteLift_SequentialPartnerIsNoOutside.
func FedClaimSQL(alias string) string {
	return alias + `.paired_core_node <> ''
	  AND NOT EXISTS (SELECT 1 FROM style_claims pc
	                  WHERE pc.core_node_name = ` + alias + `.paired_core_node
	                    AND pc.process_id = ` + alias + `.process_id
	                    AND pc.style_id = ` + alias + `.style_id)`
}

// FedOutsideSQL selects, as core, the press of the fed cell (FedClaimSQL) whose
// outside position (paired_core_node) is the node named by nodeExpr: the active
// style's claim first, then by core node name; no row when the node is no fed
// cell's outside (a sequential partner, a second paired position and every
// other node). One spelling for the lift's derivation (toteLiftSQL's cell) and
// the by-contents choice's rule that a tote cart off a fed outside keeps its
// planned drop (IsFedOutside).
func FedOutsideSQL(nodeExpr string) string {
	return fedOutsideSQL(nodeExpr, "")
}

// fedOutsideCarryingSQL is FedOutsideSQL for a fed claim that carries the part
// payloadExpr names (claimCarriesSQL): SEND BACK's pull lands tops & bottoms of
// the press's own part on its outside, and that press is the pull's asker
// (FedPressFor).
func fedOutsideCarryingSQL(nodeExpr, payloadExpr string) string {
	return fedOutsideSQL(nodeExpr, "\n\t\t   AND "+claimCarriesSQL("sc", payloadExpr))
}

func fedOutsideSQL(nodeExpr, also string) string {
	return `
		SELECT sc.core_node_name AS core
		  FROM style_claims sc
		  LEFT JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
		 WHERE sc.paired_core_node = ` + nodeExpr + `
		   AND ` + FedClaimSQL("sc") + also + `
		 ORDER BY COALESCE(ps.is_active, false) DESC, sc.core_node_name
		 LIMIT 1`
}

// claimCarriesSQL is whether a style_claims row (alias) carries the part
// payloadExpr names: its payload, or one of its allowed payloads. One spelling
// for the height an empty is wanted at (ToteWantSQL) and the fed outside a
// tops & bottoms pull lands on (fedOutsideCarryingSQL).
func claimCarriesSQL(alias, payloadExpr string) string {
	return `(` + alias + `.payload_code = ` + payloadExpr + `
	                    OR (` + alias + `.allowed_payload_codes <> '[]' AND ` + alias + `.allowed_payload_codes::jsonb ? ` + payloadExpr + `))`
}

// FedPressFor reads, in one statement, the press of the fed cell
// (fedOutsideCarryingSQL, composed from FedClaimSQL) whose outside position is
// outside and whose claim carries part: the asker of a pull of tops & bottoms. ""
// when outside is no such press's outside. Its readers are Core's intake guard
// for the pull and every find of it, the scanner's replay included (dispatch
// pressFor): the press rides no column.
func FedPressFor(db *sql.DB, part, outside string) (string, error) {
	var press string
	err := db.QueryRow(`SELECT COALESCE((`+fedOutsideCarryingSQL("$2", "$1")+`), '')`, part, outside).Scan(&press)
	return press, err
}

// IsFedOutside reports whether node is a fed press cell's outside position
// (FedOutsideSQL). One statement.
func IsFedOutside(db *sql.DB, node string) (bool, error) {
	var is bool
	err := db.QueryRow(`SELECT EXISTS (`+FedOutsideSQL("$1")+`)`, node).Scan(&is)
	return is, err
}

// FedInboundSource is where the active fed claim at a station's core node draws
// from (style_claims.inbound_source), asked about one of its positions: the
// claim whose outside position (paired_core_node) or second paired position is
// position, and whose shape is a fed press (FedClaimSQL), so a sequential
// partner never qualifies. Blank when no such claim is active at the node, or
// it names no source. kind is the episode kind of originID, the asking order's
// origin (demand_origins.kind: a steady swap's cell, a changeover's), blank
// when the order has none or Core holds no such episode. One statement: the
// shape, the source and the kind are one read.
//
// Its reader is Core's vacant-pickup fallback (dispatch reanchorVacantPickup):
// a pickup at a vacant dependent position of a fed press is fetched from here,
// for a steady leg alone (the kind).
func FedInboundSource(db *sql.DB, stationID, coreNodeName, position, originID string) (src, kind string, err error) {
	err = db.QueryRow(`SELECT sc.inbound_source,
		       COALESCE((SELECT d.kind FROM demand_origins d WHERE d.origin_id = NULLIF($4, '')::uuid), '')
		FROM style_claims sc
		JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
		WHERE ps.is_active AND ps.station_id = $1 AND sc.core_node_name = $2 AND sc.inbound_source <> ''
		  AND $3 <> '' AND $3 IN (sc.paired_core_node, sc.second_paired_core_node)
		  AND `+FedClaimSQL("sc")+`
		ORDER BY sc.process_id, sc.seq
		LIMIT 1`, stationID, coreNodeName, position, originID).Scan(&src, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return src, kind, err
}
