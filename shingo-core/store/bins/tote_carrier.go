package bins

import (
	"database/sql"

	"shingocore/store/internal/nodetree"
)

// tote_carrier.go — SEND BACK's pull: which carrier tops & bottoms of a part
// is, and for which press.

// ToteCarrier is what one read answers about tops & bottoms of a part pulled
// to a node.
//
//   - Core is the fed press (FedClaimSQL) whose outside position the node is
//     and whose claim carries the part (fedOutsideCarryingSQL); "" when the
//     node is no such outside.
//   - Types is how many tote carrier types (a type stating totes_per_layer) the
//     part names: from its carrier rule when the rule has rows (a rule with rows
//     is hard), and only when it has none, from the outside's Allowed Bin Types
//     (the node's bin_type_mode: all, specific, or inherited from the nearest
//     ancestor with types, as GetEffectiveBinTypes reads them). Type is the one,
//     when Types is 1.
type ToteCarrier struct {
	Core  string
	Type  string
	Types int
}

// effectiveTypesSQL is the bin types allowed at the node nodeExpr names, in
// SQL, needing the ancestors CTE anchored on it: GetEffectiveBinTypes' three
// modes in one predicate over bin_types bt.
func effectiveTypesSQL(nodeExpr string) string {
	return `SELECT bt.code, bt.totes_per_layer FROM bin_types bt
		 WHERE CASE COALESCE((SELECT value FROM node_properties
		                       WHERE node_id = ` + nodeExpr + ` AND key = 'bin_type_mode'), '')
		         WHEN 'all' THEN true
		         WHEN 'specific' THEN bt.id IN (SELECT bin_type_id FROM node_bin_types WHERE node_id = ` + nodeExpr + `)
		         ELSE bt.id IN (` + inheritedTypeIDsSQL + `)
		       END`
}

// ToteCarrierFor reads, in one statement, the press whose outside is outside
// and the tote carrier of part (ToteCarrier). Its readers are Core's intake
// guard for a tops & bottoms pull and every find of one, the scanner's replay
// included: no column carries the type, so each asks again.
func ToteCarrierFor(db *sql.DB, part, outside string) (ToteCarrier, error) {
	node := `(SELECT id FROM nodes WHERE name = $2)`
	var tc ToteCarrier
	err := db.QueryRow(nodetree.AncestorsOfExpr(node)+`,
		cell AS (`+fedOutsideCarryingSQL("$2", "$1")+`),
		ruled AS (SELECT bt.code, bt.totes_per_layer FROM payload_bin_types pbt
		            JOIN payloads p ON p.id = pbt.payload_id
		            JOIN bin_types bt ON bt.id = pbt.bin_type_id
		           WHERE p.code = $1),
		allowed AS (`+effectiveTypesSQL(node)+`),
		totes AS (SELECT code FROM ruled WHERE totes_per_layer IS NOT NULL
		          UNION ALL
		          SELECT code FROM allowed
		           WHERE totes_per_layer IS NOT NULL AND NOT EXISTS (SELECT 1 FROM ruled))
		SELECT COALESCE((SELECT core FROM cell), ''), (SELECT count(*) FROM totes),
		       COALESCE((SELECT min(code) FROM totes), '')`, part, outside).Scan(&tc.Core, &tc.Types, &tc.Type)
	if tc.Types != 1 {
		tc.Type = ""
	}
	return tc, err
}
