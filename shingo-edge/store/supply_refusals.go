package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/store/messaging"
)

// supply_refusals.go — a loader operator's standing statement that they cannot
// fill a call, and the cell's answer to it.
//
// OPEN-STATE ONLY. A row exists while a refusal stands and is DELETED when it
// resolves — the same shape as demand_origins_open, for the reason that file
// states: the history belongs on Core, and a second local copy of the same facts
// starts drifting from what it summarises.
//
// Keyed on the CARD: (loader_node, payload_code). That is the whole surface the
// reach-truck operator is standing at, and both board layouts reduce to it — a
// shared window renders one card per payload, a dedicated home one card per
// position. No layout branch reaches this file.

// SupplyRefusal is one open refusal, in full.
type SupplyRefusal struct {
	LoaderNode  string
	PayloadCode string

	RefusedAt time.Time
	// RefusedBy is STATION-level. The loader board carries no operator identity,
	// so this is the station name, not a person. Named honestly rather than
	// dressed up as attribution it cannot make.
	RefusedBy string

	// AckAt is nil until the cell answers. THAT NIL IS A REAL STATE — "told, not
	// answered" — and it is the state the whole project started from: the
	// information was on a screen and nobody had looked at it.
	AckAt        *time.Time
	AckChoice    string // "" | "wait" | "changeover"
	AckProcessID string // the process NAME of the cell that ANSWERED
}

// Answered reports whether the customer has responded. Distinct from "refused":
// a refusal with no answer is the case that needs surfacing.
func (r SupplyRefusal) Answered() bool { return r.AckAt != nil }

const supplyRefusalCols = `loader_node, payload_code, refused_at, refused_by,
	ack_at, ack_choice, ack_process_id`

func scanSupplyRefusal(sc interface{ Scan(...any) error }) (*SupplyRefusal, error) {
	var (
		r         SupplyRefusal
		refusedAt string
		ackAt     sql.NullString
	)
	if err := sc.Scan(&r.LoaderNode, &r.PayloadCode, &refusedAt, &r.RefusedBy,
		&ackAt, &r.AckChoice, &r.AckProcessID); err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, refusedAt); err == nil {
		r.RefusedAt = t
	}
	if ackAt.Valid && ackAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, ackAt.String); err == nil {
			r.AckAt = &t
		}
	}
	return &r, nil
}

// OpenSupplyRefusal records a refusal for one card.
//
// IDEMPOTENT ON THE CARD KEY, and that is a floor requirement rather than
// defensive habit: a second press must not mint a second row or restart the
// clock on the first. The operator pressing again is the same statement said
// twice, and the refusal they already made is the one that stands — so the
// insert leaves an existing row completely alone, including its ack.
func (db *DB) OpenSupplyRefusal(loaderNode, payloadCode, refusedBy string) error {
	_, err := db.Exec(
		`INSERT INTO supply_refusals_open (`+supplyRefusalCols+`)
		 VALUES (?,?,?,?,NULL,'','')
		 ON CONFLICT(loader_node, payload_code) DO NOTHING`,
		loaderNode, payloadCode, time.Now().UTC().Format(time.RFC3339Nano), refusedBy)
	if err != nil {
		return fmt.Errorf("open supply refusal %s/%s: %w", loaderNode, payloadCode, err)
	}
	return nil
}

// ErrNoOpenRefusal is returned when no refusal stands for a card.
var ErrNoOpenRefusal = errors.New("no open supply refusal for that card")

// GetSupplyRefusal returns the open refusal for one card, or ErrNoOpenRefusal.
func (db *DB) GetSupplyRefusal(loaderNode, payloadCode string) (*SupplyRefusal, error) {
	r, err := scanSupplyRefusal(db.QueryRow(
		`SELECT `+supplyRefusalCols+` FROM supply_refusals_open
		  WHERE loader_node = ? AND payload_code = ?`, loaderNode, payloadCode))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoOpenRefusal
	}
	if err != nil {
		return nil, fmt.Errorf("get supply refusal %s/%s: %w", loaderNode, payloadCode, err)
	}
	return r, nil
}

// ListOpenSupplyRefusals returns every open refusal.
//
// The board reads this ONCE PER POLL and indexes it in memory. The table holds
// only what is open — one row per card actually refused right now, which on a
// real plant is a handful — so a whole-table read is cheaper than a query per
// card, and the board render path is the one place that difference is felt.
func (db *DB) ListOpenSupplyRefusals() ([]SupplyRefusal, error) {
	return listOpenSupplyRefusals(db)
}

