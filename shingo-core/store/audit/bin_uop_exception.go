package audit

import (
	"database/sql"
	"encoding/json"
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
	// ExcHoldDropped: an Edge dropped some or all of a produce seat's hold
	// (pending_uop_delta) without replaying it onto a bin, and said so on
	// protocol.SubjectHoldDropped. bin_id is NULL (a hold has no carrier, the
	// v127 shape), payload_code the held part, actor who dropped it, op the
	// reason (a protocol.HoldDrop* constant), occurred_at the Edge's time, and
	// detail {node, qty, reason, station, order_id, msg_id}. One row per
	// envelope (AppendHoldDropped). Never recovered: recovered_at stays NULL.
	ExcHoldDropped = "hold_dropped"
)

// HoldDropRun is what /inventory lists of the hold_dropped rows: a run of them
// at one seat, of one part, for one reason, at one station, none more than the
// run gap after the one before. A line at its pack with a pack held drops a
// part a stroke (hold_cap) until a bin comes, so a run is one line with its
// total, never a line per stroke. Drops is the rows, Qty the parts.
type HoldDropRun struct {
	Node        string    `json:"node"`
	PayloadCode string    `json:"payload_code"`
	Reason      string    `json:"reason"`
	Station     string    `json:"station"`
	Drops       int64     `json:"drops"`
	Qty         int64     `json:"qty"`
	First       time.Time `json:"first"`
	Last        time.Time `json:"last"`
}

// holdDropDetail is the detail JSON of a hold_dropped row. MsgID is the
// envelope id the row is keyed on.
type holdDropDetail struct {
	Node    string `json:"node"`
	Qty     int64  `json:"qty"`
	Reason  string `json:"reason"`
	Station string `json:"station"`
	OrderID string `json:"order_id,omitempty"`
	MsgID   string `json:"msg_id"`
}

