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

// FedInboundSource is where the active fed claim at a station's core node draws
// from (style_claims.inbound_source), asked about one of its positions: the
// claim whose outside position (paired_core_node) or second paired position is
// position, and whose shape is a fed press (FedClaimSQL), so a sequential
// partner never qualifies. Blank when no such claim is active at the node, or
// it names no source. One statement: the shape and the source are one read.
//
// Its reader is Core's vacant-pickup fallback (dispatch reanchorVacantPickup):
// a pickup at a vacant dependent position of a fed press is fetched from here.
func FedInboundSource(db *sql.DB, stationID, coreNodeName, position string) (string, error) {
	var src string
	err := db.QueryRow(`SELECT sc.inbound_source
		FROM style_claims sc
		JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
		WHERE ps.is_active AND ps.station_id = $1 AND sc.core_node_name = $2 AND sc.inbound_source <> ''
		  AND $3 <> '' AND $3 IN (sc.paired_core_node, sc.second_paired_core_node)
		  AND `+FedClaimSQL("sc")+`
		ORDER BY sc.process_id, sc.seq
		LIMIT 1`, stationID, coreNodeName, position).Scan(&src)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return src, err
}
