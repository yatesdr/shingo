package store

import (
	"encoding/json"
	"fmt"

	"shingocore/domain"
	"shingocore/store/audit"
	"shingocore/store/bins"
)

// DeriveTotesAtLift writes what a press build drew from the cart outside it,
// when order orderID lifts that cart (liftedID, which stood on fromNode with
// outsideTotes on it and no part) off a fed press cell's outside position.
// sibling is the lifting order's two-robot sibling (an index), blank for none.
//
// One transaction: one read (bins.ReadToteLiftTx), one write of both carts
// (bins.WriteToteDrawTx), a ledger row per cart carrying the inputs. The
// front's count is trusted at an index only once its manifest is confirmed (the
// produce ingest has landed); otherwise, or with no front to read, the lifted
// cart is written unknown and the front keeps the totes it was read with: it
// holds at least those, so nothing written claims a tote that is not there
// (TestToteLift_AnIndexBeforeTheIngestLanded). A node
// that is no outside position, or a cart this order already derived, writes
// nothing. The caller has already checked the cart carries a number and no
// part, on the row its lift read, so a plant that sets no tote numbers never
// reaches here. Pins: TestToteLift_* (engine/tote_lift_docker_test.go).
func (db *DB) DeriveTotesAtLift(liftedID, orderID int64, fromNode string, outsideTotes int, sibling string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close

	r, err := bins.ReadToteLiftTx(tx, liftedID, orderID, fromNode, sibling)
	if err != nil {
		return err
	}
	if !r.Outside || r.Done {
		return nil
	}
	in := domain.ToteDrawInput{OutsideTotes: &outsideTotes, FrontTotes: r.FrontTotes, UOPPerTote: r.UOPPerTote}
	trusted := r.FrontID != nil && (sibling == "" || r.FrontConfirmed)
	if trusted {
		in.Cycles = r.FrontCount
	}
	d := domain.DeriveToteDraw(in)
	if !trusted {
		d.Front = r.FrontTotes
	}
	detail, err := json.Marshal(map[string]any{
		"from": fromNode, "outside_bin_id": liftedID, "front_bin_id": r.FrontID, "sibling": sibling,
		"count": r.FrontCount, "count_trusted": trusted, "uop_per_tote": in.UOPPerTote,
		"outside_totes_before": outsideTotes, "front_totes_before": in.FrontTotes,
		"outside_totes": d.Outside, "front_totes": d.Front, "overdrawn": d.Overdrawn,
	})
	if err != nil {
		return fmt.Errorf("ledger detail: %w", err)
	}
	front := int64(0)
	if r.FrontID != nil {
		front = *r.FrontID
	}
	written, err := bins.WriteToteDrawTx(tx, liftedID, front, d)
	if err != nil {
		return err
	}
	for _, w := range written {
		uop := w.UOP
		if err := audit.AppendBinUOP(tx, w.ID, &uop, uop, bins.OpTotesDerived,
			"store/tote_lift.go:DeriveTotesAtLift", &orderID, w.PayloadCode, "system",
			audit.BinUOPContext{NodeID: w.NodeID, Detail: detail}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
