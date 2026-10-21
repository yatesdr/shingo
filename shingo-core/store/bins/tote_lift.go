package bins

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
)

// tote_lift.go — what a press build drew from the cart outside it, written
// when that cart is lifted.

// OpTotesDerived tags the two bin_uop_ledger rows the lift writes when it
// derives the totes on a press cell's two carts (store.DB.DeriveTotesAtLift).
// before_uop == after_uop: it changes no count and is no binding boundary.
// detail carries the derivation's inputs and its answer. Kept beside the read
// that checks for it (once per cart and order); the other op tags live in
// store/audit.
const OpTotesDerived = "totes_derived"

// toteLiftSQL reads, for the cart $2 just lifted off node $1 by order $4 (whose
// two-robot sibling is $3, blank for none):
//   - whether $1 is the outside position of a press cell fed from behind
//     (FedClaimSQL; a sequential pair's partner and a second paired position
//     are not);
//   - whether this (cart, order) has already been derived (op $5);
//   - the FRONT, always by id: the cart standing on that cell's press, else
//     the bin the sibling lifted off the press (its order_bins pickup there,
//     else its bin pointer when the sibling is the press's own order);
//   - the front's count, whether its manifest is confirmed, its totes, and its
//     part's UOP per tote.
var toteLiftSQL = `
	WITH cell AS (
		SELECT sc.core_node_name AS core
		  FROM style_claims sc
		  LEFT JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
		 WHERE sc.paired_core_node = $1
		   AND ` + FedClaimSQL("sc") + `
		 ORDER BY COALESCE(ps.is_active, false) DESC, sc.core_node_name
		 LIMIT 1),
	front AS (
		SELECT COALESCE(
			(SELECT b.id FROM bins b JOIN nodes n ON n.id = b.node_id JOIN cell ON n.name = cell.core
			  WHERE b.id <> $2 ORDER BY b.id DESC LIMIT 1),
			(SELECT ob.bin_id FROM orders s
			   JOIN order_bins ob ON ob.order_id = s.id
			   JOIN cell ON ob.node_name = cell.core
			  WHERE $3 <> '' AND s.edge_uuid = $3 AND ob.action = 'pickup' AND ob.bin_id <> $2
			  ORDER BY ob.step_index LIMIT 1),
			(SELECT s.bin_id FROM orders s JOIN cell ON s.process_node = cell.core
			  WHERE $3 <> '' AND s.edge_uuid = $3 AND s.bin_id <> $2)) AS id)
	SELECT EXISTS (SELECT 1 FROM cell),
	       EXISTS (SELECT 1 FROM bin_uop_ledger g WHERE g.bin_id = $2 AND g.order_id = $4 AND g.op = $5),
	       f.id, f.totes_on_cart, f.uop_remaining, f.manifest_confirmed, p.uop_per_tote
	  FROM front
	  LEFT JOIN bins f ON f.id = front.id
	  LEFT JOIN payloads p ON p.code = f.payload_code`

// ToteLift is what ReadToteLiftTx read for a cart lifted off a node.
type ToteLift struct {
	// Outside: the node is a fed press cell's outside position.
	Outside bool
	// Done: this (cart, order) has already been derived.
	Done bool
	// FrontID is the front cart, by id; nil when there is none to read.
	FrontID *int64
	// FrontTotes, FrontCount and FrontConfirmed are the front's totes_on_cart,
	// uop_remaining and manifest_confirmed; UOPPerTote its part's.
	FrontTotes     *int
	FrontCount     *int
	FrontConfirmed bool
	UOPPerTote     *int
}

// ReadToteLiftTx is the lift hook's one read (toteLiftSQL): cart liftedID,
// lifted by order orderID (two-robot sibling: sibling, blank for none) off
// fromNode.
func ReadToteLiftTx(tx *sql.Tx, liftedID, orderID int64, fromNode, sibling string) (ToteLift, error) {
	var (
		l                        ToteLift
		front, totes, count, per sql.NullInt64
		confirmed                sql.NullBool
	)
	if err := tx.QueryRow(toteLiftSQL, fromNode, liftedID, sibling, orderID, OpTotesDerived).Scan(
		&l.Outside, &l.Done, &front, &totes, &count, &confirmed, &per); err != nil {
		return ToteLift{}, fmt.Errorf("read the cell at %s: %w", fromNode, err)
	}
	if front.Valid {
		l.FrontID = &front.Int64
	}
	l.FrontTotes, l.FrontCount, l.UOPPerTote = intOrNil(totes), intOrNil(count), intOrNil(per)
	l.FrontConfirmed = confirmed.Bool
	return l, nil
}

// TotesWritten is one cart WriteToteDrawTx wrote, with what its ledger row
// needs.
type TotesWritten struct {
	ID          int64
	UOP         int
	PayloadCode string
	NodeID      *int64
}

// WriteToteDrawTx writes the derivation on both carts in one statement: the
// lifted cart's totes and the front's (front 0 for no front: the lifted cart
// alone). It returns the rows written.
func WriteToteDrawTx(tx *sql.Tx, liftedID, front int64, d domain.ToteDraw) ([]TotesWritten, error) {
	rows, err := tx.Query(`UPDATE bins SET
		    totes_on_cart = CASE id WHEN $1 THEN $2::int ELSE $4::int END,
		    updated_at = NOW()
		WHERE id IN ($1, $3)
		RETURNING id, uop_remaining, COALESCE(payload_code, ''), node_id`,
		liftedID, d.Outside, front, d.Front)
	if err != nil {
		return nil, fmt.Errorf("write the totes on carts %d and %d: %w", liftedID, front, err)
	}
	defer rows.Close()
	var out []TotesWritten
	for rows.Next() {
		var (
			w    TotesWritten
			node sql.NullInt64
		)
		if err := rows.Scan(&w.ID, &w.UOP, &w.PayloadCode, &node); err != nil {
			return nil, fmt.Errorf("read back the totes written: %w", err)
		}
		if node.Valid {
			w.NodeID = &node.Int64
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func intOrNil(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}
