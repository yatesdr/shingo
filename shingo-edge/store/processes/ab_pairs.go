package processes

// ab_pairs.go — the A/B pairs' reciprocity, listed for the page
// (ListOneWayPairs). An A/B pair is two positions that name each other; a
// claim whose partner names nothing, or a third position, is no pair: the
// walk gives it no fallthrough (abPairOf) and the page warns.

import "shingo/protocol"

// OneWayPair is a claim that names an A/B partner whose claim does not name it
// back: no pair, so the walk gives it no fallthrough.
type OneWayPair struct {
	StyleID   int64  `json:"style_id"`
	StyleName string `json:"style_name"`
	Node      string `json:"node"`
	Partner   string `json:"partner"`
}

// ListOneWayPairs returns the process's live one-way pairs, for the page. A
// press-index front names its back position in the same field: no pair.
func ListOneWayPairs(db DBTX, processID int64) ([]OneWayPair, error) {
	rs, err := db.Query(`SELECT c.style_id, s.name, c.core_node_name, c.paired_core_node, c.swap_mode
		FROM style_node_claims c JOIN styles s ON s.id = c.style_id
		WHERE c.retired_at IS NULL AND s.deleted_at IS NULL AND c.paired_core_node <> '' AND s.process_id = ?
		ORDER BY c.id`, processID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	by := map[int64]map[string]OneWayPair{}
	var all []OneWayPair
	for rs.Next() {
		var c OneWayPair
		var mode string
		if err := rs.Scan(&c.StyleID, &c.StyleName, &c.Node, &c.Partner, &mode); err != nil {
			return nil, err
		}
		if protocol.SwapMode(mode) == protocol.SwapModeTwoRobotPressIndex {
			continue
		}
		if by[c.StyleID] == nil {
			by[c.StyleID] = map[string]OneWayPair{}
		}
		by[c.StyleID][c.Node] = c
		all = append(all, c)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	var out []OneWayPair
	for _, c := range all {
		if p, ok := by[c.StyleID][c.Partner]; !ok || p.Partner != c.Node {
			out = append(out, c)
		}
	}
	return out, nil
}