// AppendHoldDropped writes one hold_dropped row, once per msgID: a second call
// with the same msgID and occurredAt writes nothing and reports false. The
// Edge's outbox and Kafka both redeliver a message with its envelope id
// unchanged, and TypeData envelopes pass no inbox dedup, so the row carries
// the id and the INSERT checks for it in the same statement. The check reads
// idx_bin_uop_exception_open (kind, occurred_at) at one occurred_at, so it is
// a probe, not a scan. One statement.
func AppendHoldDropped(execer BinUOPExecer, msgID, station, node, payloadCode string, qty int64, reason, actor, orderID string, occurredAt time.Time) (bool, error) {
	if occurredAt.IsZero() {
		return false, fmt.Errorf("append hold_dropped %s: occurred_at is required", msgID)
	}
	// Postgres keeps microseconds; the key compares the stored value, so the
	// write and the check must carry the same one.
	at := occurredAt.UTC().Truncate(time.Microsecond)
	det, err := json.Marshal(holdDropDetail{Node: node, Qty: qty, Reason: reason, Station: station, OrderID: orderID, MsgID: msgID})
	if err != nil {
		return false, fmt.Errorf("append hold_dropped %s: encode detail: %w", msgID, err)
	}
	res, err := execer.Exec(`INSERT INTO bin_uop_exception (kind, bin_id, payload_code, actor, occurred_at, op, detail)
		SELECT $1, NULL, $2, $3, $4, $5, $6
		WHERE NOT EXISTS (SELECT 1 FROM bin_uop_exception
			WHERE kind = $1 AND recovered_at IS NULL AND occurred_at = $4 AND detail->>'msg_id' = $7)`,
		ExcHoldDropped, payloadCode, actor, at, reason, det, msgID)
	if err != nil {
		return false, fmt.Errorf("append hold_dropped %s node=%s: %w", msgID, node, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("append hold_dropped %s: rows affected: %w", msgID, err)
	}
	return n == 1, nil
}

// ListHoldDropRuns returns the hold_dropped rows that occurred at or after
// since as runs (HoldDropRun): a row more than gap after the last of its seat,
// part, reason and station starts a new run. Latest run first, at most limit
// runs; every row in the window is counted, whatever the limit. The reason is
// the row's op (AppendHoldDropped writes it there). One statement on
// idx_bin_uop_exception_open.
func ListHoldDropRuns(db *sql.DB, since time.Time, gap time.Duration, limit int) ([]HoldDropRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.Query(`WITH d AS (
			SELECT occurred_at, payload_code, COALESCE(op, '') AS reason,
			       COALESCE(detail->>'node', '') AS node, COALESCE(detail->>'station', '') AS station,
			       COALESCE((detail->>'qty')::bigint, 0) AS qty
			  FROM bin_uop_exception
			 WHERE kind = $1 AND recovered_at IS NULL AND occurred_at >= $2),
		s AS (
			SELECT *, CASE WHEN occurred_at - lag(occurred_at) OVER w <= make_interval(secs => $3)
			               THEN 0 ELSE 1 END AS starts
			  FROM d WINDOW w AS (PARTITION BY node, payload_code, reason, station ORDER BY occurred_at)),
		r AS (
			SELECT *, sum(starts) OVER (PARTITION BY node, payload_code, reason, station ORDER BY occurred_at) AS run
			  FROM s)
		SELECT node, payload_code, reason, station, count(*), sum(qty), min(occurred_at), max(occurred_at)
		  FROM r
		 GROUP BY node, payload_code, reason, station, run
		 ORDER BY max(occurred_at) DESC, node, payload_code
		 LIMIT $4`, ExcHoldDropped, since.UTC(), gap.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("list hold drop runs: %w", err)
	}
	defer rows.Close()
	out := []HoldDropRun{}
	for rows.Next() {
		var r HoldDropRun
		if err := rows.Scan(&r.Node, &r.PayloadCode, &r.Reason, &r.Station, &r.Drops, &r.Qty, &r.First, &r.Last); err != nil {
			return nil, fmt.Errorf("list hold drop runs: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeclaredFullStamp is one release_declared_full ledger row as the /inventory
// list reads it.
type DeclaredFullStamp struct {
	ID          int64  `json:"id"`
	BinID       int64  `json:"bin_id"`
	BinLabel    string `json:"bin_label"`
	Node        string `json:"node"`
	PayloadCode string `json:"payload_code"`
	Counted     int    `json:"counted"`
	Declared    int    `json:"declared"`
	Actor       string `json:"actor"`
	// Reason is the stamp's detail.reason: blank for a stamp to the pack,
	// no_pack (service.DeclaredFullNoPack) when the part had none and the
	// count was kept.
	Reason    string    `json:"reason,omitempty"`
	OrderID   *int64    `json:"order_id,omitempty"`
	AppliedAt time.Time `json:"applied_at"`
}

// ListDeclaredFullStamps returns the release_declared_full rows applied at or
// after since, newest first, at most limit. The node is the seat the bin was
// counted at (the stamp's detail), falling back to the node the bin stood at
// when stamped. One statement on idx_bin_uop_ledger_op_time.
func ListDeclaredFullStamps(db *sql.DB, since time.Time, limit int) ([]DeclaredFullStamp, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.Query(`SELECT l.id, l.bin_id, COALESCE(b.label, ''),
		       COALESCE(NULLIF(l.detail->>'node', ''), n.name, ''), l.payload_code,
		       COALESCE(l.before_uop, 0), l.after_uop, l.actor, COALESCE(l.detail->>'reason', ''), l.order_id, l.applied_at
		FROM bin_uop_ledger l
		LEFT JOIN bins b ON b.id = l.bin_id
		LEFT JOIN nodes n ON n.id = l.node_id
		WHERE l.op = $1 AND l.applied_at >= $2
		ORDER BY l.applied_at DESC, l.id DESC
		LIMIT $3`, OpReleaseDeclaredFull, since.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list declared-full stamps: %w", err)
	}
	defer rows.Close()
	out := []DeclaredFullStamp{}
	for rows.Next() {
		var st DeclaredFullStamp
		var order sql.NullInt64
		if err := rows.Scan(&st.ID, &st.BinID, &st.BinLabel, &st.Node, &st.PayloadCode,
			&st.Counted, &st.Declared, &st.Actor, &st.Reason, &order, &st.AppliedAt); err != nil {
			return nil, fmt.Errorf("list declared-full stamps: scan: %w", err)
		}
		if order.Valid {
			v := order.Int64
			st.OrderID = &v
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

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
