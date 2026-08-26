package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shingo/protocol"
)

// demand_origins.go — Edge's OPEN demand episodes.
//
// This table holds only what is open, keyed on episode_key, and a row is
// DELETED on close. The history lives on Core in demand_origins, which is the
// service that keeps history; mirroring it here would be a second copy of the
// same facts, and the uopCache lesson is that a second copy starts drifting
// from what it summarises.
//
// The hot path stays in memory. This is the WRITE-THROUGH target, touched only
// when an episode opens, is joined, or closes — because Edge restarts more
// often than anything else in the system, and an in-memory-only episode is lost
// by a `systemctl restart shingoedge`, after which the next tick mints a
// duplicate and the first never closes.

// OpenOrigin is one open demand episode, in full.
//
// It carries the WHOLE row rather than just the id because Core is sent STATE,
// not events: the close message has to include kind, direction,
// expected_orders and the rest, and at close time none of that is derivable
// from anywhere else — it was known at mint and sent, never kept.
type OpenOrigin struct {
	EpisodeKey string
	OriginID   string
	// Revision is monotonic per episode and is what Core's upsert compares.
	Revision int64

	Kind string
	// Direction holds the cell's ROLE — produce or consume — and is typed so it
	// cannot hold anything else. The column keeps the name `direction` for now;
	// the values changed under migration 87 and renaming the column on both
	// services is a cosmetic follow-on for the tree-cleanup branch, not a
	// behaviour change to smuggle into this one. Empty for the kinds that have
	// no cell behind them (threshold, changeover).
	Direction   protocol.ClaimRole
	TriggerKind string
	TriggerRef  string
	// ProcessID is the process NAME ("SNF2"), not this database's processes.id.
	// It is what the episode key carries and what Core's demand_origins.process_id
	// stores — the same value process_styles.process_id and
	// PlantClaimsReport.ProcessID already carry, which is what makes the demand
	// grain joinable with the plant-claims mirror at all. Resolved from the row id
	// once, at the mint boundary: Engine.processName.
	ProcessID    string
	CoreNodeName string
	PayloadCode  string

	OpenedTotal int
	Threshold   int
	// ExpectedOrders is nil when the denominator is UNKNOWABLE — a different
	// state from 1, and one the surface renders as "—" rather than as a ratio
	// somebody would draw a conclusion from.
	ExpectedOrders        *int
	ExpectedUnknownReason string
	RerequestCount        int
	Discretionary         bool
	OpenedAt              time.Time
}

// ErrOriginNotOpen is returned when no episode is open for a key.
var ErrOriginNotOpen = errors.New("no open demand origin for that episode key")

const openOriginCols = `episode_key, origin_id, revision, kind, direction, trigger_kind,
	trigger_ref, process_id, core_node_name, payload_code, opened_total, threshold,
	expected_orders, expected_unknown_reason, rerequest_count, discretionary, opened_at`

func scanOpenOrigin(sc interface{ Scan(...any) error }) (*OpenOrigin, error) {
	var (
		o        OpenOrigin
		expected sql.NullInt64
		opened   string
		disc     int
	)
	if err := sc.Scan(&o.EpisodeKey, &o.OriginID, &o.Revision, &o.Kind, &o.Direction,
		&o.TriggerKind, &o.TriggerRef, &o.ProcessID, &o.CoreNodeName, &o.PayloadCode,
		&o.OpenedTotal, &o.Threshold, &expected, &o.ExpectedUnknownReason,
		&o.RerequestCount, &disc, &opened); err != nil {
		return nil, err
	}
	if expected.Valid {
		v := int(expected.Int64)
		o.ExpectedOrders = &v
	}
	o.Discretionary = disc != 0
	if t, err := time.Parse(time.RFC3339Nano, opened); err == nil {
		o.OpenedAt = t
	}
	return &o, nil
}

