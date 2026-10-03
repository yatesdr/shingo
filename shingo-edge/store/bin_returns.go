package store

// bin_returns.go — what became of the bin a CANCELLED order left on a robot's
// deck, as Core reports it on protocol.SubjectBinReturn.
//
// DISPLAY ONLY. One row per cancelled order (keyed by its uuid), read by the
// orders board and the changeover row to print one line. Nothing decides
// anything from it: the cancelled order is already terminal here, and this
// table moves no order status and no lineside count.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"shingo/protocol"
)

// BinReturn is one cancelled order's bin-return notice, latest state that
// stood (see BinReturnSupersedes).
type BinReturn struct {
	OrderUUID   string
	BinLabel    string
	PayloadCode string
	State       string // protocol.BinReturnReturning | Returned | Held
	Destination string
	Reason      string
	UpdatedAt   time.Time
}

// binReturnRank orders the states for BinReturnSupersedes. Zero is a state
// this Edge does not know.
func binReturnRank(state string) int {
	switch state {
	case protocol.BinReturnReturning:
		return 1
	case protocol.BinReturnHeld:
		return 2
	case protocol.BinReturnReturned:
		return 3
	}
	return 0
}

// BinReturnSupersedes reports whether an incoming notice in state incoming may
// replace a stored one in state stored. THE ONE SPELLING OF THE PRECEDENCE
// RULE. Notices can arrive out of order, so:
//
//   - returned and held are final: a late returning never overwrites either;
//   - held never overwrites returned (the bin is back; a stale hold is not news);
//   - returned overwrites held (Core should not send it, but if the bin did get
//     back, that is the fact to show);
//   - a repeat of the same state replaces the row (a fresher destination or
//     reason).
//
// An unknown incoming state never supersedes anything.
func BinReturnSupersedes(stored, incoming string) bool {
	in := binReturnRank(incoming)
	return in > 0 && in >= binReturnRank(stored)
}

// UpsertBinReturn stores r unless the row already standing for its order
// outranks it (BinReturnSupersedes). Reports whether r was written.
func (db *DB) UpsertBinReturn(r BinReturn) (bool, error) {
	if r.OrderUUID == "" {
		return false, errors.New("bin return: empty order uuid")
	}
	if binReturnRank(r.State) == 0 {
		return false, fmt.Errorf("bin return: unknown state %q", r.State)
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = time.Now()
	}
	wrote := false
	err := db.InTx(func(tx *sql.Tx) error {
		var stored string
		err := tx.QueryRow(`SELECT state FROM bin_returns WHERE order_uuid = ?`, r.OrderUUID).Scan(&stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && !BinReturnSupersedes(stored, r.State) {
			return nil
		}
		_, err = tx.Exec(`INSERT INTO bin_returns
			(order_uuid, bin_label, payload_code, state, destination, reason, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (order_uuid) DO UPDATE SET
				bin_label = excluded.bin_label, payload_code = excluded.payload_code,
				state = excluded.state, destination = excluded.destination,
				reason = excluded.reason, updated_at = excluded.updated_at`,
			r.OrderUUID, r.BinLabel, r.PayloadCode, r.State, r.Destination, r.Reason,
			r.UpdatedAt.UTC().Format(time.RFC3339Nano))
		wrote = err == nil
		return err
	})
	if err != nil {
		return false, fmt.Errorf("write bin return for order %s: %w", r.OrderUUID, err)
	}
	return wrote, nil
}

// BinReturnsForOrders returns the stored notice for each of uuids that has
// one, keyed by order uuid. One read.
func (db *DB) BinReturnsForOrders(uuids []string) (map[string]BinReturn, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	args := make([]any, len(uuids))
	for i, u := range uuids {
		args[i] = u
	}
	rows, err := db.Query(`SELECT order_uuid, bin_label, payload_code, state, destination, reason, updated_at
		FROM bin_returns WHERE order_uuid IN (?`+strings.Repeat(",?", len(uuids)-1)+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read bin returns: %w", err)
	}
	defer rows.Close()
	out := make(map[string]BinReturn)
	for rows.Next() {
		var (
			r  BinReturn
			at string
		)
		if err := rows.Scan(&r.OrderUUID, &r.BinLabel, &r.PayloadCode, &r.State,
			&r.Destination, &r.Reason, &at); err != nil {
			return nil, fmt.Errorf("scan bin return: %w", err)
		}
		if t, perr := time.Parse(time.RFC3339Nano, at); perr == nil {
			r.UpdatedAt = t
		}
		out[r.OrderUUID] = r
	}
	return out, rows.Err()
}
