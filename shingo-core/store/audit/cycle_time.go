package audit

import (
	"database/sql"
	"fmt"
	"time"

	"shingocore/domain"
)

// cycle_time.go — the read side of cycle time (Stage 5.10).
//
// One query. Everything it returns is differenced, bucketed and classified in
// domain/cycle_time.go, which is where the reasoning is and where the tests can
// reach it without Postgres.
//
// ── WHY THE DIFFERENCING IS NOT IN SQL ───────────────────────────────────────
//
// A LAG(applied_at) OVER (PARTITION BY …) window would compute the gaps here,
// correctly, and would be untestable without a database. The rules that decide
// what a cycle IS — which reasons count, that a blank payload is reported rather
// than bucketed, that a gap never crosses a key boundary, that a negative gap is
// clock skew and not a measurement — are the ones most likely to be got wrong,
// so they live where a unit test can mutate them and watch the number move.
//
// ── AND WHY THERE IS NO DEDUP PASS ───────────────────────────────────────────
//
// Stage 5.10 as written asks for one: "~1,779/day stale-epoch drops + ~1,779/day
// replays produce phantom gaps". Do not add it. The two counts are the SAME
// POPULATION LOGGED TWICE — uop.applier's stale-epoch branch logs the drop and
// then returns ErrInventoryDeltaSkipped, whose caller logs "replay — already
// applied" — and a genuine replay returns before the INSERT, so it writes no row
// here at all. Every row this query can see was applied exactly once. A dedup
// pass over them would discard real events and manufacture the very gaps it was
// meant to remove.

// OpBinUOPDelta is the op tag on an APPLIED BinUOPDelta — the truth path, the
// row written in the same transaction as the bins.uop_remaining update.
//
// Declared here rather than beside the other Op* constants because the applier
// writes it as a LITERAL inside its INSERT and does not reference a constant at
// all. Restating it is therefore a second spelling of a string that already
// exists, and the only honest way to hold two spellings together is to check
// them: TestCycleOpMatchesTheApplier reads uop/applier.go and fails if this
// value is not the one it writes.
const OpBinUOPDelta = "bin_uop_delta"

// ListCycleEvents returns the truth-path delta rows a cycle distribution is
// built from, oldest first, plus whether the row cap bit.
//
// ── THE STATION COMES OUT OF actor, AND THAT IS NOT A TYPO ───────────────────
//
// bin_uop_audit has both a station column and a node_id column, and the applied
// delta INSERT populates NEITHER: it names
// (bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata) and
// passes the Edge station as actor. So on this op the station column is empty
// and node_id is NULL for every row ever written, historical and future.
// Reading the station column here would return an empty string for all of them
// and the page would render one nameless key.
//
// The consequence for 5.10 is worth stating plainly: the style guide assigns
// this surface a distribution per (NODE, payload), and node is not recoverable
// from the truth path. Joining bins to get one would be worse than not having it
// — a bin's node is where it is NOW, not where it was when the tick landed.
//
// ── THE CAP NARROWS THE WINDOW, IT DOES NOT PUNCH HOLES ──────────────────────
//
// Ordering DESC and capping takes the most recent contiguous run of rows, so
// every gap computed inside it is a real gap between two adjacent events. A cap
// applied any other way (a per-key limit, an offset, a sample) would leave gaps
// that skip over unread rows and read as long cycles that never happened. The
// bool is returned so the page can say the window was narrowed, because a
// distribution over a silently shortened window is a distribution that lies
// about its own n.
func ListCycleEvents(db *sql.DB, since time.Time, limit int) ([]domain.CycleEvent, bool, error) {
	if limit <= 0 {
		limit = 20000
	}
	rows, err := db.Query(`SELECT actor, payload_code, metadata->>'reason', applied_at
		FROM bin_uop_audit
		WHERE op = $1
		  AND applied_at >= $2
		  AND metadata->>'reason' IN ($3, $4)
		ORDER BY applied_at DESC
		LIMIT $5`,
		OpBinUOPDelta, since,
		domain.CycleDirectionProduce, domain.CycleDirectionConsume,
		limit)
	if err != nil {
		return nil, false, fmt.Errorf("list cycle events since %s: %w", since.Format(time.RFC3339), err)
	}
	defer rows.Close()

	out := make([]domain.CycleEvent, 0, 512)
	for rows.Next() {
		var actor, payload sql.NullString
		var reason sql.NullString
		var at time.Time
		if err := rows.Scan(&actor, &payload, &reason, &at); err != nil {
			return nil, false, fmt.Errorf("scan cycle event: %w", err)
		}
		out = append(out, domain.CycleEvent{
			CycleKey: domain.CycleKey{
				Station:   actor.String,
				Payload:   payload.String,
				Direction: reason.String,
			},
			At: at,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read cycle events: %w", err)
	}

	truncated := len(out) >= limit

	// Reverse into chronological order. The query is DESC so the cap keeps the
	// NEWEST rows; the differencing wants oldest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, truncated, nil
}
