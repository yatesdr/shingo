// Package reconciliation holds cross-aggregate anomaly detection for
// shingo-core (stuck orders, expired staged bins, stale edges, orphaned
// bin claims, manifests without active orders).
//
// Phase 5 of the architecture plan moved the reconciliation query
// fan-out out of the flat store/ package and into this sub-package.
// The outer store/ keeps type aliases and one-line delegate methods on
// *store.DB so external callers see no API change.
//
// Reconciliation is grouped as its own aggregate (rather than colocated
// with orders, bins, or edges) because the whole point of the module is
// cross-aggregate drift detection — putting it under any one aggregate
// would misrepresent the dependency direction.
package reconciliation

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store/internal/helpers"
	"shingocore/store/messaging"
	"shingocore/store/reservations"
)

const criticalOutboxAge = 5 * time.Minute
const stuckOrderAge = 30 * time.Minute

// queuedOrderAge is the SEPARATE, longer staleness bound for an order that is
// still acquiring (queued or sourcing). Waiting is what these statuses are FOR,
// so they need a different threshold from the ones where the fleet already has
// the order and nothing is moving.
//
// 30 minutes is the right question to ask of a `dispatched` leg — a robot that
// has not moved in half an hour has been forgotten. It is the wrong question to
// ask of an order waiting on material: a loop can legitimately sit unfillable
// across a shift change or a supplier gap, and flagging every one of those at
// 30 minutes trains people to ignore the anomaly board, which costs more than
// the silence did.
//
// Two hours is the operator judgement (2026-08-03): long enough that ordinary
// material churn clears on its own, short enough to catch a wedge inside one
// shift. Measured against the live Springfield board the day it was set, this
// separates a genuinely stuck window (4h41m) from routine contention (48m).
const queuedOrderAge = 2 * time.Hour

// CompletionAnomalyWindow is how far back a completion anomaly still counts
// AS A VERDICT.
//
// The list is deliberately still all-time — a completed order with a NULL
// bin_id is a real inconsistency whenever it happened, and hiding the old ones
// would make them unfindable. What changes is what "Core degraded" MEANS.
//
// Measured at Springfield 2026-07-29: ten anomalies, every one an order
// created and completed between 2026-03-30 and 2026-04-06, pinning the strip
// red for four months while database, fleet and messaging were all up and the
// pool was idle. An unbounded count turns a health verdict into a permanent
// latch on the oldest mistake anyone ever made — the strip says "something is
// wrong now" and means "something was wrong once".
const CompletionAnomalyWindow = 24 * time.Hour

