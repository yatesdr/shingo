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
// No clamp, no guard. A negative total therefore means the BIN side is wrong,
// which is why ThresholdMonitor.checkBindings refuses to signal replenishment
// on one and says so loudly — 1,119 times a day at Springfield.
//
// That refusal is the first link in the 07-21 chain: ledger goes negative →
// replenishment suppressed → the payload genuinely runs dry → the changeover
// arms → supply parks on a dry source → evac dies → 484 doomed swaps. The
// futility detector catches the last step. This is upstream of all of it.
//
// DO NOT CLAMP uop_remaining AT ZERO. The current design — go negative, refuse
// to signal, log — is correct: it makes the ledger LOUDLY wrong. Clamping
// makes it SILENTLY wrong, which is strictly worse, and removes the only
// evidence that something debits without a matching credit. The bug is
// upstream; this file is for finding it.
//
// Everything here is computable TODAY, retroactively, from bin_uop_audit —
// which records before_uop and after_uop on every delta (~4k rows/day, 234k
// on disk). No new table, no migration, no new write path.

package bins

import (
	"database/sql"
	"fmt"
	"time"

	"shingocore/domain"
)

// The row types live in shingocore/domain so www handlers can name them
// without importing this persistence package (the depguard rule).
type (
	NegativeExcursion = domain.NegativeExcursion
	OpenNegativeBin   = domain.OpenNegativeBin
	RecordAccuracy    = domain.RecordAccuracy
)

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
// Reads bins directly (the live truth) and dates the crossing from the audit
// trail. A bin whose crossing predates the audit retention shows a nil
// NegativeSince rather than being hidden.
func OpenNegativeBins(db *sql.DB) ([]OpenNegativeBin, error) {
	const q = `
SELECT b.id, COALESCE(b.label,''), COALESCE(b.payload_code,''), COALESCE(n.name,''),
       b.uop_remaining,
       (SELECT MAX(a.applied_at) FROM bin_uop_audit a
        WHERE a.bin_id = b.id AND a.before_uop >= 0 AND a.after_uop < 0) AS negative_since,
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
// total is currently negative — i.e. the payloads for which the threshold
// monitor is REFUSING to signal replenishment.
//
// "We didn't order material for this payload because we don't know what's in
// it" is the sentence this makes available. It is the same condition
// checkBindings logs 1,119 times a day, expressed once, as state.
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
