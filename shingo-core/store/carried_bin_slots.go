package store

import (
	"database/sql"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/store/nodes"
)

// FindEmptyStorageNodeForPayload returns a storage slot that will accept this
// payload and currently holds nothing.
//
// It is the second tier of the carried-bin recovery's destination fallback:
// "where does a bin of this kind belong". Deliberately NARROW — a concrete,
// enabled, non-synthetic STOR node with an explicit node_payloads link to this
// payload — because a recovery drop is unattended and the cost of a wrong
// answer is a bin somewhere nobody looks for it. A node that has not been told
// it accepts this payload is not a candidate.
//
// Excludes claimed slots and occupied ones. The occupancy read is not a
// reservation: the caller creates an order whose dispatch takes the real slot
// reservation (ReserveStorageDropoff), and a slot filled in between is that
// path's refusal to make, not this one's.
//
// ORDERED BY NAME so two calls agree. There is no "best" free slot here and
// pretending otherwise — nearest, emptiest, most recently used — would be a
// policy nobody asked for; a stable answer is worth more than a clever one.
func (db *DB) FindEmptyStorageNodeForPayload(payloadCode string) (*nodes.Node, error) {
	if payloadCode == "" {
		return nil, nil
	}
	row := db.QueryRow(`
		SELECT n.id, n.name, n.is_synthetic, n.zone, n.enabled
		FROM nodes n
		JOIN node_types nt      ON nt.id = n.node_type_id
		JOIN node_payloads np   ON np.node_id = n.id
		JOIN payloads p         ON p.id = np.payload_id
		WHERE nt.code = $1
		  AND p.code = $2
		  AND n.enabled = true
		  AND n.is_synthetic = false
		  AND n.claimed_by IS NULL
		  AND NOT EXISTS (SELECT 1 FROM bins b WHERE b.node_id = n.id)
		ORDER BY n.name
		LIMIT 1`, protocol.NodeClassSTOR, payloadCode)
	var n nodes.Node
	if err := row.Scan(&n.ID, &n.Name, &n.IsSynthetic, &n.Zone, &n.Enabled); err != nil {
		// NO ROW AND A FAILED READ ARE DIFFERENT ANSWERS. Both used to return
		// (nil, nil), so the error return was dead and a database that could not
		// answer was indistinguishable from a plant with no linked storage node
		// for this payload. The caller falls to the next destination tier either
		// way — that behaviour is deliberate and unchanged, because a recovery
		// that refuses outright leaves the bin on the deck — but an outage now
		// reaches the log and the returned error instead of reading as
		// configuration.
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find empty storage node for payload %q: %w", payloadCode, err)
	}
	return &n, nil
}