// CompletionAnomaly describes drift between terminal orders and bin
// claim state.
type CompletionAnomaly struct {
	OrderID     int64  `json:"order_id"`
	BinID       *int64 `json:"bin_id,omitempty"`
	OrderStatus string `json:"order_status"`
	BinStatus   string `json:"bin_status,omitempty"`
	Issue       string `json:"issue"`
	// ObservedAt is when the order reached the state being complained about:
	// completed_at where there is one, updated_at for the confirmed-but-never-
	// completed case, which by definition has no completed_at. Carried on the
	// row rather than derived in the caller so the anomaly LIST can print an
	// age — the thing that would have made the Springfield ten obvious on
	// sight instead of costing a psql session.
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// Anomaly describes one reconciliation finding.
type Anomaly struct {
	Category          string     `json:"category"`
	Severity          string     `json:"severity"`
	Issue             string     `json:"issue"`
	RecommendedAction string     `json:"recommended_action,omitempty"`
	OrderID           *int64     `json:"order_id,omitempty"`
	BinID             *int64     `json:"bin_id,omitempty"`
	StationID         string     `json:"station_id,omitempty"`
	OrderStatus       string     `json:"order_status,omitempty"`
	BinStatus         string     `json:"bin_status,omitempty"`
	Detail            string     `json:"detail,omitempty"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
}

// Summary aggregates reconciliation counts for the health endpoint.
type Summary struct {
	// CompletionAnomalies counts only those inside CompletionAnomalyWindow.
	// This is the one the verdict reads.
	CompletionAnomalies int `json:"completion_anomalies"`
	// CompletionAnomaliesTotal is every one on record. Reported so a windowed
	// zero cannot be mistaken for "there are none" — the old rows are still
	// real, still worth fixing, and still one query away.
	CompletionAnomaliesTotal int `json:"completion_anomalies_total"`
	// CompletionAnomalyWindowHours is CompletionAnomalyWindow, reported rather
	// than restated: the strip labels its tile from this, so the label cannot
	// drift from the constant the verdict actually uses.
	CompletionAnomalyWindowHours int        `json:"completion_anomaly_window_hours"`
	StuckOrders                  int        `json:"stuck_orders"`
	ExpiredStagedBins            int        `json:"expired_staged_bins"`
	StaleEdges                   int        `json:"stale_edges"`
	TotalAnomalies               int        `json:"total_anomalies"`
	OutboxPending                int        `json:"outbox_pending"`
	OldestOutboxAt               *time.Time `json:"oldest_outbox_at,omitempty"`
	DeadLetters                  int        `json:"dead_letters"`
	Status                       string     `json:"status"`
}

// ListOrderCompletionAnomalies surfaces high-risk drift between
// terminal orders and bin claim state.
func ListOrderCompletionAnomalies(db *sql.DB) ([]*CompletionAnomaly, error) {
	rows, err := db.Query(fmt.Sprintf(`
		SELECT o.id AS order_id, b.id AS bin_id, o.status AS order_status, b.status AS bin_status, 'terminal_order_still_claims_bin' AS issue, COALESCE(o.completed_at, o.updated_at) AS observed_at
		FROM orders o
		JOIN bins b ON b.claimed_by = o.id
		WHERE o.completed_at IS NOT NULL OR o.status IN (%s)
		UNION ALL
		SELECT o.id AS order_id, NULL::bigint AS bin_id, o.status AS order_status, '' AS bin_status, 'completed_order_missing_bin' AS issue, COALESCE(o.completed_at, o.updated_at) AS observed_at
		FROM orders o
		WHERE o.completed_at IS NOT NULL AND o.bin_id IS NULL
		  -- A COMPOUND PARENT CARRIES NO BIN, so asking it for one is asking the
		  -- wrong row. Its legs hold the bins: a service dig's parent is a
		  -- container with no cargo and no robot, and a plain buried retrieve
		  -- re-parents the demand so its own fetch becomes a leg.
		  --
		  -- The anomaly was written for a different shape — a SINGLE-BIN order
		  -- whose UpdateOrderBinID never persisted, which reaches FINISHED with
		  -- its bin still sitting at source (see wiring_completion.go's diagnostic
		  -- for the same failure caught one layer up). Every compound parent
		  -- matched it too, and matched it forever, because the condition is
		  -- permanent for that shape.
		  --
		  -- Measured on the lane-stress rig 2026-08-13: TWELVE anomalies, ten
		  -- service digs and two buried retrieves, every one of them a compound
		  -- parent whose legs had delivered correctly. Zero completed orders with
		  -- no bin AND no legs — the predicate had no true positives at all, and
		  -- the strip read "Core degraded" for the whole run because of it.
		  --
		  -- THE EXEMPTION IS THE CHILD ROWS, not the order type or a flag. Whether
		  -- an order owns legs is the fact that decides whose bin it is, it is
		  -- true of both compound shapes, and it cannot drift from a label.
		  --
		  -- THE PREDICATE IS NOW SHARED. This clause was spelled inline here and
		  -- nowhere else, while seven other sites asked the same question as
		  -- order.BinID == nil -- which is true of a coordinator AND true of a
		  -- defect. This site was right and alone. helpers.OwnsNoCargoSQL is that
		  -- spelling, lifted so the other seven can reach it.
		  AND NOT `+helpers.OwnsNoCargoSQL("o")+`
		UNION ALL
		SELECT o.id AS order_id, o.bin_id AS bin_id, o.status AS order_status, COALESCE(b.status, '') AS bin_status, 'confirmed_without_completed_at' AS issue, COALESCE(o.completed_at, o.updated_at) AS observed_at
		FROM orders o
		LEFT JOIN bins b ON b.id = o.bin_id
		WHERE o.status = 'confirmed' AND o.completed_at IS NULL
		ORDER BY order_id, issue`, protocol.FailureTerminalStatusSQLList()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var anomalies []*CompletionAnomaly
	for rows.Next() {
		var a CompletionAnomaly
		var binID *int64
		var observedAt *time.Time
		if err := rows.Scan(&a.OrderID, &binID, &a.OrderStatus, &a.BinStatus, &a.Issue, &observedAt); err != nil {
			return nil, err
		}
		a.BinID = binID
		a.ObservedAt = observedAt
		anomalies = append(anomalies, &a)
	}
	return anomalies, rows.Err()
}

// countRecent returns how many of the anomalies fall inside the window.
//
// A row with no timestamp counts as recent. The alternative — silently
// dropping it — makes a NULL look like health, which is the failure family
// this whole panel exists to catch.
func countRecent(anomalies []*CompletionAnomaly, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	n := 0
	for _, a := range anomalies {
		if a.ObservedAt == nil || a.ObservedAt.After(cutoff) {
			n++
		}
	}
	return n
}

// ListAnomalies returns all reconciliation findings across categories.
func ListAnomalies(db *sql.DB) ([]*Anomaly, error) {
	completion, err := ListOrderCompletionAnomalies(db)
	if err != nil {
		return nil, err
	}
	return listAnomaliesWith(db, completion)
}

// listAnomaliesWith is ListAnomalies over completion rows the caller already
// holds. GetSummary needs those rows themselves — countRecent reads their
// timestamps, which the mapped Anomaly does not carry — so without this seam it
// ran the completion query, then called ListAnomalies, which ran it again. Two
// round trips per health hit, and nothing tied the two results together.
func listAnomaliesWith(db *sql.DB, completion []*CompletionAnomaly) ([]*Anomaly, error) {
	var anomalies []*Anomaly
	for _, a := range completion {
		issue := a.Issue
		action := ""
		switch issue {
		case "confirmed_without_completed_at":
			action = "reapply_completion"
		case "terminal_order_still_claims_bin":
			action = "release_terminal_claim"
		}
		orderID := a.OrderID
		anomalies = append(anomalies, &Anomaly{
			Category:          "order_completion",
			Severity:          "critical",
			Issue:             issue,
			RecommendedAction: action,
			OrderID:           &orderID,
			BinID:             a.BinID,
			OrderStatus:       a.OrderStatus,
			BinStatus:         a.BinStatus,
		})
	}

	// Two thresholds, picked per row by status — see queuedOrderAge.
	//
	// QUEUED ONLY, not both acquiring statuses. `sourcing` is meant to be
	// transient (MoveToSourcing sits at the start of the reserve attempt, so few
	// orders rest there), which makes half an hour in `sourcing` a real signal
	// worth keeping. Widening the longer bound to cover it would silence an alarm
	// that currently works, to fix a problem it does not have.
	//
	// Splitting it in SQL rather than running two queries keeps the ORDER BY over
	// the whole result, so the oldest anomaly is still first regardless of which
	// bound produced it.
	//
	// THE ::int CASTS ARE LOAD-BEARING. The driver sends these parameters
	// untyped, so without them Postgres infers the CASE result as `text` and the
	// whole query dies at RUNTIME on `operator does not exist: text * interval`
	// — a live 500 on the health endpoint, from a query that builds and vets
	// clean. It also survives a psql `PREPARE stuckq(int, int, text)` check,
	// because declaring the types is exactly what the driver does not do.
	// TestListAnomalies_QueuedGetsTheLongerBound exercises this through the
	// driver, which is the only check that would have caught it.
	//
	// ── AND `NOW()` WAS THE WRONG CLOCK (§R.98 stage D) ───────────────────
	//
	// `orders.updated_at` is stamped with `clock.Now()` by every one of its ~20
	// writers (orders/orders.go says so in as many words). Comparing it against
	// the DATABASE's wall NOW() is the exact mistake AutoConfirmStuckDeliveredOrders
	// documents and avoids, on the same column, in the same subsystem, two files
	// away: "a wall-NOW() comparison never fires once the sim clock outruns wall
	// time (10× → immediately)".
	//
	// It matters more here than anywhere else in the census, because this query
	// IS `ListAnomalies`' runtime-stuck detector — the ONE instrument in the system
	// that flags a wedged `in_transit` order. Under the rig's clamp the two clocks
	// agreed and it fired. Remove the clamp, which is the next repair in this same
	// stage, and every `updated_at` sits in the future and this goes permanently
	// silent. The one thing that would have said "order 2 has not advanced in
	// sixteen minutes" was one config flag from saying nothing at all.
	// ── AND updated_at MEANS TOUCHED, NOT PROGRESSED ─────────────────────
	//
	// It is stamped by ~20 writers, and several of them run on the dispatch retry
	// loop: an order that re-enters the scanner every tick, is refused, and parks
	// again has its updated_at moved forward every time. So the one order this
	// detector exists to catch — the one going nowhere fastest — refreshes its own
	// staleness timer, and the alarm never fires.
	//
	// MEASURED, on the sim wedge of 2026-08-28 (main 1a6b6d23): order 143 sat in
	// `sourcing` behind a leaked lane hold for over two hours of sim time. Its last
	// real transition was at 19:00:55; its updated_at read 19:16:55 and kept
	// climbing. Sixteen minutes of "activity" in which nothing happened, against a
	// thirty-minute bound it would never reach.
	//
	// So the clock is the last STATUS TRANSITION — an order_history row, which is
	// written only when an order actually moves (a re-entry that changes nothing
	// appends nothing, and setQueueReason short-circuits an unchanged reason). Its
	// created_at is stamped with clock.Now() by the same writers, so this does not
	// re-introduce the wall-clock mismatch stage D removed just above.
	//
	// COALESCE to the order's own created_at: an order with no history yet has not
	// progressed since it was born, which is the honest reading and keeps a row
	// that never transitioned from being invisible forever.
	//
	// The POPULATION is untouched — same statuses, same two bounds, same casts.
	// Only the clock changes.
	now := clock.Now().UTC()
	rows, err := db.Query(fmt.Sprintf(`
		SELECT o.id, o.status, COALESCE(h.last_progress, o.created_at) AS progressed_at
		FROM orders o
		LEFT JOIN LATERAL (
		        SELECT MAX(created_at) AS last_progress
		        FROM order_history WHERE order_id = o.id
		) h ON TRUE
		WHERE o.status IN (%s)
		  AND COALESCE(h.last_progress, o.created_at) < $4::timestamptz - (
		        CASE WHEN o.status = $3 THEN $2::int ELSE $1::int END * INTERVAL '1 second')
		ORDER BY progressed_at ASC`, protocol.RuntimeStuckCandidateStatusSQLList()),
		int(stuckOrderAge.Seconds()), int(queuedOrderAge.Seconds()), string(protocol.StatusQueued), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var orderID int64
		var status string
		var progressedAt time.Time
		if err := rows.Scan(&orderID, &status, &progressedAt); err != nil {
			return nil, err
		}
		// ── IT RECOMMENDED THE ONE ACT THIS HOUSE RULED IS NEVER RIGHT ────
		//
		// The value here was `cancel_stuck_order`, and the board turned that into
		// its only affordance for this row: a single button reading "Cancel Stuck
		// Order", beside an Issue cell reading `active_order_stuck` and nothing
		// else. Cancelling a stuck order is ruled 4/4 never the answer — the
		// order is stuck because something in the plant is stuck, and cancelling
		// it destroys the evidence while leaving the robot exactly where it was.
		//
		// So the recommendation names the act that IS right: go and look. The
		// operator can still cancel — the repair endpoint still accepts
		// `cancel_stuck_order` and RecordRecoveryAction still writes it, because
		// that verb records something a human genuinely did — but the board no
		// longer proposes it.
		//
		// AND THE ROW NOW SAYS WHAT IS WRONG. Detail was populated here and
		// rendered by neither the template nor the JS, so the operator got an
		// enum and a button. A row that names no robot, node or bin and offers
		// one destructive act is not a diagnosis.
		anomalies = append(anomalies, &Anomaly{
			Category:          "order_runtime",
			Severity:          "degraded",
			Issue:             "active_order_stuck",
			RecommendedAction: "investigate_stuck_order",
			OrderID:           &orderID,
			OrderStatus:       status,
			ObservedAt:        &progressedAt,
			Detail: "the order has not CHANGED STATUS within the allowed age threshold — the time " +
				"shown is its last real transition, not the last time a writer touched the row. " +
				"Find what its robot is doing before anything else — cancelling clears the row and " +
				"leaves the plant as it was",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Same column, same rule: `staged_expires_at` is written from a Go value on
	// the injected clock (bins.Stage, and the placement primitive since §R.98
	// stage D), and the sweep that ACTS on it — bins.ReleaseExpiredStaged —
	// compares it against clock.Now(). One column, two readers, and they used to
	// be on two clocks: the sweep and this page could give opposite answers about
	// the same bin.
	rows, err = db.Query(`
		SELECT id, status, staged_expires_at
		FROM bins
		WHERE status='staged'
		  AND staged_expires_at IS NOT NULL
		  AND staged_expires_at < $1::timestamptz
		ORDER BY staged_expires_at ASC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var binID int64
		var status string
		var observedAt time.Time
		if err := rows.Scan(&binID, &status, &observedAt); err != nil {
			return nil, err
		}
		anomalies = append(anomalies, &Anomaly{
			Category:          "bin_staging",
			Severity:          "degraded",
			Issue:             "staged_bin_expired",
			RecommendedAction: "release_staged_bin",
			BinID:             &binID,
			BinStatus:         status,
			ObservedAt:        &observedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	staleEdgeAnomalies, err := listStaleEdgeAnomalies(db)
	if err != nil {
		return nil, err
	}
	anomalies = append(anomalies, staleEdgeAnomalies...)

	stackedBinAnomalies, err := listStackedBinAnomalies(db)
	if err != nil {
		return nil, err
	}
	anomalies = append(anomalies, stackedBinAnomalies...)

	orphanManifestAnomalies, err := listOrphanManifestAnomalies(db)
	if err != nil {
		return nil, err
	}
	anomalies = append(anomalies, orphanManifestAnomalies...)

	return anomalies, nil
}

// listStaleEdgeAnomalies reports Edge stations the registry has marked stale.
func listStaleEdgeAnomalies(db *sql.DB) ([]*Anomaly, error) {
	var anomalies []*Anomaly
	rows, err := db.Query(`
		SELECT station_id, last_heartbeat
		FROM edge_registry
		WHERE status='stale'
		ORDER BY station_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var stationID string
		var observedAt *time.Time
		if err := rows.Scan(&stationID, &observedAt); err != nil {
			return nil, err
		}
		anomalies = append(anomalies, &Anomaly{
			Category:          "edge_connectivity",
			Severity:          "degraded",
			Issue:             "edge_marked_stale",
			RecommendedAction: "request_reregistration",
			StationID:         stationID,
			ObservedAt:        observedAt,
		})
	}
	// The other four detectors in this function check rows.Err(); this one did
	// not, so a mid-iteration driver error silently truncated the list instead
	// of reporting it -- on the detector that says Edge stations have gone dark.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return anomalies, nil
}

// Detect bins stacked at a concrete node that is not a storage
// container — i.e., more than one bin physically present at a process
// node (line node, dropoff target, etc.). This indicates a prior
// cycle's evac order failed to complete the bin handoff (e.g., Robot B
// faulted en route from core to AMR group, operator took manual
// control, transaction never finalized) while subsequent cycles
// continued to deliver new bins to the same node.
// See bug-fix-review-plan.md item 3.1.
//
// Excluded by type — containers whose bin_count legitimately rolls up
// across child slots, so >1 bin is normal, not stacked:
//
//	NGRP — synthetic parent for lanes / direct nodes
//	LANE — depth-ordered slot group (children are the actual slots)
//	STOR — a single physical supermarket position that holds many bins
//	       (dispatch/store_slot.go treats STOR as one addressable slot
//	       with multi-bin capacity — it is NOT an aggregate of children)
//
// The join is LEFT so an untyped node still gets checked (node_type_id
// NULL); only the three named codes are excluded. TRANSIT is not listed:
// no node_type with that code exists — the in-flight model is the
// synthetic _TRANSIT node, already excluded by is_synthetic.
//
// Retired bins are excluded (status <> 'retired'), matching
// CountByNode/ListByNode in store/bins: a retired bin parked on a node
// is inventory bookkeeping, not a physical stack, and counting it
// fires a false critical on the first live delivery to that node.
//
// Scope: root-level nodes only (parent_id IS NULL) — roughly two-thirds
// of slots. Group-parented slots are not watched by this check; a clean
// board is not proof that no group-parented slot is stacked.
func listStackedBinAnomalies(db *sql.DB) ([]*Anomaly, error) {
	var anomalies []*Anomaly
	rows, err := db.Query(`
		SELECT n.id, n.name, COUNT(b.id) AS bin_count
		FROM bins b
		JOIN nodes n ON n.id = b.node_id
		LEFT JOIN node_types nt ON nt.id = n.node_type_id
		WHERE n.is_synthetic = false
		  AND b.status <> 'retired'
		  AND COALESCE(nt.code, '') NOT IN ('NGRP', 'LANE', 'STOR')
		  AND n.parent_id IS NULL
		GROUP BY n.id, n.name
		HAVING COUNT(b.id) > 1
		ORDER BY n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID int64
		var nodeName string
		var binCount int
		if err := rows.Scan(&nodeID, &nodeName, &binCount); err != nil {
			return nil, err
		}
		anomalies = append(anomalies, &Anomaly{
			Category:          "node_inventory",
			Severity:          "critical",
			Issue:             "multi_bin_at_non_storage_node",
			RecommendedAction: "clear_stacked_bins",
			Detail: fmt.Sprintf("node %s has %d bins stacked — likely prior evac handoff failed; clear via admin bin-move",
				nodeName, binCount),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return anomalies, nil
}

// Detect bins with speculative manifest but no active claiming order.
// This is informational only — manifest represents physical reality and
// should NOT be cleared. The detection surfaces these bins for review.
func listOrphanManifestAnomalies(db *sql.DB) ([]*Anomaly, error) {
	var anomalies []*Anomaly
	rows, err := db.Query(fmt.Sprintf(`
		SELECT b.id, b.label, b.status, b.claimed_by,
		       COALESCE(o.status, 'no_order') AS order_status
		FROM bins b
		LEFT JOIN orders o ON o.id = b.claimed_by
		WHERE b.manifest IS NOT NULL
		  AND (b.claimed_by IS NULL
		       OR o.status IN (%s))
		ORDER BY b.id`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var binID int64
		var label, binStatus string
		var claimedBy *int64
		var orderStatus string
		if err := rows.Scan(&binID, &label, &binStatus, &claimedBy, &orderStatus); err != nil {
			return nil, err
		}
		anomalies = append(anomalies, &Anomaly{
			Category:          "bin_manifest",
			Severity:          "info",
			Issue:             "manifest_without_active_order",
			RecommendedAction: "review_manifest",
			BinID:             &binID,
			BinStatus:         binStatus,
			OrderID:           claimedBy,
			OrderStatus:       orderStatus,
			Detail:            fmt.Sprintf("bin %s has manifest but no active claiming order (claimed_by=%v, order_status=%s)", label, claimedBy, orderStatus),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return anomalies, nil
}

// GetSummary rolls ListAnomalies plus outbox counts into a single
// status payload for the health endpoint.
func GetSummary(db *sql.DB) (*Summary, error) {
	completion, err := ListOrderCompletionAnomalies(db)
	if err != nil {
		return nil, err
	}
	anomalies, err := listAnomaliesWith(db, completion)
	if err != nil {
		return nil, err
	}

	summary := &Summary{
		CompletionAnomalies:          countRecent(completion, time.Now().UTC(), CompletionAnomalyWindow),
		CompletionAnomaliesTotal:     len(completion),
		CompletionAnomalyWindowHours: int(CompletionAnomalyWindow / time.Hour),
		TotalAnomalies:               len(anomalies),
	}

	row := db.QueryRow(`SELECT COUNT(*), MIN(created_at) FROM outbox WHERE sent_at IS NULL AND retries < $1`, messaging.MaxOutboxRetries)
	if err := row.Scan(&summary.OutboxPending, &summary.OldestOutboxAt); err != nil {
		return nil, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE sent_at IS NULL AND retries >= $1`, messaging.MaxOutboxRetries).Scan(&summary.DeadLetters); err != nil {
		return nil, err
	}

	for _, a := range anomalies {
		switch a.Issue {
		case "active_order_stuck":
			summary.StuckOrders++
		case "staged_bin_expired":
			summary.ExpiredStagedBins++
		case "edge_marked_stale":
			summary.StaleEdges++
		}
	}
	summary.Status = "ok"
	if summary.OutboxPending > 0 || summary.StuckOrders > 0 || summary.ExpiredStagedBins > 0 || summary.StaleEdges > 0 {
		summary.Status = "degraded"
	}
	if summary.CompletionAnomalies > 0 || summary.DeadLetters > 0 {
		summary.Status = "critical"
	} else if summary.OldestOutboxAt != nil && time.Since(summary.OldestOutboxAt.UTC()) >= criticalOutboxAge {
		summary.Status = "critical"
	}

	return summary, nil
}

// ReleaseOrphanedClaims finds bins still claimed by terminal orders and
// releases them. Defense-in-depth sweep that catches any claims that
// leaked past the atomic status transitions (e.g. due to a process
// crash mid-transaction). Returns the number of claims released.
func ReleaseOrphanedClaims(db *sql.DB) (int, error) {
	result, err := db.Exec(fmt.Sprintf(`
		UPDATE bins
		SET claimed_by = NULL, updated_at = NOW()
		WHERE claimed_by IS NOT NULL
		  AND claimed_by IN (
		    SELECT id FROM orders
		    WHERE status IN (%s)
		  )`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()

	// Store dual: release destination-slot claims (nodes.claimed_by) held by
	// terminal orders. Catches any slot claim that leaked past the atomic
	// terminal transitions (FailOrderAtomic/SkipOrderAtomic/CancelOrderAtomic),
	// same as the bin sweep above.
	slotRes, err := db.Exec(fmt.Sprintf(`
		UPDATE nodes
		SET claimed_by = NULL, updated_at = NOW()
		WHERE claimed_by IS NOT NULL
		  AND claimed_by IN (
		    SELECT id FROM orders
		    WHERE status IN (%s)
		  )`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return int(n), err
	}
	sn, _ := slotRes.RowsAffected()
	return int(n + sn), nil
}

// acquiringStatusSQL is the SQL literal list for the two statuses this sweep
// covers: an order that is still ACQUIRING its material.
//
// Not derived from a protocol helper because there is no predicate for "still
// acquiring" — `queued` and `sourcing` are named here on purpose, as the two
// states where holding a claim without a reservation is drift rather than a
// stage of some longer transaction. A dispatched order mid-handoff is a
// different question and is deliberately outside this sweep.
const acquiringStatusSQL = `'queued','sourcing'`

// ReleaseAcquiringOrphanClaims is the claim-side backstop's SECOND arm: it
// clears a claim held by a LIVE acquiring order that has no reservation behind
// it, and says so loudly.
//
// ── THE DRIFT, AND WHY NOTHING ELSE SWEEPS IT ─────────────────────────────
//
// Ownership of a bin is written in two books — bins.claimed_by and the
// reservations row — coupled by convention rather than by schema. A release that
// drops one and not the other leaves a bin claimed by an order that holds
// nothing on it. Every availability predicate is owner-blind, so from that
// moment the bin is invisible to EVERYBODY, including the order whose name is on
// the claim: its own demand waits forever on material it already owns, and the
// existing sweep cannot help because that order is not terminal. Observed on the
// sim 2026-08-28 (order 109 / bin 10).
//
// ── IT IS INSURANCE, AND IT IS DELIBERATELY LOUD ──────────────────────────
//
// Owner ruling 2026-08-28: take it as a minutes-scale self-heal while the
// ownership conversion lands, knowing Stage 3 obsoletes it. So every clear is
// logged with the bin, the order, and the sentence saying this is the two books
// having come apart — NOT normal operation. A sweep that fires often is telling
// you the conversion is overdue, and that signal only exists if it shouts. Same
// rule the floor's own recovery records obey: somebody is going to count these.
//
// ── THE PRECISION RULES, EACH OF WHICH IS A REFUSAL ───────────────────────
//
//	ANY LIVE RESERVATION ON THE RESOURCE and the claim is never touched. That is
//	what every healthy order in the plant looks like, and taking one would strip
//	a bin off a robot. The predicate is resource-keyed, not owner-keyed, on
//	purpose: a reservation held by somebody ELSE means the two books disagree
//	about WHO, which is a different defect with a different fix, and guessing
//	here would resolve it by deletion.
//
//	orders.bin_id IS NEVER TOUCHED. A third book, already ruled: different fix.
//
//	TERMINAL ORDERS stay with ReleaseOrphanedClaims. Two sweeps, two
//	populations, two log lines — a merged count could not tell a leak past a
//	terminal transition from ownership drift on a live order, and they send the
//	reader to different files.
//
// Rides the existing reconciliation interval; no cadence of its own.
func ReleaseAcquiringOrphanClaims(db *sql.DB) (int, error) {
	// THE OWNER IS READ IN A CTE, NOT IN RETURNING. An UPDATE's RETURNING sees
	// the NEW row, where claimed_by has just been set to NULL — so the log line
	// could name the bin and never the order that was holding it, which is half
	// of what makes this line chaseable. The victim set is selected first and
	// joined back to what the UPDATE actually changed, so the two cannot disagree
	// about which rows were swept.
	binRows, err := db.Query(fmt.Sprintf(`
		WITH victims AS (
		  SELECT b.id, b.claimed_by AS owner FROM bins b
		  WHERE b.claimed_by IS NOT NULL
		    AND EXISTS (
		      SELECT 1 FROM orders o
		      WHERE o.id = b.claimed_by AND o.status IN (%s)
		    )
		    AND NOT `+reservations.OnTheBooksSQL(reservations.KindBin, "b.id")+`
		), swept AS (
		  UPDATE bins SET claimed_by = NULL, updated_at = NOW()
		  WHERE id IN (SELECT id FROM victims)
		  RETURNING id
		)
		SELECT v.id, v.owner FROM victims v JOIN swept s ON s.id = v.id`,
		acquiringStatusSQL))
	if err != nil {
		return 0, fmt.Errorf("reconciliation acquiring-orphan-bin-claims: %w", err)
	}
	n := 0
	for binRows.Next() {
		var binID, orderID int64
		if err := binRows.Scan(&binID, &orderID); err != nil {
			binRows.Close()
			return n, fmt.Errorf("reconciliation acquiring-orphan-bin-claims scan: %w", err)
		}
		n++
		log.Printf("RECONCILIATION: cleared an ORPHANED CLAIM — bin %d was claimed by order %d, which "+
			"is still acquiring and holds no reservation on it. Ownership is written in two books and "+
			"they have come apart: the claim made the bin invisible to every demand INCLUDING its own "+
			"owner's. This is drift being swept, not normal operation — if it recurs, the ownership "+
			"conversion is overdue", binID, orderID)
	}
	if err := binRows.Err(); err != nil {
		binRows.Close()
		return n, fmt.Errorf("reconciliation acquiring-orphan-bin-claims rows: %w", err)
	}
	binRows.Close()

	// The node dual — a destination slot held the same way, by the same orders,
	// with the same consequence: the slot is unavailable to everybody, so no
	// demand can resolve onto it and the holder cannot progress toward it.
	slotRows, err := db.Query(fmt.Sprintf(`
		WITH victims AS (
		  SELECT nd.id, nd.claimed_by AS owner FROM nodes nd
		  WHERE nd.claimed_by IS NOT NULL
		    AND EXISTS (
		      SELECT 1 FROM orders o
		      WHERE o.id = nd.claimed_by AND o.status IN (%s)
		    )
		    AND NOT `+reservations.OnTheBooksSQL(reservations.KindSlot, "nd.id")+`
		), swept AS (
		  UPDATE nodes SET claimed_by = NULL, updated_at = NOW()
		  WHERE id IN (SELECT id FROM victims)
		  RETURNING id
		)
		SELECT v.id, v.owner FROM victims v JOIN swept s ON s.id = v.id`,
		acquiringStatusSQL))
	if err != nil {
		return n, fmt.Errorf("reconciliation acquiring-orphan-slot-claims: %w", err)
	}
	defer slotRows.Close()
	for slotRows.Next() {
		var nodeID, orderID int64
		if err := slotRows.Scan(&nodeID, &orderID); err != nil {
			return n, fmt.Errorf("reconciliation acquiring-orphan-slot-claims scan: %w", err)
		}
		n++
		log.Printf("RECONCILIATION: cleared an ORPHANED CLAIM — slot node %d was claimed by order %d, "+
			"which is still acquiring and holds no slot reservation on it. Same two-books drift as the "+
			"bin arm, on the destination side: the slot was unavailable to every demand including the "+
			"holder's own. Not normal operation", nodeID, orderID)
	}
	return n, slotRows.Err()
}
