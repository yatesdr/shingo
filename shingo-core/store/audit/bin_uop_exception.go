package audit

import (
	"fmt"
	"time"
)

// bin_uop_exception.go — the permanent exceptions ledger (v93, owner decision
// D2: kept forever).
//
// THE TABLE IS THE RULING, MATERIALIZED. "How parts moved / were consumed or
// produced, and where the negatives are" is the durable question; until v93 it
// could only be answered from bin_uop_ledger's raw delta rows (~7,000/day), and
// the 90-day retention on those (D6, later in the same wave) is about to start
// deleting that derivation path. This table captures the ~140 events/day worth
// keeping — 3.6 of them negative crossings — at event time.
//
// NO RETENTION ON THIS TABLE, EVER (D2). ~10 MB/year. The system spends 7,000
// raw rows a day to capture the 140 that land here, so bounding this table
// would discard the cheap half and save nothing. If bounding is ever genuinely
// wanted, bound the two diagnostic kinds (stale_epoch, payload_mismatch — 97%
// of rows); NEVER the negative crossings.
//
// The backfill and the shape live in store/migrations.go (v93); the repointed
// permanent readers (OpenNegativeBins, ListBinUOPDiscrepancies) read this
// table, not the raw stream.

// Exception kinds. Stable strings — historical rows reference them.
const (
	// ExcNegativeCrossing: an applied delta took a bin from >= 0 to < 0. The
	// crossing, not the continuation — a bin going further negative is the
	// same excursion, and deepest_uop/recovered_at carry the shape. This is
	// the kind the owner named durable; never bound, never purged.
	ExcNegativeCrossing = "negative_crossing"
	// ExcStaleEpoch: a delta dropped for carrying a retired generation. The
	// dropped quantity lives in detail, same shape as the applier's metadata.
	ExcStaleEpoch = "stale_epoch"
	// ExcPayloadMismatch: a delta dropped for naming a payload the bin does
	// not hold (ALN_001 guard). Detail carries both labels.
	ExcPayloadMismatch = "payload_mismatch"
	// ExcBoundary: an EpochBumpOps write — the row that retires one binding
	// and starts the next. Diagnostic context for every other kind (a
	// crossing directly after a boundary is the release-race shape) and the
	// join key the daily roll-up segments on.
	ExcBoundary = "boundary"
	// ExcEdgeRollback: a station's count stream went backward — a message at
	// or below the scope's last applied seq, with a window that ends after the
	// last applied one (SYNTH-round2 S6). On a bin: detail carries the seqs,
	// the windows and the nets, and an edge_rollback boundary follows. On a
	// bucket scope: bin_id is NULL (v127 made it nullable), the station,
	// payload and core node name are on the row and in detail, and nothing
	// follows — a bucket has no generation to start.
	ExcEdgeRollback = "edge_rollback"
)

// AppendBinUOPException records one exception at event time, on the caller's
// transaction — the same contract as AppendBinUOP. deepestUOP and recoveredAt
// are only meaningful for negative_crossing rows written by the recovery
// updater; the event-time writer passes nil and the backfill/recovery paths
// fill them (deepest folds continuations; recovered_at is set when the bin
// next reads >= 0).
//
// detail may be nil; it is the applier's metadata blob verbatim when present.
// occurredAt is required — an exception without a timestamp is not an
// exception.
func AppendBinUOPException(execer BinUOPExecer, kind string, binID int64, payloadCode, actor string, epochSeq *int64, occurredAt time.Time, beforeUOP, afterUOP, deepestUOP *int, recoveredAt *time.Time, op string, detail []byte) error {
	var epoch any
	if epochSeq != nil {
		epoch = *epochSeq
	}
	var before any
	if beforeUOP != nil {
		before = *beforeUOP
	}
	var after any
	if afterUOP != nil {
		after = *afterUOP
	}
	var deepest any
	if deepestUOP != nil {
		deepest = *deepestUOP
	}
	var recovered any
	if recoveredAt != nil {
		recovered = *recoveredAt
	}
	var det any
	if len(detail) > 0 {
		det = []byte(detail)
	}
	if occurredAt.IsZero() {
		return fmt.Errorf("append bin_uop_exception bin=%d kind=%s: occurred_at is required", binID, kind)
	}
	if _, err := execer.Exec(`INSERT INTO bin_uop_exception
		(kind, bin_id, payload_code, actor, epoch_seq, occurred_at, before_uop, after_uop,
		 deepest_uop, recovered_at, op, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		kind, binID, payloadCode, actor, epoch, occurredAt.UTC(), before, after,
		deepest, recovered, op, det); err != nil {
		return fmt.Errorf("append bin_uop_exception bin=%d kind=%s: %w", binID, kind, err)
	}
	return nil
}

// RecoverBinUOPOpenCrossing closes the still-open negative_crossing for one
// bin — the event-time complement of the backfill's recovered_at. Called from
// the apply path whenever a delta lands the bin back at >= 0; a plain UPDATE
// because the open set per bin is at most one excursion (a bin cannot cross
// twice without recovering in between).
//
// deepest folds in the same excursion's continuations: the minimum after_uop
// between the crossing and now, read from the raw stream while it is still
// inside the retention window. A crossing whose continuations have already
// been purged keeps the crossing's own after_uop — the number that made the
// bin negative — rather than being silently unrecoverable.
// NextBinUOPEpochSeq returns the epoch_seq the next boundary row for one bin
// should carry: the count of boundary rows already written for it. The
// migration backfill numbers boundaries 1..n by the same count over the raw
// stream, so continuing that numbering at runtime is what keeps epoch_seq a
// stable epoch identifier after the raw stream ages out of the retention
// window — a NULL here would strand boundary rows out of any epoch.
func NextBinUOPEpochSeq(execer BinUOPExecer, binID int64) (int64, error) {
	var seq int64
	if err := execer.QueryRow(`SELECT count(*) FROM bin_uop_exception
		WHERE bin_id = $1 AND kind = $2`, binID, ExcBoundary).Scan(&seq); err != nil {
		return 0, fmt.Errorf("next epoch seq bin=%d: %w", binID, err)
	}
	return seq + 1, nil
}

func RecoverBinUOPOpenCrossing(execer BinUOPExecer, binID int64, recoveredAt time.Time) error {
	if _, err := execer.Exec(`
		UPDATE bin_uop_exception e
		SET deepest_uop = COALESCE((
		        SELECT MIN(d.after_uop) FROM bin_uop_ledger d
		        WHERE d.bin_id = e.bin_id AND d.applied_at >= e.occurred_at
		          AND d.applied_at <= $2), e.after_uop),
		    recovered_at = $2
		WHERE e.bin_id = $1 AND e.kind = $3 AND e.recovered_at IS NULL`,
		binID, recoveredAt.UTC(), ExcNegativeCrossing); err != nil {
		return fmt.Errorf("recover open crossing bin=%d: %w", binID, err)
	}
	return nil
}
