// ledger_integrity.go — read-side forensics on the bins UOP ledger.
//
// THE STRUCTURAL CAUSE (verified)
//
// The two halves of the in-loop total have asymmetric protection:
//
//	lineside_buckets.qty  INTEGER NOT NULL CHECK (qty >= 0)   ← DB refuses negative
//	bins.uop_remaining    INTEGER NOT NULL DEFAULT 0           ← no constraint
//
// and the apply path is a raw accumulate (uop/applier.go):
//
//	UPDATE bins SET uop_remaining = uop_remaining + $1 WHERE id=$2
//
// No clamp, no guard. A negative total means the BIN side is wrong — the count
// drifted from the shelf, which happens for ordinary physical reasons: a press
// overpacked, a fork truck delivered parts outside ShinGo, someone moved a bin
// by hand.
//
// IT DOES NOT STOP REPLENISHMENT, AND MUST NOT. checkBindings used to refuse to
// signal on a negative total, and that was the first link in the 2026-07-21
// chain: ledger negative, replenishment silent, payload genuinely dry, the
// changeover then arming onto a dry source. Springfield logged that refusal
// 1,119 times a day. The reading is too LOW, so the honest response to it is to
// order material — over-ordering is recoverable, starving a line because a
// count was wrong is not. The suppression is gone; the flag is not.
//
// So what this file is FOR is narrower and more useful than it first appeared:
// finding which counts drifted, when, how far, and behind what — so a person
// can go and recount them.
//
// DO NOT CLAMP uop_remaining AT ZERO. Going negative is what makes the drift
// LOUDLY wrong; clamping makes it SILENTLY wrong, and destroys the only
// evidence of what happened. Storing the honest number and acting sensibly on
// it are two different decisions — keep the first, and the second is
// "keep ordering, and tell someone".
//
// Everything here is computable TODAY, retroactively, from bin_uop_audit —
// which records before_uop and after_uop on every delta (~4k rows/day, 234k
// on disk). No new table, no migration, no new write path.

package bins

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"shingocore/domain"
)

// The row types live in shingocore/domain so www handlers can name them
// without importing this persistence package (the depguard rule).
type (
	NegativeExcursion = domain.NegativeExcursion
	OpenNegativeBin   = domain.OpenNegativeBin
	RecordAccuracy    = domain.RecordAccuracy
	CarrierBinding    = domain.CarrierBinding
)

