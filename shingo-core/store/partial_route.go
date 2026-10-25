package store

import (
	"database/sql"
	"errors"
	"sort"
	"time"

	"shingo/protocol"
	"shingocore/store/internal/helpers"
)

// PartialHomeFor is the partial_destination of the produce claim at
// coreNodeName that makes payloadCode: where a carrier of that part, lifted off
// that node unfinished, goes to wait and be called back. "" when no such claim
// declares one.
//
// KEYED ON THE NODE AND THE CARRIER'S PART, not the node alone. Two styles may
// claim one node with different parts and different homes; the carrier says
// which claim it came from, whichever style is active when it leaves.
//
// ambiguous reports MORE THAN ONE distinct home for the same node and part
// (two processes claiming the node and disagreeing). The caller leaves the
// carrier on its authored drop: a partial sent to the wrong home is lost to
// the line that would finish it, and the disagreement is a config problem.
func (db *DB) PartialHomeFor(coreNodeName, payloadCode string) (home string, ambiguous bool, err error) {
	if coreNodeName == "" || payloadCode == "" {
		return "", false, nil
	}
	rows, err := db.DB.Query(`SELECT DISTINCT partial_destination FROM style_claims
		WHERE core_node_name = $1 AND payload_code = $2 AND role = $3 AND partial_destination <> ''`,
		coreNodeName, payloadCode, string(protocol.ClaimRoleProduce))
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return "", false, err
		}
		if home == "" {
			home = h
			continue
		}
		if h != home {
			return home, true, rows.Err()
		}
	}
	return home, false, rows.Err()
}

