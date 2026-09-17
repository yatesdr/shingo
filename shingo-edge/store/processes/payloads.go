// payloads.go — the process PART SET (process_payloads) inside the processes
// aggregate: which payloads a process's flows may put on a position. See the
// DDL's own note for why it is a table and why the offer list is a union.

package processes

import (
	"sort"
	"strings"
)

// ListProcessPayloads is the STORED half of the part set — the rows an
// engineer typed — in payload-code order.
//
// The engineer's half alone, because it is the only half a write may touch. A
// screen that offers parts wants ProcessPalette below; this one is for the
// sheet that EDITS the list, which must show what it is about to replace and
// not a union it cannot write back.
func ListProcessPayloads(db DBTX, processID int64) ([]string, error) {
	rows, err := db.Query(`SELECT payload_code FROM process_payloads
		WHERE process_id = ? ORDER BY payload_code`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	return out, rows.Err()
}

// ReplaceProcessPayloads sets the stored half to exactly this list.
//
// A SET-TO, AND ONE ROUND TRIP. The page sends the whole list because that is
// what the picker holds; a per-name POST would be the routing set's shape,
// which costs Add Process a round trip per part. Blanks and duplicates are
// dropped rather than written: the list comes from a picker, and a payload
// code of "" is a nameless part every screen would then offer.
//
// A DELETE-AND-INSERT INSIDE ONE TRANSACTION rather than a diff. The table has
// no dependants — nothing references a process_payloads row, which is the
// difference between this and the routing set, whose delete is refused while a
// live claim routes through the name. A part taken out of the set stops being
// OFFERED; the claims that already run it are untouched and keep it in the
// palette (ProcessPalette).
func ReplaceProcessPayloads(db DBTX, processID int64, codes []string) error {
	clean := make([]string, 0, len(codes))
	seen := map[string]bool{}
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		clean = append(clean, c)
	}
	if _, err := db.Exec(`DELETE FROM process_payloads WHERE process_id = ?`, processID); err != nil {
		return err
	}
	for _, c := range clean {
		if _, err := db.Exec(`INSERT INTO process_payloads (process_id, payload_code) VALUES (?, ?)`,
			processID, c); err != nil {
			return err
		}
	}
	return nil
}

// ProcessPalette is the OFFER LIST: the stored rows and every payload the
// process's live claims already name, as one sorted set.
//
// THE UNION IS THE RULE (SYNTH §2). Only the stored half is writable, so this
// is not two sources of truth — it is one list assembled from the one place a
// part can be typed and the one place a part is already in use. A backfill of
// the claimed half into rows would repeat the routing set's "21 rows nobody
// switched on", and there is nothing here for an engineer to adopt: a part the
// cell demonstrably runs is not a name waiting for a decision.
func ProcessPalette(db DBTX, processID int64) ([]string, error) {
	rows, err := db.Query(`SELECT payload_code FROM process_payloads WHERE process_id = ?
		UNION
		SELECT payload_code FROM style_node_claims
		 WHERE payload_code <> ''
		   AND style_id IN (SELECT id FROM styles WHERE process_id = ? AND`+liveStyles+`)
		   AND`+liveClaims, processID, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
