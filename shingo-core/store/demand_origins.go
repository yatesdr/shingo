package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/domain"
)

// demand_origins.go — Core's history of every demand episode.
//
// A DEMAND EPISODE is a continuous period during which a specific place needed
// material. Edge keeps only what is open, in demand_origins_open, and deletes
// the row on close; this table keeps every episode, open and closed. That is
// the whole difference between the two names.
//
// STATE TRANSFER, NOT EVENTS. Edge sends the WHOLE episode row on every change,
// stamped with a monotonic revision, and Core upserts it under a revision
// guard. Rebuilding episode state here by replaying opened/closed events would
// be a second copy maintained by replay — the uopCache mistake in a new place,
// where a private incremental tally drifted from the database and left
// Springfield reading 139 against a truth of 31.
//
// What the guard buys STRUCTURALLY rather than by handling: a duplicate
// delivery is a no-op at equal revision, an out-of-order pair resolves by
// comparison instead of a parking queue, a lost message self-heals on the next
// change, and the LAST message is sufficient on its own — lose everything
// except the close and this table still converges.

// DemandOrigin is one episode as Core stores it.
//
// THE TYPE LIVES IN domain/, not here, and this is an alias. www handlers must
// not import shingocore/store (a depguard rule with no remaining exemptions),
// so any row shape a handler needs to name has to be declared where a handler
// can reach it. Internal store callers compile unchanged through the alias —
// the pattern Stage 2A.2 established when it drained the original 17-file
// ratchet, and the same arrangement sourceability_event.go uses.
type DemandOrigin = domain.DemandOrigin

// DemandEpisode is an episode plus its child-order count. Also domain-owned;
// see the note above.
type DemandEpisode = domain.DemandEpisode

// Close reasons Core assigns on its own. Edge's live in protocol.
const (
	// CloseReasonSuperseded is domain-owned for the same reason as the type.
	// The reasoning behind the value lives with it.
	CloseReasonSuperseded = domain.CloseReasonSuperseded
)

// UpsertDemandOrigin applies one state message under the revision guard.
//
// THE GUARD IS PER origin_id, and that is exactly why SupersedeOpenEpisode
// exists alongside it — see the handler. Revisions are comparable only within
// one episode; two different episodes for the same place have no ordering
// relationship at all.
//
// ON UPDATE IT TOUCHES ONLY WHAT EDGE AUTHORS. signal_count and uop_delivered
// are accumulated on Core from its own signals and its own audit trail, and
// used_edge_reports records which total decided a Core-side threshold. Listing
// them in the SET clause would zero Core's own facts on every Edge message —
// silently, and only for episodes that get more than one.
func (db *DB) UpsertDemandOrigin(o DemandOrigin) error {
	var expected any
	if o.ExpectedOrders != nil {
		expected = *o.ExpectedOrders
	}
	_, err := db.Exec(`
		INSERT INTO demand_origins (
		    origin_id, revision, episode_key, kind, direction, trigger_kind,
		    trigger_ref, station_id, process_id, core_node_name, payload_code,
		    opened_at, opened_total, threshold, expected_orders,
		    expected_reason, rerequest_count, discretionary, closed_at, close_reason,
		    closed_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (origin_id) DO UPDATE SET
		    revision         = EXCLUDED.revision,
		    episode_key      = EXCLUDED.episode_key,
		    kind             = EXCLUDED.kind,
		    direction        = EXCLUDED.direction,
		    trigger_kind     = EXCLUDED.trigger_kind,
		    trigger_ref      = EXCLUDED.trigger_ref,
		    station_id       = EXCLUDED.station_id,
		    process_id       = EXCLUDED.process_id,
		    core_node_name   = EXCLUDED.core_node_name,
		    payload_code     = EXCLUDED.payload_code,
		    opened_at        = EXCLUDED.opened_at,
		    opened_total     = EXCLUDED.opened_total,
		    threshold        = EXCLUDED.threshold,
		    expected_orders  = EXCLUDED.expected_orders,
		    expected_reason  = EXCLUDED.expected_reason,
		    rerequest_count  = EXCLUDED.rerequest_count,
		    discretionary    = EXCLUDED.discretionary,
		    closed_at        = EXCLUDED.closed_at,
		    close_reason     = EXCLUDED.close_reason,
		    closed_by        = EXCLUDED.closed_by
		WHERE demand_origins.revision < EXCLUDED.revision`,
		o.OriginID, o.Revision, o.EpisodeKey, o.Kind, o.Direction, o.Trigger,
		o.TriggerRef, o.StationID, o.ProcessID, o.CoreNodeName, o.PayloadCode,
		o.OpenedAt, o.OpenedTotal, o.Threshold, expected,
		o.ExpectedUnknownReason, o.RerequestCount, o.Discretionary,
		o.ClosedAt, o.CloseReason, nullIfEmpty(o.ClosedBy))
	if err != nil {
		return fmt.Errorf("upsert demand origin %s: %w", o.OriginID, err)
	}
	return nil
}