// CarrierBindings returns every carrier with the binding ShinGo currently holds
// about it: the payload, when that binding started, and what the ledger reads.
//
// ── WHY THIS IS NOT A FILTERED QUERY ─────────────────────────────────────────
//
// It returns ALL carriers and selects none. The selection rule for 5.11 — which
// bindings are stale-binding candidates — lives in one tested pure function in
// www, for two reasons. A WHERE clause cannot be unit-tested at its boundary,
// and more importantly the rule has to be able to include the rows it CANNOT
// judge: a carrier whose bind predates the audit trail has an unknowable binding
// age, and a SQL predicate on that column would silently drop it. An absence
// filtered out by the query is an absence the page can never say it had.
//
// Fleet sizes make this affordable: Springfield runs 29 carriers, and the page
// reads them once per load.
//
// ── THE BINDING BOUNDARY, AND WHY IT IS NOT bins.loaded_at ───────────────────
//
// bound_at comes from the newest audit.EpochBumpOps row, not from
// bins.loaded_at. MEASURED, Springfield 2026-07-27: bin 12 carried
// loaded_at = 07-21 14:46 against a set_for_production at 07-23 15:07, and bin
// 29 read loaded_at = 07-16 against a boundary at 07-23. loaded_at tracks the
// physical load and is not written by every path that starts a new count, so it
// runs EARLIER than the binding and would report ages that are too long — the
// direction that manufactures candidates.
//
// A carrier with no boundary row at all returns NULL rather than being hidden:
// same rule as OpenNegativeBins and NegativeSince.
//
// ── WHY boundaryOps IS A PARAMETER AND NOT audit.EpochBumpOps INLINE ─────────
//
// It is exactly that slice, and the caller is store.DB.CarrierBindings, one
// level up. The op set cannot be referenced from here: depguard's
// store-sub-pkg-isolation rule forbids a store sub-package from importing a
// sibling aggregate, because cross-aggregate orchestration belongs at the outer
// store/ level, and store/audit is a sibling.
//
// SO THE ONE THING NOT TO DO IS TYPE THE OPS OUT HERE. A hardcoded op list
// beside a computed one is the exact drift that flat-lined the velocity chart
// (Q-036: live unloads write released_capture_empty and released_underpack,
// which a hand-written three-op filter missed). The parameter keeps one source
// of truth on the other side of the boundary the linter is enforcing.
func CarrierBindings(db *sql.DB, boundaryOps []string) ([]CarrierBinding, error) {
	if len(boundaryOps) == 0 {
		// An empty set would make bound_at NULL for every carrier — which the view
		// renders as "age unknown" and lists as a candidate, so EVERY carrier would
		// be flagged. Refuse rather than produce that: a page where every row is
		// flagged has flagged nothing, and it would look like a finding.
		return nil, fmt.Errorf("carrier bindings: no boundary ops supplied — every " +
			"binding age would read as unknown and every carrier would be a candidate")
	}
	// pq.Array is unavailable here (this package takes a bare *sql.DB and the
	// driver is wired above it), so the op set is expanded as a positional IN list.
	ph := make([]string, len(boundaryOps))
	args := make([]any, len(boundaryOps))
	for i, op := range boundaryOps {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = op
	}

	q := `
SELECT b.id, COALESCE(b.label,''), COALESCE(b.payload_code,''), COALESCE(n.name,''),
       b.uop_remaining,
       p.uop_capacity,
       (SELECT MAX(a.applied_at) FROM bin_uop_audit a
         WHERE a.bin_id = b.id AND a.op IN (` + strings.Join(ph, ",") + `)) AS bound_at,
       b.last_counted_at,
       b.anomaly_at
FROM bins b
LEFT JOIN nodes n ON n.id = b.node_id
LEFT JOIN payloads p ON b.payload_code <> '' AND p.code = b.payload_code
ORDER BY b.id`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("carrier bindings: %w", err)
	}
	defer rows.Close()

	var out []CarrierBinding
	for rows.Next() {
		var c CarrierBinding
		// capacity is NULL for an unbound carrier and for a payload row that does
		// not exist; ZERO for a payload that exists with no capacity set. Both
		// mean "cannot size a negative", and both are carried as nil rather than
		// as 0 — a 0 here would divide, and the quotient would render.
		var capacity sql.NullInt64
		var boundAt, counted, anomaly sql.NullTime
		if err := rows.Scan(&c.BinID, &c.Label, &c.PayloadCode, &c.NodeName,
			&c.UOPRemaining, &capacity, &boundAt, &counted, &anomaly); err != nil {
			return nil, fmt.Errorf("scan carrier binding: %w", err)
		}
		if capacity.Valid && capacity.Int64 > 0 {
			v := int(capacity.Int64)
			c.UOPCapacity = &v
		}
		if boundAt.Valid {
			t := boundAt.Time
			c.BoundAt = &t
		}
		if counted.Valid {
			t := counted.Time
			c.LastCountedAt = &t
		}
		if anomaly.Valid {
			t := anomaly.Time
			c.AnomalyAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NegativeExcursions finds every crossing of zero since `since`, newest first.
//
// A crossing is a bin_uop_audit row where before_uop >= 0 AND after_uop < 0.
// Continuations (already negative, going further negative) are not crossings —
// they are the same excursion, and Deepest folds them in.
//
// PrecededByRelease tests the standing hypothesis, which is that the drops and
// the negatives are two faces of one race:
//
//	service/bin_manifest.go zeroes uop_remaining AND bumps delta_epoch on
//	release. In-flight consume deltas then arrive stale and are DROPPED —
//	that is the ~1,779/day. But a delta arriving with the NEW epoch against a
//	just-zeroed bin debits from zero and goes negative.
//
// Dropped deltas alone cannot explain negatives: a dropped NEGATIVE delta
// leaves the count too HIGH, not too low. So if the crossings cluster behind
// recent releases, the hypothesis holds. If they do not, the crossings still
// say what the real cause is — which is why this reports either way rather
// than filtering for the expected answer.
//
// releaseWindow is how recently a release must precede the crossing to count.
func NegativeExcursions(db *sql.DB, since time.Time, releaseWindow time.Duration, limit int) ([]NegativeExcursion, error) {
	if limit <= 0 {
		limit = 200
	}
	const q = `
WITH crossings AS (
    SELECT a.id, a.bin_id, a.applied_at, a.before_uop, a.after_uop,
           a.op, a.source, a.actor, COALESCE(a.metadata::text,'') AS metadata,
           a.payload_code
    FROM bin_uop_audit a
    WHERE a.applied_at >= $1
      AND a.before_uop IS NOT NULL
      AND a.before_uop >= 0
      AND a.after_uop < 0
)
SELECT c.bin_id,
       COALESCE(NULLIF(c.payload_code,''), b.payload_code, '') AS payload_code,
       COALESCE(b.label,''), COALESCE(n.name,''),
       c.applied_at, c.before_uop, c.after_uop,
       -- Deepest: the floor reached before the bin next read >= 0.
       COALESCE((SELECT MIN(d.after_uop) FROM bin_uop_audit d
                 WHERE d.bin_id = c.bin_id AND d.applied_at >= c.applied_at
                   AND d.applied_at < COALESCE((SELECT MIN(r.applied_at) FROM bin_uop_audit r
                                                WHERE r.bin_id = c.bin_id
                                                  AND r.applied_at > c.applied_at
                                                  AND r.after_uop >= 0), 'infinity')), c.after_uop) AS deepest,
       (SELECT MIN(r.applied_at) FROM bin_uop_audit r
        WHERE r.bin_id = c.bin_id AND r.applied_at > c.applied_at AND r.after_uop >= 0) AS recovered_at,
       c.op, c.source, c.actor, c.metadata,
       EXISTS (SELECT 1 FROM bin_uop_audit p
               WHERE p.bin_id = c.bin_id
                 AND p.applied_at < c.applied_at
                 AND p.applied_at >= c.applied_at - $2::interval
                 AND p.after_uop = 0
                 AND p.before_uop > 0) AS preceded_by_release
FROM crossings c
LEFT JOIN bins b ON b.id = c.bin_id
LEFT JOIN nodes n ON n.id = b.node_id
ORDER BY c.applied_at DESC
LIMIT $3`

	rows, err := db.Query(q, since.UTC(), fmt.Sprintf("%d seconds", int(releaseWindow.Seconds())), limit)
	if err != nil {
		return nil, fmt.Errorf("negative excursions: %w", err)
	}
	defer rows.Close()

	var out []NegativeExcursion
	for rows.Next() {
		var e NegativeExcursion
		var recovered sql.NullTime
		if err := rows.Scan(&e.BinID, &e.PayloadCode, &e.Label, &e.NodeName,
			&e.CrossedAt, &e.BeforeUOP, &e.AfterUOP, &e.Deepest, &recovered,
			&e.Op, &e.Source, &e.Actor, &e.Metadata, &e.PrecededByRelease); err != nil {
			return nil, fmt.Errorf("scan negative excursion: %w", err)
		}
		if recovered.Valid {
			t := recovered.Time
			e.RecoveredAt = &t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// OpenNegativeBins lists bins currently below zero.
//
// This is the read that turns "the ledger is broken somewhere" into "SMN_02
// reads -443 for 74577-6SA0A.06 and has done since 09:14" — the difference
// between a log line nobody joins and a supervisor-actionable sentence.
//
// Reads bins directly (the live truth) and dates the crossing from the
// PERMANENT exceptions ledger (bin_uop_exception, v93) rather than the raw
// audit stream — one of the two reads the retention DELETE would otherwise
// destroy (the 90-day window is shorter than some real excursions have lasted).
// A bin whose crossing predates the backfill shows a nil NegativeSince rather
// than being hidden.
func OpenNegativeBins(db *sql.DB) ([]OpenNegativeBin, error) {
	const q = `
SELECT b.id, COALESCE(b.label,''), COALESCE(b.payload_code,''), COALESCE(n.name,''),
       b.uop_remaining,
       (SELECT MAX(e.occurred_at) FROM bin_uop_exception e
        WHERE e.bin_id = b.id AND e.kind = 'negative_crossing') AS negative_since,
       b.last_counted_at
FROM bins b
LEFT JOIN nodes n ON n.id = b.node_id
WHERE b.uop_remaining < 0
ORDER BY b.uop_remaining ASC`

	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("open negative bins: %w", err)
	}
	defer rows.Close()

	var out []OpenNegativeBin
	for rows.Next() {
		var b OpenNegativeBin
		var since, counted sql.NullTime
		if err := rows.Scan(&b.BinID, &b.Label, &b.PayloadCode, &b.NodeName,
			&b.UOPRemaining, &since, &counted); err != nil {
			return nil, fmt.Errorf("scan open negative bin: %w", err)
		}
		if since.Valid {
			t := since.Time
			b.NegativeSince = &t
		}
		if counted.Valid {
			t := counted.Time
			b.LastCountedAt = &t
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// NegativePayloads returns the payload codes whose plant-wide in-loop bin
// total is currently negative — the payloads the threshold monitor is
// deciding on from a reading it cannot trust.
//
// It is NOT a list of suppressed replenishment. Suppression was removed: a
// negative total means material moved off the books, which is when the loop
// most needs restocking. What the monitor loses is a usable denominator, not
// the decision — "we ordered for this payload against a count we know is
// wrong" is the sentence this makes available. Same condition checkBindings
// logs 1,119 times a day, expressed once, as state.
func NegativePayloads(db *sql.DB) (map[string]int, error) {
	rows, err := db.Query(`
		SELECT payload_code, SUM(uop_remaining)::INTEGER
		FROM bins
		WHERE payload_code <> ''
		GROUP BY payload_code
		HAVING SUM(uop_remaining) < 0`)
	if err != nil {
		return nil, fmt.Errorf("negative payloads: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var code string
		var total int
		if err := rows.Scan(&code, &total); err != nil {
			return nil, fmt.Errorf("scan negative payload: %w", err)
		}
		out[code] = total
	}
	return out, rows.Err()
}

// GetRecordAccuracy computes inventory-record accuracy over the window.
//
// Corrections are read from bin_uop_audit rather than a bespoke table: a
// recount lands as a delta like any other, and its op/source identify it.
func GetRecordAccuracy(db *sql.DB, since time.Time, staleAfter time.Duration) (*RecordAccuracy, error) {
	var r RecordAccuracy
	err := db.QueryRow(`
		SELECT
		  COUNT(*) FILTER (WHERE last_counted_at IS NOT NULL),
		  COUNT(*) FILTER (WHERE last_counted_at IS NULL),
		  COUNT(*) FILTER (WHERE last_counted_at IS NOT NULL AND last_counted_at < $1),
		  COALESCE(EXTRACT(DAY FROM (NOW() - MIN(last_counted_at)))::INTEGER, 0)
		FROM bins`,
		time.Now().UTC().Add(-staleAfter)).
		Scan(&r.BinsWithCount, &r.BinsNeverCounted, &r.StaleBins, &r.OldestCountDays)
	if err != nil {
		return nil, fmt.Errorf("record accuracy (bins): %w", err)
	}

	err = db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(AVG(ABS(after_uop - before_uop)), 0)::float8,
		       COALESCE(MAX(ABS(after_uop - before_uop)), 0)
		FROM bin_uop_audit
		WHERE applied_at >= $1
		  AND before_uop IS NOT NULL
		  AND (op LIKE '%correction%' OR op LIKE '%count%' OR op LIKE '%recount%')`,
		since.UTC()).
		Scan(&r.Corrections, &r.MeanAbsCorrection, &r.LargestCorrection)
	if err != nil {
		return nil, fmt.Errorf("record accuracy (corrections): %w", err)
	}
	return &r, nil
}