func listOpenSupplyRefusals(q interface {
	Query(query string, args ...any) (*sql.Rows, error)
}) ([]SupplyRefusal, error) {
	rows, err := q.Query(`SELECT ` + supplyRefusalCols + ` FROM supply_refusals_open`)
	if err != nil {
		return nil, fmt.Errorf("list open supply refusals: %w", err)
	}
	defer rows.Close()
	var out []SupplyRefusal
	for rows.Next() {
		r, err := scanSupplyRefusal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan supply refusal: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// AckSupplyRefusal records the customer's answer.
//
// FIRST ANSWER WINS — the UPDATE is guarded on ack_at IS NULL. The modal fires
// on "refused and not yet answered", so a second write could only come from a
// double-tap or two tabs on the same board, and the operator's first answer is
// the real one. Returns whether a row was actually updated so the caller can
// tell "recorded" from "already answered" rather than reporting both as success.
func (db *DB) AckSupplyRefusal(loaderNode, payloadCode, choice, processID string) (bool, error) {
	res, err := db.Exec(
		`UPDATE supply_refusals_open
		    SET ack_at = ?, ack_choice = ?, ack_process_id = ?
		  WHERE loader_node = ? AND payload_code = ? AND ack_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), choice, processID, loaderNode, payloadCode)
	if err != nil {
		return false, fmt.Errorf("ack supply refusal %s/%s: %w", loaderNode, payloadCode, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteSupplyRefusal removes the open refusal for one card.
//
// THE ANSWER GOES WITH IT, and that is deliberate. The refusal and the reply to
// it are one episode; a row left behind carrying ack_choice='wait' after its
// refusal was withdrawn would make the NEXT refusal look already-answered, and
// the modal would never fire for it.
//
// Two callers, both ending the same episode: a LOAD at that window for that
// payload (the normal path — the parts arrived, the operator loads them, the
// card goes back to normal) and UNDO (the mis-tap path).
//
// Returns whether a row was deleted, so clear-on-LOAD tells the plant only
// when there was a refusal to end rather than on every LOAD.
func (db *DB) DeleteSupplyRefusal(loaderNode, payloadCode string) (bool, error) {
	res, err := db.Exec(
		`DELETE FROM supply_refusals_open WHERE loader_node = ? AND payload_code = ?`,
		loaderNode, payloadCode)
	if err != nil {
		return false, fmt.Errorf("delete supply refusal %s/%s: %w", loaderNode, payloadCode, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SupplyRefusalMerge decides, from the open set read inside the transaction,
// the set to keep and the encoded supply.refusal messages to enqueue with it.
type SupplyRefusalMerge func(open []SupplyRefusal) (keep []SupplyRefusal, outbox [][]byte, err error)

// ReconcileSupplyRefusals reads the open set, hands it to merge, and replaces
// the table with what merge keeps, enqueuing merge's messages, in ONE
// transaction. The read is inside it so an operator's refuse, answer or undo
// landing between the read and the write cannot be overwritten by a merge that
// never saw it; the messages are inside it so a kept row and the message that
// re-sends it to Core commit together or not at all. An error from merge rolls
// everything back and leaves the table as it was.
func (db *DB) ReconcileSupplyRefusals(merge SupplyRefusalMerge) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("reconcile supply refusals: begin: %w", err)
	}
	defer tx.Rollback()
	open, err := listOpenSupplyRefusals(tx)
	if err != nil {
		return err
	}
	keep, outbox, err := merge(open)
	if err != nil {
		return err
	}
	if err := replaceSupplyRefusals(tx, keep); err != nil {
		return err
	}
	for _, p := range outbox {
		if _, err := messaging.EnqueueIn(tx, p, protocol.SubjectSupplyRefusal); err != nil {
			return fmt.Errorf("reconcile supply refusals: enqueue: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reconcile supply refusals: commit: %w", err)
	}
	if len(outbox) > 0 {
		messaging.NotifyEnqueued()
	}
	return nil
}

// replaceSupplyRefusals rewrites the open table as rows. The table holds a
// handful of rows, one per card refused right now, and it is rewritten only
// when Core's digest differed from this Edge's, so a whole rewrite costs less
// than working out which rows moved.
func replaceSupplyRefusals(tx *sql.Tx, rows []SupplyRefusal) error {
	if _, err := tx.Exec(`DELETE FROM supply_refusals_open`); err != nil {
		return fmt.Errorf("reconcile supply refusals: clear: %w", err)
	}
	for _, r := range rows {
		var ackAt any
		if r.AckAt != nil {
			ackAt = r.AckAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.Exec(
			`INSERT INTO supply_refusals_open (`+supplyRefusalCols+`) VALUES (?,?,?,?,?,?,?)`,
			r.LoaderNode, r.PayloadCode, r.RefusedAt.UTC().Format(time.RFC3339Nano), r.RefusedBy,
			ackAt, r.AckChoice, r.AckProcessID); err != nil {
			return fmt.Errorf("reconcile supply refusals: insert %s/%s: %w", r.LoaderNode, r.PayloadCode, err)
		}
	}
	return nil
}