// SupersedeOpenEpisode closes any OTHER episode still holding this place open.
//
// WHY THIS IS NEEDED AT ALL. The revision guard orders messages within one
// origin_id; the partial unique index enforces one OPEN episode per
// episode_key. Those are two different identities, and the gap between them is
// reachable: the outbox drainer does not stop at a failed message, so a close
// for episode A can fail to publish while a subsequent open for episode B on
// the same key succeeds. B then arrives at a key A still holds, and without
// this the insert violates the index and B is lost — the newer, truer episode
// discarded to protect a stale one.
//
// WHY IT IS SOUND. Edge cannot have minted B while A was open there:
// demand_origins_open has episode_key as its PRIMARY KEY, so the invariant is
// structural, not a convention someone might have broken. B's existence is
// therefore proof that A ended.
//
// IT DOES NOT BUMP THE REVISION, and that is the point. A's real close is still
// out there at a higher revision; leaving A's revision alone means that close
// still wins when it lands and replaces this placeholder with the true reason.
// Bumping would make Core's own guess outrank the truth.
func (db *DB) SupersedeOpenEpisode(episodeKey, newOriginID string, at time.Time) (int64, error) {
	res, err := db.Exec(`
		UPDATE demand_origins
		   SET closed_at = $1, close_reason = $2
		 WHERE episode_key = $3
		   AND origin_id <> $4
		   AND closed_at IS NULL`,
		at, CloseReasonSuperseded, episodeKey, newOriginID)
	if err != nil {
		return 0, fmt.Errorf("supersede open episode %q: %w", episodeKey, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// OpenThresholdEpisode mints a Core-owned threshold episode at revision 1.
//
// A plain INSERT, deliberately, and the partial unique index is the point: if
// something already holds this place open the write FAILS rather than quietly
// creating a second demand for one need. "One open episode per place" is the
// invariant the whole surface rests on, and a mint race is worth surfacing
// rather than resolving into whichever wrote last.
//
// Core mints only this kind. Cell and changeover episodes are authored on Edge
// and arrive through the state-transfer seam.
func (db *DB) OpenThresholdEpisode(o DemandOrigin, usedEdgeReports bool) error {
	var expected any
	if o.ExpectedOrders != nil {
		expected = *o.ExpectedOrders
	}
	_, err := db.Exec(`
		INSERT INTO demand_origins (
		    origin_id, revision, episode_key, kind, trigger_ref, station_id,
		    core_node_name, payload_code, opened_at, opened_total, threshold,
		    used_edge_reports, expected_orders, expected_reason, signal_count
		) VALUES ($1,1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,0)`,
		o.OriginID, o.EpisodeKey, o.Kind, o.TriggerRef, o.StationID,
		o.CoreNodeName, o.PayloadCode, o.OpenedAt, o.OpenedTotal, o.Threshold,
		usedEdgeReports, expected, o.ExpectedUnknownReason)
	if err != nil {
		return fmt.Errorf("open threshold episode %s (%s): %w", o.OriginID, o.EpisodeKey, err)
	}
	return nil
}

// CloseDemandOriginByID ends an episode Core owns, bumping the revision.
//
// The bump is not bookkeeping. Core's threshold episodes never cross the seam
// inbound, but the revision still has to move so that anything comparing
// versions of this row — the reconciler re-closing what a notification path
// already closed, a future read model — sees the close as newer. Closing an
// already-closed episode is a NO-OP rather than an error: the level edges are
// evaluated from several sites and the sweep runs underneath all of them, so
// two of them racing to close one episode is ordinary.
//
// ONLY FOR EPISODES CORE MINTS. Applying it to an Edge-authored episode would
// push demand_origins.revision past the number Edge is about to send, and the
// real close would then lose the upsert guard's comparison and be dropped —
// see CloseDemandOriginInferred, which exists for exactly that case.
//
// Returns whether a row actually moved, so a caller that reports what it closed
// reports work it did rather than work it attempted.
func (db *DB) CloseDemandOriginByID(originID, reason, closedBy string, at time.Time) (bool, error) {
	res, err := db.Exec(`
		UPDATE demand_origins
		   SET closed_at = $1, close_reason = $2, closed_by = $3
		 WHERE origin_id = $4 AND closed_at IS NULL`,
		at, reason, nullIfEmpty(closedBy), originID)
	if err != nil {
		return false, fmt.Errorf("close demand origin %s: %w", originID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CloseDemandOriginInferred ends an episode Core did NOT author, WITHOUT
// touching the revision.
//
// THE REVISION IS DELIBERATELY LEFT ALONE, and this is the same reasoning
// SupersedeOpenEpisode is built on. An Edge-authored episode's true close is
// still out there — in flight, or dead-lettered and awaiting a retry — carrying
// the revision Edge stamped on it. Bumping here would put Core's local revision
// at or above that number, and the upsert guard (`WHERE revision < EXCLUDED.
// revision`) would then silently DISCARD the real close when it lands. Core's
// inference would permanently outrank the truth, and the surface would show
// `unattributed` for an episode Edge knows ended `claim_removed`.
//
// So this close is a PLACEHOLDER by construction: it stops an ended demand
// rendering as a permanent alarm, and it steps aside the moment the owner says
// what actually happened.
//
// It is also the right shape for Core's own aging closes. Nothing else writes
// those rows, so preserving the revision costs nothing there, and one rule —
// "an inferred close never bumps" — is easier to keep true than a rule that
// depends on remembering which kind you are holding.
func (db *DB) CloseDemandOriginInferred(originID, reason, closedBy string, at time.Time) (bool, error) {
	res, err := db.Exec(`
		UPDATE demand_origins
		   SET closed_at = $1, close_reason = $2, closed_by = $3
		 WHERE origin_id = $4 AND closed_at IS NULL`,
		at, reason, nullIfEmpty(closedBy), originID)
	if err != nil {
		return false, fmt.Errorf("close demand origin (inferred) %s: %w", originID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// OpenEpisodeState is one open episode as the reconciling sweep sees it: the
// identity, plus the two facts the sweep decides on that are NOT on the row.
//
// Children and the last-contact timestamp are computed in the same query as the
// episode because fetching them one round trip at a time would be N+1 over a
// set whose whole cost argument is that it is small.
type OpenEpisodeState struct {
	OriginID   string
	EpisodeKey string
	Kind       string
	StationID  string
	OpenedAt   time.Time

	// Children is COUNT(orders WHERE origin_id = this). Zero is the input to
	// the childless auto-close.
	Children int

	// EdgeLastSeen is WHEN CORE LAST HEARD FROM THIS STATION —
	// edge_registry.last_heartbeat — and nil when Core cannot say at all:
	// either there is no registry row, or there is one that registered and then
	// never sent a heartbeat.
	//
	// IT IS DELIBERATELY THE TIMESTAMP AND NOT edge_registry.status, and that is
	// this struct's whole point. status is written 'active' by Register and by
	// every heartbeat, and exactly one thing ever moves it off 'active':
	// MarkStaleEdges, on a 60-second ticker inside CoreHandler — a different
	// service from the one running this sweep. Deciding reachability from
	// status therefore infers it from the ABSENCE OF A STALENESS FLAG, so a
	// stale-edge loop that is unstarted, misconfigured or itself broken leaves
	// every station in the plant reading 'active' forever and the sweep closes
	// EVERY OPEN EPISODE on the strength of a signal that was never computed.
	// The sharpest case needs nothing broken at all: a station that registers at
	// boot and never heartbeats has status 'active' and last_heartbeat NULL, and
	// Core has never heard one word from it.
	//
	// So the sweep asks the positive question instead — when did we last hear
	// from this Edge? — and the answer being absent is an UNKNOWN, not a "no".
	// An unknown decorates; it never closes. See engine.classifyEdgeContact,
	// which owns that decision, so a future reader cannot reintroduce the
	// inverted one by adding a column back to this query.
	EdgeLastSeen *time.Time
}

// ListOpenEpisodeStates returns every open episode with its child count and the
// time Core last heard from the Edge that owns it.
//
// COST IS BOUNDED BY OPEN-EPISODE COUNT, which is one per place currently short
// of material. That is what makes a per-episode subquery affordable here where
// it would be indefensible on a tick path — and idx_orders_origin_id is a
// partial index on exactly this lookup.
func (db *DB) ListOpenEpisodeStates() ([]OpenEpisodeState, error) {
	rows, err := db.Query(`
		SELECT o.origin_id, o.episode_key, o.kind, o.station_id, o.opened_at,
		       (SELECT COUNT(*) FROM orders c WHERE c.origin_id = o.origin_id),
		       e.last_heartbeat
		  FROM demand_origins o
		  LEFT JOIN edge_registry e ON e.station_id = o.station_id
		 WHERE o.closed_at IS NULL
		 ORDER BY o.opened_at`)
	if err != nil {
		return nil, fmt.Errorf("list open episode states: %w", err)
	}
	defer rows.Close()

	var out []OpenEpisodeState
	for rows.Next() {
		var s OpenEpisodeState
		if err := rows.Scan(&s.OriginID, &s.EpisodeKey, &s.Kind, &s.StationID,
			&s.OpenedAt, &s.Children, &s.EdgeLastSeen); err != nil {
			return nil, fmt.Errorf("scan open episode state: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AgeOutOrphanOrders retires orphan findings older than the cutoff.
//
// IT STAMPS A TIMESTAMP AND LEAVES origin_class ALONE. origin_class answers
// "how did this order relate to a demand" — a create-time fact, true forever —
// so a sweep that rewrote it would leave the row unable to say what it was
// classified as at creation, which is a fact overwritten by a derivation. The
// row still records that this order should have carried an origin; what changes
// is only whether it is still being ASKED ABOUT. See migration 61.
//
// `orphan_aged_at IS NULL` IS NOT DECORATION. Without it the sweep restamps the
// same rows on every pass, and "when did this stop being asked about" would
// permanently read "a minute ago" — the timestamp would be measuring the sweep
// rather than the order, and the trend line Phase 6 draws off it would be flat
// by construction.
//
// Returns the number retired, because a sweep that acts silently is
// unauditable by construction — the same reason closed_by exists.
func (db *DB) AgeOutOrphanOrders(createdBefore, agedAt time.Time) (int64, error) {
	res, err := db.Exec(`
		UPDATE orders
		   SET orphan_aged_at = $1
		 WHERE origin_class = $2
		   AND orphan_aged_at IS NULL
		   AND created_at < $3`,
		agedAt, protocol.OriginClassOrphan, createdBefore)
	if err != nil {
		return 0, fmt.Errorf("age out orphan orders: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// OrphanFinding is one order that should have carried an origin and didn't.
type OrphanFinding struct {
	EdgeUUID  string
	StationID string
	CreatedAt time.Time

	// AgedAt is when the sweep stopped asking about this one. NIL MEANS IT IS
	// STILL A LIVE FINDING — the whole aged/fresh split is this one predicate,
	// and it is derived here rather than stored as a class because the class
	// answers a different question that was already answered at create time.
	AgedAt *time.Time
}

// ListOrphanFindings returns the orphan bucket, live findings first.
//
// A NEW COLUMN IS NOT DONE UNTIL SOMETHING READS IT BACK AND ASSERTS ON THE
// VALUE. closed_by shipped on this same table with a migration, a struct field
// and every writer, and no reader — every read returned "" through a full green
// gate, because the write path and the read path are different code and only
// the one just written was tested. orphan_aged_at gets its reader in the commit
// that creates it.
//
// Unbounded deliberately. The orphan bucket is orders whose origin went
// missing, which should be near-empty; if it is large, that IS the finding, and
// a LIMIT here would hide it behind a page boundary.
func (db *DB) ListOrphanFindings() ([]OrphanFinding, error) {
	rows, err := db.Query(`
		SELECT edge_uuid, station_id, created_at, orphan_aged_at
		  FROM orders
		 WHERE origin_class = $1
		 ORDER BY orphan_aged_at NULLS FIRST, created_at`, protocol.OriginClassOrphan)
	if err != nil {
		return nil, fmt.Errorf("list orphan findings: %w", err)
	}
	defer rows.Close()

	var out []OrphanFinding
	for rows.Next() {
		var f OrphanFinding
		if err := rows.Scan(&f.EdgeUUID, &f.StationID, &f.CreatedAt, &f.AgedAt); err != nil {
			return nil, fmt.Errorf("scan orphan finding: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListOpenThresholdEpisodes returns every open Core-owned threshold episode.
//
// THIS IS WHAT startupSweep REHYDRATES FROM, and without it every Core restart
// mints a duplicate for every place that is currently below threshold while the
// original stays open forever. That is not an edge case: restarting Core is the
// remedy an operator reaches for BECAUSE the counts look wrong, which is
// exactly when open episodes exist.
func (db *DB) ListOpenThresholdEpisodes() ([]DemandOrigin, error) {
	rows, err := db.Query(`
		SELECT origin_id, revision, episode_key, station_id, core_node_name,
		       payload_code, opened_at, opened_total, threshold
		  FROM demand_origins
		 WHERE kind = $1 AND closed_at IS NULL`, "threshold")
	if err != nil {
		return nil, fmt.Errorf("list open threshold episodes: %w", err)
	}
	defer rows.Close()

	var out []DemandOrigin
	for rows.Next() {
		var o DemandOrigin
		if err := rows.Scan(&o.OriginID, &o.Revision, &o.EpisodeKey, &o.StationID,
			&o.CoreNodeName, &o.PayloadCode, &o.OpenedAt, &o.OpenedTotal, &o.Threshold); err != nil {
			return nil, fmt.Errorf("scan open threshold episode: %w", err)
		}
		o.Kind = "threshold"
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetDemandOrigin reads one episode back. Returns nil when absent.
//
// closed_by is read as a NullString because the column has no default and NULL
// is a real value with its own meaning — "the sender did not say", i.e. an
// older Edge or a row written before the column existed. It is a different fact
// from "a notification path closed it", and a scan that could not distinguish
// them would collapse the two, which is the entire reason the column has no
// default in the first place.
func (db *DB) GetDemandOrigin(originID string) (*DemandOrigin, error) {
	var (
		o        DemandOrigin
		expected *int
		closedAt *time.Time
		closedBy sql.NullString
	)
	err := db.QueryRow(`
		SELECT origin_id, revision, episode_key, kind, direction, trigger_kind,
		       trigger_ref, station_id, process_id, core_node_name, payload_code,
		       opened_at, opened_total, threshold, expected_orders,
		       expected_reason, rerequest_count, discretionary, closed_at,
		       close_reason, closed_by
		  FROM demand_origins WHERE origin_id = $1`, originID).Scan(
		&o.OriginID, &o.Revision, &o.EpisodeKey, &o.Kind, &o.Direction, &o.Trigger,
		&o.TriggerRef, &o.StationID, &o.ProcessID, &o.CoreNodeName, &o.PayloadCode,
		&o.OpenedAt, &o.OpenedTotal, &o.Threshold, &expected,
		&o.ExpectedUnknownReason, &o.RerequestCount, &o.Discretionary,
		&closedAt, &o.CloseReason, &closedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get demand origin %s: %w", originID, err)
	}
	o.ExpectedOrders = expected
	o.ClosedAt = closedAt
	o.ClosedBy = closedBy.String
	return &o, nil
}

// nullIfEmpty writes SQL NULL for an empty string.
//
// closed_by has no default and must stay NULL when nobody said — "the sender
// did not tell us" is a different fact from "a notification path closed it",
// and writing ” would collapse them into one indistinguishable value.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ListDemandEpisodes returns episodes newest-first for the demand browser
// (Stage 5.1), each joined to its child-order count.
//
// SCOPE IS "OPEN, PLUS ANYTHING THAT CLOSED INSIDE THE WINDOW", not "opened
// inside the window". An episode that has been open for six hours is the single
// most important row this page can show, and a window on opened_at would drop it
// off the page precisely as it got bad enough to matter — the surface would go
// quiet exactly when it should be loudest.
//
// LIMIT IS A HARD CAP, NOT A PAGE. Cardinality at a plant is unmeasured (the
// sweep's cost is bounded by open-episode count, which nobody has counted at
// Springfield yet), so an unbounded SELECT here is a query whose cost nobody
// knows. The caller is told when the cap bit — see the returned truncated flag —
// because a silently truncated list is a list that lies about what the floor is
// doing.
//
// The child count is a correlated subquery over idx_orders_origin_id, a partial
// index on exactly this lookup. Same construction ListOpenEpisodeStates uses and
// for the same reason: nothing computed is ever stored, so cost is counted at
// read time. A stored rollup starts drifting from what it summarises.
func (db *DB) ListDemandEpisodes(since time.Time, limit int) (episodes []DemandEpisode, truncated bool, err error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.Query(`
		SELECT o.origin_id, o.revision, o.episode_key, o.kind, o.direction,
		       o.trigger_kind, o.trigger_ref, o.station_id, o.process_id,
		       o.core_node_name, o.payload_code, o.opened_at, o.opened_total,
		       o.threshold, o.expected_orders, o.expected_reason,
		       o.rerequest_count, o.discretionary, o.closed_at, o.close_reason,
		       o.closed_by,
		       (SELECT COUNT(*) FROM orders c WHERE c.origin_id = o.origin_id)
		  FROM demand_origins o
		 WHERE o.closed_at IS NULL OR o.closed_at >= $1
		 ORDER BY o.opened_at DESC
		 LIMIT $2`, since, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list demand episodes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			e        DemandEpisode
			closedAt *time.Time
			closedBy sql.NullString
		)
		// expected_orders and closed_by are scanned through nullable carriers
		// deliberately. Both have a NULL that MEANS something different from any
		// value they could hold — "the denominator is unknowable" and "the sender
		// did not say" — and a scan into a plain int or string would destroy that
		// here, before any renderer could tell the two apart.
		if err := rows.Scan(&e.OriginID, &e.Revision, &e.EpisodeKey, &e.Kind,
			&e.Direction, &e.Trigger, &e.TriggerRef, &e.StationID, &e.ProcessID,
			&e.CoreNodeName, &e.PayloadCode, &e.OpenedAt, &e.OpenedTotal,
			&e.Threshold, &e.ExpectedOrders, &e.ExpectedUnknownReason,
			&e.RerequestCount, &e.Discretionary, &closedAt, &e.CloseReason,
			&closedBy, &e.Children); err != nil {
			return nil, false, fmt.Errorf("scan demand episode: %w", err)
		}
		e.ClosedAt = closedAt
		e.ClosedBy = closedBy.String
		episodes = append(episodes, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list demand episodes: %w", err)
	}

	// One row over the limit was requested so the cap can be REPORTED rather
	// than inferred. Asking for exactly `limit` leaves "got limit rows"
	// ambiguous between a full page and a truncated one.
	if len(episodes) > limit {
		return episodes[:limit], true, nil
	}
	return episodes, false, nil
}

// ListClosedBySince returns the closed_by value of every episode closed since
// `since`, with NULL preserved as the empty string.
//
// FEEDS 5.6, AND THE NULL IS THE POINT. The sweep's share of closes climbing
// toward 100% means the notification paths have silently stopped firing, and
// nothing else in the system would say so. Aggregating in SQL with a GROUP BY
// would be cheaper and would fold NULL into whatever the caller defaulted it to;
// returning the raw values keeps the third state intact all the way to the
// summariser, which counts it separately on purpose.
func (db *DB) ListClosedBySince(since time.Time) ([]string, error) {
	rows, err := db.Query(`
		SELECT closed_by FROM demand_origins
		 WHERE closed_at IS NOT NULL AND closed_at >= $1`, since)
	if err != nil {
		return nil, fmt.Errorf("list closed_by: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan closed_by: %w", err)
		}
		out = append(out, v.String)
	}
	return out, rows.Err()
}
