// return_sources.go — where the plant DECLARES a bin is used from, for the
// cancel-return policy (engine/cancel_return.go, its only caller).
//
// A PLACE THIS FILE NAMES IS A PLACE THE FINDER SERVES — claims' inbound
// sources for fulls (dispatch.SourceFinder tiers 1/2), claims' declared empties
// places for empties (tier 3 through the same claims), fence included. Nothing
// here invents a place.
//
// That sentence is the predicate until a second user exists. The fence half
// lives with the caller (nodes.GroupFencesAsker, asked per declared place),
// because it is a question about the asking process, not about the claim.

package store

import "fmt"

// ReturnSourcesForPayload names the nodes the plant will source a FULL bin of
// payload from the next time a line asks for it, best first.
//
// TWO TABLES, BECAUSE THE PLANT KEEPS THIS FACT IN TWO PLACES:
//
//	style_claims  a line's CONSUME claim names where it draws its payload from
//	              (inbound_source, v121) — the node the Edge stamps as the
//	              order's SourceNode when the line asks again, and the scope
//	              Core's finder resolves inside and never widens. Matched on the
//	              claim's payload_code or its allowed set (a JSON array in TEXT).
//	bin_loaders   an UNLOADER's (role consume) inbound_source is where its fulls
//	              come from, and its payloads are its bin_loader_payloads rows
//	              (shared window) or its pinned homes (dedicated positions). The
//	              Edge publisher never mirrors a loader's own claim into
//	              style_claims, so this table is the only place Core holds it.
//
// CONSUME ONLY. A produce claim's inbound_source is where that press draws its
// EMPTIES from; a full bin of the part it makes does not belong there.
//
// THE ORDER is the policy's walk order: a claim at one of ownNodes first (the
// cancelled order's own process), then claims under a style a process is
// running now, then by name. An unloader ranks with the claims under no running
// style, because nothing about a loader says which style is live. Blank sources
// are dropped: Core reads a blank leg as "unknown", never as a node.
//
// ONE STATEMENT. The style_claims arm's payload_code match is served by
// idx_style_claims_payload; the allowed-set match and the loader arm scan
// tables that hold one row per claim and per loader, which on a plant is a few
// hundred rows.
func (db *DB) ReturnSourcesForPayload(payload string, ownNodes []string) ([]string, error) {
	if payload == "" {
		return nil, nil
	}
	if ownNodes == nil {
		ownNodes = []string{}
	}
	return db.declaredPlaces(`
		WITH named AS (
			SELECT sc.inbound_source AS src,
			       (sc.core_node_name = ANY($2)) AS own,
			       COALESCE(ps.is_active, false) AS active
			  FROM style_claims sc
			  LEFT JOIN process_styles ps
			    ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
			 WHERE sc.role = 'consume' AND sc.inbound_source <> ''
			   AND (sc.payload_code = $1
			        OR (sc.allowed_payload_codes <> '[]' AND sc.allowed_payload_codes::jsonb ? $1))
			UNION ALL
			SELECT l.inbound_source, false, false
			  FROM bin_loaders l
			 WHERE l.archived_at IS NULL AND l.role = 'consume' AND l.inbound_source <> ''
			   AND (EXISTS (SELECT 1 FROM bin_loader_payloads p WHERE p.loader_id = l.id AND p.payload_code = $1)
			        OR EXISTS (SELECT 1 FROM bin_loader_homes h WHERE h.loader_id = l.id AND h.payload_code = $1))
		)
		SELECT src FROM named
		 GROUP BY src
		 ORDER BY bool_or(own) DESC, bool_or(active) DESC, src`, "return sources for "+payload, payload, ownNodes)
}

// EmptiesPlacesForBinType names the nodes the plant DECLARES an empty carrier of
// this bin type goes to and is drawn from, best first — the twin of
// ReturnSourcesForPayload for an empty bin. Same mirror, same walk order.
//
// FOUR DECLARATIONS, EACH ONE SOMEBODY CONFIGURED:
//
//	consume claim  outbound_destination — where an ordinary evacuation takes
//	               this line's spent carrier (the swap plan's drop).
//	produce claim  inbound_source — where the press draws its empties from.
//	consume loader outbound_dest — where an unloader sends the carriers it has
//	               drained.
//	produce loader inbound_source — where a loader's empties come from.
//
// NOT the maintained groups supporting a process, and not any bank's overflow:
// neither says where this line's empties go. A bank at its level still follows
// its OWN overflow when a declaration names that bank (the caller's one hop).
//
// WHICH CLAIMS COUNT FOR A TYPE. A claim declares a part, not a carrier; it
// counts for this type when its payload's carrier rule admits the type — the
// payload maps the type, or maps none — the same rule the empty finder applies
// to the asking part (helpers.PayloadBinTypeRuleArm). A loader counts when any
// of its payloads does. A claim with no payload counts.
func (db *DB) EmptiesPlacesForBinType(binTypeID int64, ownNodes []string) ([]string, error) {
	if ownNodes == nil {
		ownNodes = []string{}
	}
	admits := func(payloadExpr string) string {
		return `(` + payloadExpr + ` = ''
		     OR EXISTS (SELECT 1 FROM payload_bin_types pbt JOIN payloads p ON p.id = pbt.payload_id
		                 WHERE p.code = ` + payloadExpr + ` AND pbt.bin_type_id = $1)
		     OR NOT EXISTS (SELECT 1 FROM payload_bin_types pbt JOIN payloads p ON p.id = pbt.payload_id
		                     WHERE p.code = ` + payloadExpr + `))`
	}
	return db.declaredPlaces(`
		WITH named AS (
			SELECT CASE WHEN sc.role = 'consume' THEN sc.outbound_destination ELSE sc.inbound_source END AS src,
			       (sc.core_node_name = ANY($2)) AS own,
			       COALESCE(ps.is_active, false) AS active
			  FROM style_claims sc
			  LEFT JOIN process_styles ps
			    ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
			 WHERE ((sc.role = 'consume' AND sc.outbound_destination <> '')
			     OR (sc.role = 'produce' AND sc.inbound_source <> ''))
			   AND `+admits("sc.payload_code")+`
			UNION ALL
			SELECT CASE WHEN l.role = 'consume' THEN l.outbound_dest ELSE l.inbound_source END, false, false
			  FROM bin_loaders l
			 WHERE l.archived_at IS NULL
			   AND ((l.role = 'consume' AND l.outbound_dest <> '')
			     OR (l.role = 'produce' AND l.inbound_source <> ''))
			   AND EXISTS (SELECT 1 FROM (
			         SELECT p.payload_code AS pc FROM bin_loader_payloads p WHERE p.loader_id = l.id
			         UNION SELECT h.payload_code FROM bin_loader_homes h WHERE h.loader_id = l.id
			       ) lp WHERE `+admits("lp.pc")+`)
		)
		SELECT src FROM named
		 GROUP BY src
		 ORDER BY bool_or(own) DESC, bool_or(active) DESC, src`, fmt.Sprintf("empties places for bin type %d", binTypeID),
		binTypeID, ownNodes)
}

// declaredPlaces runs one of the statements above and returns its node names.
func (db *DB) declaredPlaces(query, what string, args ...any) ([]string, error) {
	rows, err := db.DB.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", what, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
