package store

import (
	"database/sql"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/store/internal/helpers"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
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
// ── IT USED TO ASK FOUR QUESTIONS AND A STOR NODE NEEDS SIX ───────────────
//
// This was `SELECT … LIMIT 1` over enabled / non-synthetic / unclaimed / empty,
// ordered by name, and the census of destination writers (2026-08-31) found it
// the only site in the plant that consulted no shared predicate at all. The gap
// is not theoretical: `STOR` is the class of a LANE SLOT, not just a flat
// position — 30 of demo.yaml's SMN_* lane slots carry it — so the four
// conditions above are answered "yes" for a slot that
//
//   - sits behind an occupied slot in the same lane, where no robot can reach
//     it (a recovery drop is unattended; the robot arrives, cannot place, and
//     stands there), and
//   - another live order is already driving a bin to.
//
// So it now asks the two questions every other destination reader asks, in the
// same words they use: helpers.ReachableSQL for "can a robot get to it", and the
// reservation + delivery_node pair for "has somebody else already spoken for
// it". Owner-blind on purpose — the recovery order does not exist yet, so there
// is no owner to exempt.
//
// ORDER: DEEPEST FIRST, then name. The old comment argued name-order was right
// because "there is no best free slot here" — true of flat positions and false
// inside a lane, where filling the mouth before the back is how a lane bubbles.
// Depth-descending is the same back-to-front packing findStoreSlot uses; the
// name tiebreak keeps the answer stable for the flat positions the original
// sentence was about. A NULL depth (a flat STOR node) sorts with the flats.
func (db *DB) FindEmptyStorageNodeForPayload(payloadCode string) (*nodes.Node, error) {
	if payloadCode == "" {
		return nil, nil
	}
	row := db.QueryRow(fmt.Sprintf(`
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
		  AND NOT `+reservations.SlotSpokenForByStrangerSQL("r", "n.id", "0")+`
		  AND NOT EXISTS (
			SELECT 1 FROM orders o
			WHERE o.delivery_node = n.name
			  AND o.status NOT IN (%s)
		  )
		  AND `+helpers.ReachableSQL("n")+`
		ORDER BY COALESCE(n.depth, 0) DESC, n.name
		LIMIT 1`, protocol.TerminalStatusSQLList()), protocol.NodeClassSTOR, payloadCode)
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
