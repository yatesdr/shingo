package bins

import (
	"database/sql"
	"errors"
	"fmt"

	"shingocore/domain"
	"shingocore/store/internal/helpers"
	"shingocore/store/internal/nodetree"
)

// bare_cart.go — the two-stage unloader's cart, in the store: the stamp a CLEAR
// makes inside its own transaction, the pull's candidate list, and the type
// code an operator may see.

// BareStamp is domain.BareStamp: what a CLEAR does to the cart's type.
type BareStamp = domain.BareStamp

// The three stamps, re-exported for callers inside store/.
const (
	StampNone    = domain.StampNone
	StampMarker  = domain.StampMarker
	StampCarrier = domain.StampCarrier
)

// ResolveBareStampTx returns the type a clear of binID stamps under stamp, or
// nil for "leave the type as it is". It locks the bin row, so the type it
// reads is the type the clear then overwrites.
func ResolveBareStampTx(tx *sql.Tx, binID int64, stamp BareStamp) (*int64, error) {
	if stamp == StampNone {
		return nil, nil
	}
	var typeID int64
	var bareOf sql.NullInt64
	if err := tx.QueryRow(`SELECT b.bin_type_id, bt.bare_of FROM bins b
		JOIN bin_types bt ON bt.id = b.bin_type_id WHERE b.id=$1 FOR UPDATE OF b`, binID).Scan(&typeID, &bareOf); err != nil {
		return nil, fmt.Errorf("read cart type of bin %d: %w", binID, err)
	}
	switch stamp {
	case StampMarker:
		id, err := EnsureBareMarkerTx(tx, typeID)
		if err != nil {
			return nil, err
		}
		return &id, nil
	case StampCarrier:
		if !bareOf.Valid {
			return nil, nil
		}
		return &bareOf.Int64, nil
	}
	return nil, fmt.Errorf("unknown bare stamp %d", stamp)
}

// RealTypeCode is the code of the bin's cart type as an operator knows it: the
// carrier's code when the bin is bare, its own code otherwise. A marker code
// never reaches a screen.
func RealTypeCode(db *sql.DB, binID int64) (string, error) {
	var code string
	err := db.QueryRow(`SELECT COALESCE(c.code, bt.code) FROM bins b
		JOIN bin_types bt ON bt.id = b.bin_type_id
		LEFT JOIN bin_types c ON c.id = bt.bare_of
		WHERE b.id=$1`, binID).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return code, err
}

// ListBareInGroup returns the bare carts standing in a group's subtree that a
// stage-2 pull may move: unclaimed, unreserved, unlocked, not on quality hold,
// of a sourceable status, at a live node, and reachable (nothing in front of it
// in a lane). Oldest first by updated_at — the arrival in the group is the last
// write a waiting cart gets, since nothing else touches a bare cart parked
// there — then id.
//
// It is NOT an empty finder and composes none of EmptyCarrierWhere: that
// predicate says a bare cart is nobody's empty, and it stays absolute. This
// list is read only by the pull, which names the cart it moves.
func ListBareInGroup(db *sql.DB, groupNodeID int64) ([]*Bin, error) {
	q := nodetree.DescendantsOf(1) + " " + BinJoinQuery + `
	WHERE bt.bare
	  AND ` + SourceableStatusSQL + `
	  AND ` + helpers.BinUnheldSQL + `
	  AND b.node_id IN (SELECT id FROM descendants)
	  AND ` + BinAtLiveNodeSQL + `
	  AND NOT COALESCE(b.quality_hold, false)
	  AND (n.parent_id IS NULL OR n.depth IS NULL OR ` + helpers.ReachableSQL("n") + `)
	ORDER BY b.updated_at, b.id`
	rows, err := db.Query(q, groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("list bare carts in group %d: %w", groupNodeID, err)
	}
	defer rows.Close()
	var out []*Bin
	for rows.Next() {
		b, err := ScanBin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