// OpenDemandOrigin records a newly minted episode at revision 1.
//
// A plain INSERT, not an upsert: the caller must have established that nothing
// is open for this key. A conflict means two mint sites raced for one place,
// which is a bug worth surfacing rather than silently resolving into whichever
// wrote last — "one open episode per place" is the invariant the whole surface
// rests on.
func (db *DB) OpenDemandOrigin(o *OpenOrigin) error {
	var expected any
	if o.ExpectedOrders != nil {
		expected = *o.ExpectedOrders
	}
	disc := 0
	if o.Discretionary {
		disc = 1
	}
	_, err := db.Exec(
		`INSERT INTO demand_origins_open (`+openOriginCols+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.EpisodeKey, o.OriginID, 1, o.Kind, o.Direction, o.TriggerKind,
		o.TriggerRef, o.ProcessID, o.CoreNodeName, o.PayloadCode,
		o.OpenedTotal, o.Threshold, expected, o.ExpectedUnknownReason,
		o.RerequestCount, disc, o.OpenedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("open demand origin %q: %w", o.EpisodeKey, err)
	}
	o.Revision = 1
	return nil
}

// GetOpenDemandOrigin returns the episode open for a key, or ErrOriginNotOpen.
func (db *DB) GetOpenDemandOrigin(episodeKey string) (*OpenOrigin, error) {
	o, err := scanOpenOrigin(db.QueryRow(
		`SELECT `+openOriginCols+` FROM demand_origins_open WHERE episode_key = ?`, episodeKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOriginNotOpen
	}
	if err != nil {
		return nil, fmt.Errorf("get open demand origin %q: %w", episodeKey, err)
	}
	return o, nil
}

// JoinDemandOrigin records an operator push that joined an already-open
// episode, bumping the revision, and returns the updated row.
//
// The push is NOT a new demand — it is the same one expressed impatiently, and
// "this demand was manually re-requested 6 times" is a better signal than six
// demands of one order each. The revision bump is what makes the re-sent state
// win at Core.
func (db *DB) JoinDemandOrigin(episodeKey string) (*OpenOrigin, error) {
	res, err := db.Exec(
		`UPDATE demand_origins_open
		    SET rerequest_count = rerequest_count + 1, revision = revision + 1
		  WHERE episode_key = ?`, episodeKey)
	if err != nil {
		return nil, fmt.Errorf("join demand origin %q: %w", episodeKey, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrOriginNotOpen
	}
	return db.GetOpenDemandOrigin(episodeKey)
}

// CloseDemandOrigin bumps the revision, returns the full row so the caller can
// send the closing STATE, and deletes it.
//
// ENQUEUE FIRST, THEN DELETE is the caller's contract, not this function's —
// see the engine. Here the order is: read (so the caller has the content), then
// remove. Returns ErrOriginNotOpen when nothing was open, which callers treat
// as "already closed" rather than an error: a close can legitimately be reached
// twice, and closing twice must be harmless.
func (db *DB) CloseDemandOrigin(episodeKey string) (*OpenOrigin, error) {
	if _, err := db.Exec(
		`UPDATE demand_origins_open SET revision = revision + 1 WHERE episode_key = ?`,
		episodeKey); err != nil {
		return nil, fmt.Errorf("bump revision on close %q: %w", episodeKey, err)
	}
	open, err := db.GetOpenDemandOrigin(episodeKey)
	if err != nil {
		return nil, err
	}
	return open, nil
}

// DeleteDemandOrigin removes a closed episode. Called AFTER its closing state
// has been enqueued: the durable outbox owns delivery from that point, which is
// what lets this table stay "open episodes" rather than becoming "episodes we
// are still responsible for".
func (db *DB) DeleteDemandOrigin(episodeKey string) error {
	if _, err := db.Exec(`DELETE FROM demand_origins_open WHERE episode_key = ?`, episodeKey); err != nil {
		return fmt.Errorf("delete demand origin %q: %w", episodeKey, err)
	}
	return nil
}

// ListOpenDemandOrigins returns every open episode — the input to Edge's
// reconciling sweep, which closes any whose precondition no longer holds
// regardless of whether a notification path fired.
func (db *DB) ListOpenDemandOrigins() ([]OpenOrigin, error) {
	rows, err := db.Query(`SELECT ` + openOriginCols + ` FROM demand_origins_open`)
	if err != nil {
		return nil, fmt.Errorf("list open demand origins: %w", err)
	}
	defer rows.Close()

	var out []OpenOrigin
	for rows.Next() {
		o, err := scanOpenOrigin(rows)
		if err != nil {
			return nil, fmt.Errorf("scan open demand origin: %w", err)
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// CellLevelStillBreached reports whether a cell episode's precondition still
// holds: does ANY claim on this process still have its level breached, for this
// payload and this direction?
//
// THE FALLING EDGE IS THE PRECONDITION, IN ITS PERSISTED FORM. below_reorder_
// since is set on one crossing and cleared on the other, so asking "is it still
// set" asks the level question without recomputing a level — no runtime read,
// no bin arithmetic, nothing that can disagree with what the tick path decided.
//
// SCOPED TO THE ACTIVE STYLE, and that is load-bearing rather than tidiness. A
// claim on an inactive style is never evaluated, so its flag is frozen at
// whatever it held when the style was swapped out. Counting those would keep an
// episode open forever on the strength of a reading nobody is taking any more —
// and a changeover is exactly when that happens. Joining through
// processes.active_style_id means a style swap makes the precondition stop
// holding, which is correct: the process no longer needs that payload there.
//
// ANY claim, not the minting one: an A/B pair is two claims on one process and
// the process needs the payload while EITHER half is below. That is the same
// grain rule that put the episode on the process instead of the node.
// PROCESSNAME, NOT A ROW ID, and it joins on processes.name — which is UNIQUE in
// this schema, so it selects exactly the same single row p.id did. The episode
// row carries the name because that is what the whole demand grain is keyed on;
// re-deriving an id here just to join by it would put the translation back in a
// second place.
func (db *DB) CellLevelStillBreached(processName, payloadCode, role string) (bool, error) {
	var n int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		  FROM style_node_claims c
		  JOIN processes p ON p.active_style_id = c.style_id
		 WHERE p.name = ? AND c.payload_code = ? AND c.role = ?
		   AND c.below_reorder_since IS NOT NULL
		   AND c.below_reorder_since != ''`,
		processName, payloadCode, role).Scan(&n); err != nil {
		return false, fmt.Errorf("cell level still breached process=%q payload=%q role=%q: %w",
			processName, payloadCode, role, err)
	}
	return n > 0, nil
}

// CellPayloadStillClaimed reports whether the active style still claims this
// payload at this process in this direction, regardless of level.
//
// It is what separates the two ways a cell precondition stops holding. The
// level flag being clear says only that nothing is below; it does not say
// whether that is because material arrived or because the claim stopped
// existing. Those are a healthy ending and a silent disappearance, and a
// close_reason that merged them would make the second invisible — which is
// precisely the failure the reconciler is here to catch.
func (db *DB) CellPayloadStillClaimed(processName, payloadCode, role string) (bool, error) {
	var n int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		  FROM style_node_claims c
		  JOIN processes p ON p.active_style_id = c.style_id
		 WHERE p.name = ? AND c.payload_code = ? AND c.role = ?`,
		processName, payloadCode, role).Scan(&n); err != nil {
		return false, fmt.Errorf("cell payload still claimed process=%q payload=%q role=%q: %w",
			processName, payloadCode, role, err)
	}
	return n > 0, nil
}

// SetClaimBelowReorderSince stamps or clears a claim's falling edge.
//
// WRITE-THROUGH ON TRANSITION ONLY. The level is evaluated on every PLC consume
// tick; a write per tick would put a database round trip on the hot path for a
// value that changes twice per episode. Pass nil to clear.
func (db *DB) SetClaimBelowReorderSince(claimID int64, since *time.Time) error {
	var v any
	if since != nil {
		v = since.UTC().Format(time.RFC3339Nano)
	}
	if _, err := db.Exec(
		`UPDATE style_node_claims SET below_reorder_since = ? WHERE id = ?`, v, claimID); err != nil {
		return fmt.Errorf("set below_reorder_since claim=%d: %w", claimID, err)
	}
	return nil
}

// GetClaimBelowReorderSince reads a claim's falling edge, nil when not below.
func (db *DB) GetClaimBelowReorderSince(claimID int64) (*time.Time, error) {
	var since sql.NullString
	if err := db.QueryRow(
		`SELECT below_reorder_since FROM style_node_claims WHERE id = ?`, claimID).Scan(&since); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get below_reorder_since claim=%d: %w", claimID, err)
	}
	if !since.Valid || since.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, since.String)
	if err != nil {
		return nil, nil
	}
	return &t, nil
}
