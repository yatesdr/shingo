package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
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

// OpenOrigin is one open demand episode.
type OpenOrigin struct {
	EpisodeKey     string
	OriginID       string
	RerequestCount int
	OpenedAt       time.Time
}

// ErrOriginNotOpen is returned when no episode is open for a key.
var ErrOriginNotOpen = errors.New("no open demand origin for that episode key")

// OpenDemandOrigin records a newly minted episode.
//
// It is a plain INSERT, not an upsert, and the caller must have established
// that nothing is open for this key. A conflict here means two mint sites raced
// for one place, which is a bug worth surfacing rather than silently resolving
// into whichever wrote last — "one open episode per place" is the invariant the
// whole surface rests on.
func (db *DB) OpenDemandOrigin(episodeKey, originID string, openedAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO demand_origins_open (episode_key, origin_id, rerequest_count, opened_at)
		 VALUES (?, ?, 0, ?)`,
		episodeKey, originID, openedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("open demand origin %q: %w", episodeKey, err)
	}
	return nil
}

// GetOpenDemandOrigin returns the episode open for a key, or ErrOriginNotOpen.
func (db *DB) GetOpenDemandOrigin(episodeKey string) (*OpenOrigin, error) {
	var (
		o      OpenOrigin
		opened string
	)
	err := db.QueryRow(
		`SELECT episode_key, origin_id, rerequest_count, opened_at
		   FROM demand_origins_open WHERE episode_key = ?`, episodeKey).
		Scan(&o.EpisodeKey, &o.OriginID, &o.RerequestCount, &opened)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOriginNotOpen
	}
	if err != nil {
		return nil, fmt.Errorf("get open demand origin %q: %w", episodeKey, err)
	}
	if t, perr := time.Parse(time.RFC3339Nano, opened); perr == nil {
		o.OpenedAt = t
	}
	return &o, nil
}

// JoinDemandOrigin records an operator push that joined an already-open
// episode, returning the new count.
//
// The push is NOT a new demand — it is the same one expressed impatiently, and
// "this demand was manually re-requested 6 times" is a better signal than six
// demands of one order each.
func (db *DB) JoinDemandOrigin(episodeKey string) (int, error) {
	res, err := db.Exec(
		`UPDATE demand_origins_open SET rerequest_count = rerequest_count + 1
		  WHERE episode_key = ?`, episodeKey)
	if err != nil {
		return 0, fmt.Errorf("join demand origin %q: %w", episodeKey, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrOriginNotOpen
	}
	var count int
	if err := db.QueryRow(
		`SELECT rerequest_count FROM demand_origins_open WHERE episode_key = ?`, episodeKey).
		Scan(&count); err != nil {
		return 0, fmt.Errorf("read rerequest count %q: %w", episodeKey, err)
	}
	return count, nil
}

// CloseDemandOrigin removes the open episode and reports what it was, so the
// caller can emit origin.closed with the right id and final re-request count.
//
// Returns ErrOriginNotOpen when nothing was open — which callers treat as
// "already closed", not an error: a close can legitimately be reached twice
// (a recovery tick and a state-change poke racing), and closing twice must be
// harmless.
func (db *DB) CloseDemandOrigin(episodeKey string) (*OpenOrigin, error) {
	open, err := db.GetOpenDemandOrigin(episodeKey)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`DELETE FROM demand_origins_open WHERE episode_key = ?`, episodeKey); err != nil {
		return nil, fmt.Errorf("close demand origin %q: %w", episodeKey, err)
	}
	return open, nil
}

// ListOpenDemandOrigins returns every open episode. Used to rehydrate the
// in-memory cache at startup — Edge's half of "a restart must not mint a
// duplicate episode".
func (db *DB) ListOpenDemandOrigins() ([]OpenOrigin, error) {
	rows, err := db.Query(
		`SELECT episode_key, origin_id, rerequest_count, opened_at FROM demand_origins_open`)
	if err != nil {
		return nil, fmt.Errorf("list open demand origins: %w", err)
	}
	defer rows.Close()

	var out []OpenOrigin
	for rows.Next() {
		var (
			o      OpenOrigin
			opened string
		)
		if err := rows.Scan(&o.EpisodeKey, &o.OriginID, &o.RerequestCount, &opened); err != nil {
			return nil, fmt.Errorf("scan open demand origin: %w", err)
		}
		if t, perr := time.Parse(time.RFC3339Nano, opened); perr == nil {
			o.OpenedAt = t
		}
		out = append(out, o)
	}
	return out, rows.Err()
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
