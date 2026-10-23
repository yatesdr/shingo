package bins

import (
	"fmt"
)

// tote_want.go — the height an empty cart is wanted at, read with the node it
// is set down on.

// ToteWantSQL is the height, in layers, an empty set down on node n is wanted
// at, for the payload in parameter payloadParam:
//   - none (NULL) when the payload states no tote layers: a plant that sets no
//     tote numbers wants no height anywhere, so no finder's statement changes;
//   - 0 at the core node of a fed press cell (FedClaimSQL) whose claim
//     carries the payload: its totes come from the cart behind it, so tops &
//     bottoms is wanted first, else the shortest stack (domain.Layers(0));
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
