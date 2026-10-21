package bins

import (
	"fmt"
)

// tote_want.go — the height an empty cart is wanted at, read with the node it
// is set down on.

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

// ToteWantSQL is the height, in layers, an empty set down on node n is wanted
// at, for the payload in parameter payloadParam:
//   - none (NULL) when the payload states no tote layers: a plant that sets no
//     tote numbers wants no height anywhere, so no finder's statement changes;
//   - 0, a base, at the core node of a fed press cell (FedClaimSQL) whose claim
//     carries the payload: its totes come from the cart behind it;
//   - else the payload's tote_layers, a full stack.
//
// The CASE reads the claims only for a payload that states its layers. It is a
// column of the planner's destination read (store.DB.GetSetDownNode), which
// names the node n.
func ToteWantSQL(payloadParam int) string {
	return fmt.Sprintf(`(SELECT CASE WHEN EXISTS (
	            SELECT 1 FROM style_claims sc
	             WHERE sc.core_node_name = n.name
	               AND (sc.payload_code = p.code
	                    OR (sc.allowed_payload_codes <> '[]' AND sc.allowed_payload_codes::jsonb ? p.code))
	               AND `+FedClaimSQL("sc")+`)
	          THEN 0 ELSE p.tote_layers END
	     FROM payloads p WHERE p.code = $%d AND p.tote_layers IS NOT NULL)`, payloadParam)
}
