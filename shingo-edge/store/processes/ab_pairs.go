package processes

// ab_pairs.go — the A/B pairs' reciprocity, read over the finished claims
// table (CheckPairPartners) and listed for the page (ListOneWayPairs).

import (
	"fmt"
	"sort"
	"strings"

	"shingo/protocol"
)

// pairClaim is one live claim that names a partner (not a press-index front,
// whose field names its back position).
type pairClaim struct {
	StyleID   int64
	StyleName string
	Node      string
	Partner   string
}

const pairClaimsSQL = `SELECT c.style_id, s.name, c.core_node_name, c.paired_core_node, c.swap_mode
	FROM style_node_claims c JOIN styles s ON s.id = c.style_id
	WHERE c.retired_at IS NULL AND s.deleted_at IS NULL AND c.paired_core_node <> ''
	ORDER BY c.id`

// readPairClaims returns the live claims naming a partner, by style and node.
func readPairClaims(db DBTX, where string, args ...any) (map[int64]map[string]pairClaim, []pairClaim, error) {
	q := pairClaimsSQL
	if where != "" {
		q = strings.Replace(q, "ORDER BY", "AND "+where+" ORDER BY", 1)
	}
	rs, err := db.Query(q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	by := map[int64]map[string]pairClaim{}
	var all []pairClaim
	for rs.Next() {
		var c pairClaim
		var mode string
		if err := rs.Scan(&c.StyleID, &c.StyleName, &c.Node, &c.Partner, &mode); err != nil {
			return nil, nil, err
		}
		if protocol.SwapMode(mode) == protocol.SwapModeTwoRobotPressIndex {
			continue
		}
		if by[c.StyleID] == nil {
			by[c.StyleID] = map[string]pairClaim{}
		}
		by[c.StyleID][c.Node] = c
		all = append(all, c)
	}
	return by, all, rs.Err()
}

// CheckPairPartners is the A/B pairs' rule, run at the end of a write that
// touches claims, beside CheckKeepStagedSpots and for the same reason: a pair
// is two rows, and a save passes through states no one asked for.
//
// It refuses a CONTRADICTION: a claim naming a partner whose claim, in the same
// style, names a third position. A ONE-WAY pair (the partner names nothing, or
// has no claim yet) is allowed, because a door that writes one claim at a time
// can only build a pair one side at a time: the walk treats it as no pair
// (no second lit side skipped, no fallthrough), and the page warns
// (ListOneWayPairs). One SELECT over the live pair claims.
func CheckPairPartners(tx DBTX) error {
	by, all, err := readPairClaims(tx, "")
	if err != nil {
		return fmt.Errorf("A/B pair check: %w", err)
	}
	var bad []string
	for _, c := range all {
		p, ok := by[c.StyleID][c.Partner]
		if ok && p.Partner != c.Node {
			bad = append(bad, fmt.Sprintf("%s: %s names %s as its A/B partner, and %s names %s",
				c.StyleName, c.Node, c.Partner, c.Partner, p.Partner))
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("an A/B pair is two positions that name each other — %s", strings.Join(bad, "; "))
	}
	return nil
}

// OneWayPair is a claim that names an A/B partner whose claim does not name it
// back: no pair, so the walk gives it no fallthrough.
type OneWayPair struct {
	StyleID   int64  `json:"style_id"`
	StyleName string `json:"style_name"`
	Node      string `json:"node"`
	Partner   string `json:"partner"`
}

// ListOneWayPairs returns the process's live one-way pairs, for the page.
func ListOneWayPairs(db DBTX, processID int64) ([]OneWayPair, error) {
	by, all, err := readPairClaims(db, "s.process_id = ?", processID)
	if err != nil {
		return nil, err
	}
	var out []OneWayPair
	for _, c := range all {
		if p, ok := by[c.StyleID][c.Partner]; !ok || p.Partner != c.Node {
			out = append(out, OneWayPair(c))
		}
	}
	return out, nil
}
