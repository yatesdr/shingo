package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store/bins"
	"shingocore/store/reservations"
)

// HeldPickupExchange is one same-part claim exchange at a dig
// (dispatch/claim_exchange.go): the holder gives up the mouth bin it
// hard-claims for a pickup its robot has not reached, and takes the bin behind
// it; the digger, not yet dispatched, takes a hold on the mouth bin. Every
// field is what the decision read; the transaction re-checks each under locks.
type HeldPickupExchange struct {
	HolderID, DiggerID      int64
	MouthBin, DeepBin       int64
	MouthNodeID, DeepNodeID int64
	DeepNodeName            string
	HolderWaitIndex         int
	HolderUpdatedAt         time.Time
	HolderStepsWas          string
	HolderStepsNow          string
	DiggerStepsWas          string
	DiggerStepsNow          string
	HolderNote, DiggerNote  string
	MouthNote, DeepNote     string
}

// claimExchangeActor names the exchange on the reservation rows and the ledger.
const claimExchangeActor = "claim-exchange"

// ErrExchangeRefused is an exchange whose re-check failed: nothing is written,
// and the caller parks the digger as it would have. The wrapped text says
// which re-check.
var ErrExchangeRefused = errors.New("claim exchange refused")

// ExchangeHeldPickup writes a claim exchange in ONE transaction, or nothing.
// It returns false and an error wrapping ErrExchangeRefused when a re-check
// refuses it:
//
//   - the holder is still at the wait index, plan and updated_at the decision
//     read (a release whose append CAS landed in between bumped updated_at,
//     orders.ClaimAppend), and non-terminal;
//   - the digger is still acquiring with the plan the decision read;
//   - the mouth bin is still at its slot, unlocked, claimed by the holder, and
//     held by no other order; the deep bin still at its slot, unlocked,
//     unclaimed, and held by nobody but the digger.
//
// Then: the holder's claim and hold leave the mouth bin; the holder holds and
// claims the deep bin (the ordinary reserve, claim CAS and confirm); the
// digger holds the mouth bin, pending, as its own plan would; the holder's
// plan, bin pointer (when it named the mouth bin) and junction row are
// re-pointed; the digger's plan names the mouth slot; and one claim_exchanged
// ledger row is written on each order and each bin. The ledger is audit_log,
// not order_history: a history row carries the order's status, and one more
// `staged` row on the holder would read as a new staging to every dwell
// reader.
func (db *DB) ExchangeHeldPickup(ex HeldPickupExchange) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, fmt.Errorf("claim exchange: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	if err := exchangeHeldPickupTx(tx, ex); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("claim exchange: commit: %w", err)
	}
	return true, nil
}

