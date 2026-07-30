package store

import (
	"fmt"

	"shingo/protocol"
)

// supply_refusals.go — Core's record of the one inventory fact it cannot compute.
//
// Every other statement on this subject is derivable here: how many bins exist,
// what a style claims, whether a pool is dry. "There are none" is not. Shingo's
// coverage is a SUBSET of the greater Martinrea system, so only a person who
// walked the floor can say it — and this table is where that person's statement
// lands.
//
// HISTORY, not open state. The edge mirror deletes on resolution because it
// renders live cards; Core keeps the row and stamps closed_at, which is the same
// division demand_origins already draws.

// ApplySupplyRefusal folds one refusal message into the record.
//
// PER-ACTION FIELD APPLICATION, and it is what makes a two-author row safe. The
// loader's edge opens the refusal; the cell's edge answers it; on a multi-edge
// line those are different boxes writing the same card. Because each action
// touches a disjoint set of columns, they merge without a revision counter and
// without either being able to erase the other.
func (db *DB) ApplySupplyRefusal(st protocol.SupplyRefusalState, stationID string) error {
	switch st.Action {
	case protocol.SupplyRefusalOpened:
		// Idempotent on the OPEN row. A second press is the same statement said
		// twice — it must not mint a second row and must not restart the clock,
		// because the timestamp is what the cell was told.
		_, err := db.Exec(`
			INSERT INTO supply_refusals (loader_node, payload_code, station_id, refused_at, refused_by)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (loader_node, payload_code) WHERE closed_at IS NULL DO NOTHING`,
			st.LoaderNode, st.PayloadCode, stationID, st.RefusedAt, st.RefusedBy)
		if err != nil {
			return fmt.Errorf("apply supply refusal opened %s/%s: %w", st.LoaderNode, st.PayloadCode, err)
		}
		return nil

	case protocol.SupplyRefusalAcked:
		// First answer wins, guarded on ack_at IS NULL — a second can only be a
		// double-tap or a second screen, and the operator's first answer is real.
		_, err := db.Exec(`
			UPDATE supply_refusals
			   SET ack_at = $3, ack_choice = $4, ack_process_id = $5, updated_at = NOW()
			 WHERE loader_node = $1 AND payload_code = $2
			   AND closed_at IS NULL AND ack_at IS NULL`,
			st.LoaderNode, st.PayloadCode, st.AckAt, st.AckChoice, st.AckProcessID)
		if err != nil {
			return fmt.Errorf("apply supply refusal acked %s/%s: %w", st.LoaderNode, st.PayloadCode, err)
		}
		return nil

	case protocol.SupplyRefusalClosed:
		_, err := db.Exec(`
			UPDATE supply_refusals SET closed_at = NOW(), updated_at = NOW()
			 WHERE loader_node = $1 AND payload_code = $2 AND closed_at IS NULL`,
			st.LoaderNode, st.PayloadCode)
		if err != nil {
			return fmt.Errorf("apply supply refusal closed %s/%s: %w", st.LoaderNode, st.PayloadCode, err)
		}
		return nil
	}
	// ACCEPT AND LOG is the house rule on this seam, but an unknown ACTION is
	// different from an unknown episode key: there is nothing to store, because
	// the action is what says which columns the message is about. Refusing it
	// tells the caller rather than writing something arbitrary.
	return fmt.Errorf("supply refusal: unknown action %q for %s/%s",
		st.Action, st.LoaderNode, st.PayloadCode)
}

// ListOpenSupplyRefusals returns every refusal still standing — the set Core
// broadcasts to the edges, and what a late-joining edge needs to be current.
func (db *DB) ListOpenSupplyRefusals() ([]protocol.SupplyRefusalState, error) {
	rows, err := db.Query(`
		SELECT loader_node, payload_code, refused_at, refused_by, ack_at, ack_choice, ack_process_id
		  FROM supply_refusals WHERE closed_at IS NULL ORDER BY refused_at`)
	if err != nil {
		return nil, fmt.Errorf("list open supply refusals: %w", err)
	}
	defer rows.Close()
	var out []protocol.SupplyRefusalState
	for rows.Next() {
		st := protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened}
		if err := rows.Scan(&st.LoaderNode, &st.PayloadCode, &st.RefusedAt, &st.RefusedBy,
			&st.AckAt, &st.AckChoice, &st.AckProcessID); err != nil {
			return nil, fmt.Errorf("scan supply refusal: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