// PartialHomesForPayload is every home a partial of payloadCode may wait in:
// the partial_destination of each produce claim that makes it. A refill that
// would fill a carrier of the part looks there first. Sorted, distinct.
func (db *DB) PartialHomesForPayload(payloadCode string) ([]string, error) {
	if payloadCode == "" {
		return nil, nil
	}
	rows, err := db.DB.Query(`SELECT DISTINCT partial_destination FROM style_claims
		WHERE payload_code = $1 AND role = $2 AND partial_destination <> ''`,
		payloadCode, string(protocol.ClaimRoleProduce))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// StampHeldSince: a count declared at the node the carrier stands on makes it
// held (or not) from that moment (helpers.StampHeldSinceTx).
func (db *DB) StampHeldSince(binID int64) error {
	return helpers.StampHeldSinceDeclaredTx(db.DB, binID)
}

// IsHeldPartial reports whether the bin is held: unfinished, in a partial home
// of its own part. The clear refuses one unless the person says unload.
func (db *DB) IsHeldPartial(binID int64) (bool, error) {
	var held bool
	err := db.DB.QueryRow(`SELECT `+helpers.HeldPartialSQL("b")+` FROM bins b WHERE b.id = $1`, binID).Scan(&held)
	return held, err
}

// InPartialHomeOf reports whether a node stands inside a partial home of the
// part: a window where a carrier of it is held to be finished later. A load
// there must say what it holds.
func (db *DB) InPartialHomeOf(nodeID int64, payloadCode string) (bool, error) {
	var in bool
	err := db.DB.QueryRow(`SELECT `+helpers.InPartialHomeSQL("$1::bigint", "$2::text"), nodeID, payloadCode).Scan(&in)
	return in, err
}

// StampCarrierForLoad gives a bare cart its carrier's type before a LOAD
// judges and writes it: a person loading parts onto a bare cart has put a
// carrier on it, and the carrier rule judges the carrier, not the marker a
// cart carries while it has none. Returns the marker's type id to restore if
// the load is refused, or nil when the bin was not a bare cart.
func (db *DB) StampCarrierForLoad(binID int64) (*int64, error) {
	var marker int64
	err := db.DB.QueryRow(`WITH old AS (SELECT b.bin_type_id AS id, bt.bare_of FROM bins b
			JOIN bin_types bt ON bt.id = b.bin_type_id
			WHERE b.id = $1 AND bt.bare AND bt.bare_of IS NOT NULL)
		UPDATE bins SET bin_type_id = old.bare_of, updated_at = NOW() FROM old
		WHERE bins.id = $1 RETURNING old.id`, binID).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &marker, nil
}

// RestoreBinType puts a bin's type back, after a refused load that had
// stamped its carrier (StampCarrierForLoad).
func (db *DB) RestoreBinType(binID, typeID int64) error {
	_, err := db.DB.Exec(`UPDATE bins SET bin_type_id = $2, updated_at = NOW() WHERE id = $1`, binID, typeID)
	return err
}

// AcceptsPartialsNodeIDs is every window of a live loader whose windows take
// partly filled carriers (accept_partials), and the group above each, and the
// one above that: the node list marks them so a claim's partial home is never
// one of them.
func (db *DB) AcceptsPartialsNodeIDs() (map[int64]bool, error) {
	rows, err := db.DB.Query(`SELECT n.id, n.parent_id, p.parent_id FROM bin_loader_homes h
		JOIN bin_loaders l ON l.id = h.loader_id AND l.archived_at IS NULL AND l.accept_partials
		JOIN nodes n ON n.id = h.position_node_id
		LEFT JOIN nodes p ON p.id = n.parent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		var parent, grand sql.NullInt64
		if err := rows.Scan(&id, &parent, &grand); err != nil {
			return nil, err
		}
		out[id] = true
		if parent.Valid {
			out[parent.Int64] = true
		}
		if grand.Valid {
			out[grand.Int64] = true
		}
	}
	return out, rows.Err()
}

// LoaderWindowIsPartialHome reports whether any window of the loader stands
// inside some produce claim's partial home: such a loader may not take partly
// filled carriers, which its windows would then be told both to finish and to
// unload.
func (db *DB) LoaderWindowIsPartialHome(loaderID int64) (bool, error) {
	var is bool
	err := db.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM bin_loader_homes h
		JOIN nodes n ON n.id = h.position_node_id
		JOIN style_claims sc ON sc.role = 'produce' AND sc.partial_destination <> ''
		JOIN nodes hn ON hn.name = sc.partial_destination
		WHERE h.loader_id = $1
		  AND (hn.id = n.id OR hn.id = n.parent_id
		       OR hn.id = (SELECT p.parent_id FROM nodes p WHERE p.id = n.parent_id)))`, loaderID).Scan(&is)
	return is, err
}

// HeldPartialInfo is a held carrier's board line: when it entered its home and
// the line whose claim keeps it there. held is false for any other carrier.
func (db *DB) HeldPartialInfo(binID int64) (since time.Time, forLine string, held bool, err error) {
	var s sql.NullTime
	var line sql.NullString
	err = db.DB.QueryRow(`SELECT b.held_since,
		(SELECT sc.core_node_name FROM style_claims sc JOIN nodes hn ON hn.name = sc.partial_destination
		 WHERE sc.payload_code = b.payload_code AND sc.role = 'produce' AND sc.partial_destination <> ''
		   AND (hn.id = b.node_id OR hn.id = n.parent_id
		        OR hn.id = (SELECT p.parent_id FROM nodes p WHERE p.id = n.parent_id))
		 ORDER BY sc.core_node_name LIMIT 1)
		FROM bins b JOIN nodes n ON n.id = b.node_id
		WHERE b.id = $1 AND b.held_since IS NOT NULL`, binID).Scan(&s, &line)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, "", false, nil
	}
	if err != nil {
		return time.Time{}, "", false, err
	}
	return s.Time, line.String, s.Valid, nil
}

// ListHeldPartials is every held partial of the part, oldest held first: the
// list a finish-later claim's REQUEST shows the operator to choose from.
func (db *DB) ListHeldPartials(payloadCode string) ([]protocol.HeldPartial, error) {
	rows, err := db.DB.Query(`SELECT b.id, b.label, n.name, b.uop_remaining, b.held_since
		FROM bins b JOIN nodes n ON n.id = b.node_id
		WHERE b.payload_code = $1 AND b.held_since IS NOT NULL AND `+helpers.HeldPartialSQL("b")+`
		ORDER BY b.held_since, b.id`, payloadCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.HeldPartial{}
	for rows.Next() {
		var h protocol.HeldPartial
		var since time.Time
		if err := rows.Scan(&h.BinID, &h.BinLabel, &h.NodeName, &h.UOPRemaining, &since); err != nil {
			return nil, err
		}
		h.HeldSince = since.UTC().Format(time.RFC3339)
		out = append(out, h)
	}
	return out, rows.Err()
}

// StampHeldSinceDeclaredTx is the declaration's held-since stamp on the
// caller's transaction (helpers.StampHeldSinceDeclaredTx), for service/.
func StampHeldSinceDeclaredTx(tx *sql.Tx, binID int64) error {
	return helpers.StampHeldSinceDeclaredTx(tx, binID)
}
