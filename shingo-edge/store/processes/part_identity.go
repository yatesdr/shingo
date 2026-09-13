// part_identity.go — a process's live styles with the part identities their
// produce claims carry, in one query.
//
// The engine derives a style's part-identity set (the CATIDs the press may
// legitimately stamp while running it) from the style's produce claims: each
// claim's payload → the synced payload_catalog row → its CATID. Read per style
// that is one claim list plus one catalog lookup per produce claim, and the
// PLC CATID monitor asks it for EVERY style in the process on every stable
// part change — ~450 queries at 90 styles × 4 claims, on a store pinned to
// one connection. This read answers the process-wide question in one
// statement; the derivation rules (the manual pin wins, a catalog value
// splits on commas) stay in the engine, which is why the catalog values come
// back raw.

package processes

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
)

// StylePartIdentity is one live style of a process and the raw catalog CATID
// value of each of its produce claims' payloads.
type StylePartIdentity struct {
	StyleID int64
	Name    string
	// ExpectedCATID is the style's manual pin, verbatim ("" when none). The
	// engine treats a non-blank pin as the whole set.
	ExpectedCATID string
	// CatalogCATIDs holds one entry per produce claim whose payload has a
	// catalog row with a non-blank CATID — the catalog's own comma-joined
	// convention, unsplit. A produce claim whose payload is absent from the
	// catalog, or whose catalog CATID is blank, contributes no entry, exactly
	// as the per-style derivation skipped it.
	CatalogCATIDs []string
}

// ListStylePartIdentitiesByProcess returns every LIVE style of the process, in
// name order, with its produce claims' catalog CATIDs, in ONE query.
//
// Same liveStyles predicate as ListStylesByProcess — a retired style is not a
// candidate the press can be running. styles is the only table in the join
// with a deleted_at column, so the shared fragment applies unqualified; if
// another joined table ever grows one this query fails loudly as ambiguous
// rather than silently filtering the wrong table.
//
// Both joins are LEFT so a style with no produce claims, or none whose payload
// is in the catalog, still comes back (with no CATIDs): a style that maps to
// nothing is still a style, and stylesForCATID's zero/one/many contract needs
// every live style visited. The retired filter sits in the claim join's ON
// clause for the same reason — in the WHERE it would drop the style, not the
// claim. A retired claim contributes no CATID, exactly as a deleted one did.
func ListStylePartIdentitiesByProcess(db *sql.DB, processID int64) ([]StylePartIdentity, error) {
	rows, err := db.Query(`SELECT s.id, s.name, COALESCE(s.expected_catid, ''), COALESCE(pc.catid, '')
		FROM styles s
		LEFT JOIN style_node_claims c ON c.style_id = s.id AND c.role = ? AND c.payload_code != '' AND c.retired_at IS NULL
		LEFT JOIN payload_catalog pc ON pc.code = c.payload_code
		WHERE s.process_id = ? AND`+liveStyles+`
		ORDER BY s.name, s.id, c.id`, string(protocol.ClaimRoleProduce), processID)
	if err != nil {
		return nil, fmt.Errorf("style part identities for process %d: %w", processID, err)
	}
	defer rows.Close()

	var out []StylePartIdentity
	for rows.Next() {
		var (
			id       int64
			name     string
			expected string
			catid    string
		)
		if err := rows.Scan(&id, &name, &expected, &catid); err != nil {
			return nil, fmt.Errorf("style part identities for process %d: scan: %w", processID, err)
		}
		// Rows arrive grouped by style (ORDER BY s.name, s.id); the first row
		// of a style opens its entry, every row's catalog value joins it.
		if len(out) == 0 || out[len(out)-1].StyleID != id {
			out = append(out, StylePartIdentity{StyleID: id, Name: name, ExpectedCATID: expected})
		}
		if catid != "" {
			last := &out[len(out)-1]
			last.CatalogCATIDs = append(last.CatalogCATIDs, catid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("style part identities for process %d: %w", processID, err)
	}
	return out, nil
}