func exchangeHeldPickupTx(tx *sql.Tx, ex HeldPickupExchange) error {
	now := clock.Now().UTC()
	refused := func(why string) error { return fmt.Errorf("%w: %s", ErrExchangeRefused, why) }
	one := func(why, q string, args ...any) error {
		res, err := tx.Exec(q, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return refused(why)
		}
		return nil
	}

	if err := one("the holder changed since the decision read it", fmt.Sprintf(`UPDATE orders SET steps_json=$1,
		bin_id = CASE WHEN bin_id=$2 THEN $3 ELSE bin_id END, updated_at=$4
		WHERE id=$5 AND wait_index=$6 AND updated_at=$7 AND steps_json=$8 AND status NOT IN (%s)`,
		protocol.TerminalStatusSQLList()),
		ex.HolderStepsNow, ex.MouthBin, ex.DeepBin, now, ex.HolderID, ex.HolderWaitIndex, ex.HolderUpdatedAt,
		ex.HolderStepsWas); err != nil {
		return err
	}
	if err := one("the digger changed since the decision read it", fmt.Sprintf(`UPDATE orders SET steps_json=$1, updated_at=$2
		WHERE id=$3 AND steps_json=$4 AND status IN (%s)`, protocol.AcquiringStatusSQLList()),
		ex.DiggerStepsNow, now, ex.DiggerID, ex.DiggerStepsWas); err != nil {
		return err
	}

	var mouthOK, deepOK bool
	if err := tx.QueryRow(`SELECT
		  bool_or(id=$1 AND node_id=$3 AND NOT locked AND claimed_by=$5),
		  bool_or(id=$2 AND node_id=$4 AND NOT locked AND claimed_by IS NULL)
		FROM (SELECT id, node_id, locked, claimed_by FROM bins WHERE id IN ($1,$2) ORDER BY id FOR UPDATE) b`,
		ex.MouthBin, ex.DeepBin, ex.MouthNodeID, ex.DeepNodeID, ex.HolderID).Scan(&mouthOK, &deepOK); err != nil {
		return err
	}
	if !mouthOK || !deepOK {
		return refused("a bin moved, locked or changed hands")
	}
	var strangers int
	if err := tx.QueryRow(`SELECT count(*) FROM reservations
		WHERE resource_kind=$1 AND `+reservations.ActiveStateSQL("")+`
		  AND ((bin_id=$2 AND order_id<>$4) OR (bin_id=$3 AND order_id<>$5))`,
		string(reservations.KindBin), ex.MouthBin, ex.DeepBin, ex.HolderID, ex.DiggerID).Scan(&strangers); err != nil {
		return err
	}
	if strangers != 0 {
		return refused("another order holds one of the bins")
	}

	if err := one("the mouth bin's claim moved", `UPDATE bins SET claimed_by=NULL, updated_at=$3 WHERE id=$1 AND claimed_by=$2`,
		ex.MouthBin, ex.HolderID, now); err != nil {
		return err
	}
	if err := reservations.ReleaseByBinForOrder(tx, ex.MouthBin, ex.HolderID); err != nil {
		return err
	}
	if err := reservations.Release(tx, ex.DiggerID, ex.DeepBin); err != nil {
		return err
	}
	if err := reservations.Acquire(tx, ex.HolderID, ex.HolderID, ex.DeepBin, claimExchangeActor); err != nil {
		return refusedOr(err, refused)
	}
	if err := bins.ClaimTx(tx, ex.DeepBin, ex.HolderID); err != nil {
		return refused(err.Error())
	}
	if err := reservations.Confirm(tx, ex.HolderID, ex.DeepBin); err != nil {
		return err
	}
	if err := reservations.Acquire(tx, ex.DiggerID, ex.DiggerID, ex.MouthBin, claimExchangeActor); err != nil {
		return refusedOr(err, refused)
	}
	if _, err := tx.Exec(`UPDATE order_bins SET bin_id=$3, node_name=$4 WHERE order_id=$1 AND bin_id=$2`,
		ex.HolderID, ex.MouthBin, ex.DeepBin, ex.DeepNodeName); err != nil {
		return err
	}
	for _, row := range []struct {
		entity string
		id     int64
		note   string
	}{
		{"order", ex.HolderID, ex.HolderNote}, {"order", ex.DiggerID, ex.DiggerNote},
		{"bin", ex.MouthBin, ex.MouthNote}, {"bin", ex.DeepBin, ex.DeepNote},
	} {
		if _, err := tx.Exec(`INSERT INTO audit_log (entity_type, entity_id, action, old_value, new_value, actor)
			VALUES ($1, $2, 'claim_exchanged', '', $3, $4)`, row.entity, row.id, row.note, claimExchangeActor); err != nil {
			return err
		}
	}
	return nil
}

// refusedOr reads a reservation refusal (a conflict, an owner gone, a dug
// lane) as a refused exchange, and anything else as the error it is.
func refusedOr(err error, refused func(string) error) error {
	if errors.Is(err, reservations.ErrReservationConflict) || errors.Is(err, reservations.ErrOwnerEnded) ||
		errors.Is(err, reservations.ErrLaneDugByAnother) || errors.Is(err, reservations.ErrNotTheNamedBin) {
		return refused(err.Error())
	}
	return err
}
