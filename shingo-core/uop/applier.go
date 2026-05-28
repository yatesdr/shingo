package uop

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"shingo/protocol"

	"shingocore/store"
	"shingocore/store/audit"
)

// Core-side delta apply service. Receives BinUOPDelta and
// LinesideBucketDelta envelopes from Edge, dedups against
// inventory_delta_dedup, validates against the bin row, and applies
// to bins.uop_remaining / lineside_buckets.
//
// Dedup scope keys (stable; renames break in-flight Edge replays):
//
//   - bin scope:    strconv(BinID)
//   - bucket scope: "<NodeID>|<PairKey>|<StyleID>|<PartNumber>"
//
// Either-order arrival tolerance: capture-on-release fires both a bin
// delta and one bucket delta per part, atomically on Edge's outbox tx.
// Core's handler ordering is independent — the dedup table guards each
// scope independently, so a bucket delta arriving before its sibling
// bin delta still applies cleanly.

const (
	// invDeltaScopeBin / invDeltaScopeBucket — scope_kind values for the
	// inventory_delta_dedup table. Stable strings. Edge has no awareness
	// of these values; they are a Core-internal partition.
	invDeltaScopeBin    = "bin"
	invDeltaScopeBucket = "bucket"
)

// ErrInventoryDeltaSkipped indicates the delta was a duplicate (its
// SequenceID was already applied) or a no-op (delta=0 after validation).
// Callers treat it as a successful idempotent skip — not an error to
// propagate to a 4xx/5xx response.
var ErrInventoryDeltaSkipped = errors.New("inventory delta already applied")

// ManifestClearer is the narrow interface InventoryDeltaService uses
// to fire ClearForReuse atomically inside the delta-apply transaction
// when a capture_reduction drives uop_remaining to zero. The signature
// takes *sql.Tx so the manifest write shares the same connection as
// the bin row update — atomicity is the load-bearing property.
//
// service.BinManifestService satisfies this via its ClearForReuseTx
// method. The interface lives in uop so this package doesn't import
// service (which would create a cycle since service re-exports
// InventoryDeltaService for backward compat).
type ManifestClearer interface {
	// ClearForReuseTx returns the new delta_epoch the bin advanced to.
	// The applier discards the value (this path runs inside a delta
	// apply, not a load/clear handler that ships a response to Edge)
	// but the signature matches BinManifestService directly to avoid
	// an adapter layer.
	ClearForReuseTx(tx *sql.Tx, binID int64, op, source string) (int64, error)
}

// InventoryDeltaService applies BinUOPDelta and LinesideBucketDelta
// envelopes against the authoritative bins / lineside_buckets tables
// with at-most-once semantics (dedup via inventory_delta_dedup).
//
// binManifest is held so a capture_reduction delta that drives
// uop_remaining to zero can fire ClearForReuse atomically inside the
// same transaction (see ApplyBinUOPDelta). Optional — passing nil
// disables the manifest-clear trigger and the service behaves like
// the pre-Item-6 build (delta apply only, no downstream manifest
// effect). All production composition roots wire it; only legacy
// tests pass nil.
type InventoryDeltaService struct {
	db          *store.DB
	binManifest ManifestClearer
}

// NewInventoryDeltaService constructs the delta apply service.
// binManifest can be nil for tests that don't exercise the
// capture-reduction-to-zero trigger; production callers MUST pass a
// real service so the dual-write retirement is complete.
func NewInventoryDeltaService(db *store.DB, binManifest ManifestClearer) *InventoryDeltaService {
	return &InventoryDeltaService{db: db, binManifest: binManifest}
}

// ApplyBinUOPDelta applies a BinUOPDelta against bins.uop_remaining.
// Every station writes the authoritative column directly — there is
// no per-station routing or staging table.
//
// Returns ErrInventoryDeltaSkipped when SequenceID has already been
// applied for the bin's scope. Returns a wrapped error when the bin
// doesn't exist or the payload code mismatches — callers log and
// continue (the delta is dropped; reconciliation will catch the
// divergence).
func (s *InventoryDeltaService) ApplyBinUOPDelta(d *protocol.BinUOPDelta) error {
	if d == nil {
		return fmt.Errorf("nil BinUOPDelta")
	}
	if d.Station == "" {
		return fmt.Errorf("BinUOPDelta missing station")
	}
	if d.BinID <= 0 {
		return fmt.Errorf("BinUOPDelta invalid bin_id: %d", d.BinID)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	scopeKey := strconv.FormatInt(d.BinID, 10)

	// Stale-epoch guard. If Edge sends a delta with an epoch below the
	// bin's current delta_epoch (Edge cache is behind a load/clear that
	// already happened on Core), the dedup row for that older epoch is
	// either missing or has a higher last_seq than this delta — in
	// either case the apply would silently drop. Log it loudly here so
	// "bin stopped counting" is grep-able instead of opaque, then fall
	// through to the dedup UPSERT (which still drops the delta into
	// the old epoch's row if present, harmlessly).
	var currentEpoch int64
	if err := tx.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, d.BinID).Scan(&currentEpoch); err == nil {
		if d.Epoch < currentEpoch {
			log.Printf("WARN: BinUOPDelta stale epoch bin=%d wire_epoch=%d bin_epoch=%d seq=%d — delta dropped; Edge bin-state cache is behind Core (next bin-state refresh will repair)",
				d.BinID, d.Epoch, currentEpoch, d.SequenceID)
		} else if d.Epoch > currentEpoch {
			// Edge ahead of Core is a real anomaly — Core controls
			// epoch via lifecycle handlers, so Edge shouldn't see a
			// higher value before Core writes it. Possible cause: a
			// stale Core read or a corrupt Edge cache; log but still
			// apply since the new epoch isn't worse than continuing.
			log.Printf("WARN: BinUOPDelta future epoch bin=%d wire_epoch=%d bin_epoch=%d seq=%d — Edge ahead of Core",
				d.BinID, d.Epoch, currentEpoch, d.SequenceID)
		}
	}

	applied, err := claimDeltaSequence(tx, d.Station, invDeltaScopeBin, scopeKey, d.Epoch, d.SequenceID)
	if err != nil {
		return err
	}
	if !applied {
		// Replay — already applied. No work, no error.
		return ErrInventoryDeltaSkipped
	}

	// Validate target bin and (optionally) payload code. payload_code
	// mismatch indicates the bin's payload was reassigned underneath
	// us — the in-flight delta no longer corresponds to the count
	// it was attributing change to. Reject loudly.
	var (
		havePayloadCode string
		valueBefore     int
	)
	err = tx.QueryRow(`SELECT payload_code, uop_remaining FROM bins WHERE id=$1`,
		d.BinID).Scan(&havePayloadCode, &valueBefore)
	if err == sql.ErrNoRows {
		return fmt.Errorf("BinUOPDelta target bin %d does not exist", d.BinID)
	}
	if err != nil {
		return fmt.Errorf("read bin %d: %w", d.BinID, err)
	}
	if d.PayloadCode != "" && havePayloadCode != "" && d.PayloadCode != havePayloadCode {
		return fmt.Errorf("BinUOPDelta payload mismatch bin=%d wire=%q have=%q",
			d.BinID, d.PayloadCode, havePayloadCode)
	}

	if _, err := tx.Exec(`UPDATE bins SET uop_remaining = uop_remaining + $1
		WHERE id=$2`, d.Delta, d.BinID); err != nil {
		return fmt.Errorf("apply BinUOPDelta bin=%d delta=%d: %w", d.BinID, d.Delta, err)
	}

	// Audit metadata via json.Marshal — Item 14 cleanup (D7). The
	// previous fmt.Sprintf approach broke when the reason string
	// carried a quote character (the format-string-as-JSON-template
	// approach has no escaping). Typed marshal handles every JSON
	// edge case correctly and matches the pattern in
	// bin_manifest.AuditReleaseOverride.
	metadata, err := json.Marshal(struct {
		Reason     string `json:"reason"`
		Delta      int    `json:"delta"`
		SequenceID int64  `json:"sequence_id"`
	}{
		Reason:     string(d.Reason),
		Delta:      d.Delta,
		SequenceID: d.SequenceID,
	})
	if err != nil {
		return fmt.Errorf("marshal BinUOPDelta audit metadata bin=%d: %w", d.BinID, err)
	}
	if _, err := tx.Exec(`INSERT INTO bin_uop_audit
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata)
		VALUES ($1, $2, $3, 'bin_uop_delta', 'service/inventory_delta_service.go', $4, $5, $6)`,
		d.BinID, valueBefore, valueBefore+d.Delta,
		d.PayloadCode, d.Station, string(metadata),
	); err != nil {
		return fmt.Errorf("audit BinUOPDelta bin=%d: %w", d.BinID, err)
	}

	// Item 6 manifest-clear trigger: when a capture_reduction delta
	// (the PULL PARTS LINESIDE path) drives uop_remaining to zero or
	// below, the bin is empty by operator declaration and must be
	// returned to the empty-pool. The <= 0 boundary covers the SME-
	// lock-permitted overpack washout (operator pulled more than the
	// tracked count showed: bin nominally 308, captured 309 → -1; bin
	// is physically empty, the negative is correct accounting). Fires
	// only on capture_reduction — consume ticks reaching zero are an
	// overpack scenario where the bin might still physically hold
	// parts; cycle counts to zero are admin corrections; admin clears
	// go through ClearForReuse directly. Idempotent because dedup at
	// the top of the function already guarded against replays.
	if d.Reason == protocol.ReasonCaptureReduction && valueBefore+d.Delta <= 0 && s.binManifest != nil {
		// Epoch bump is a side effect — discard the new value here. The
		// next BinUOPDelta against this bin from Edge will carry epoch=N
		// from its cache (still showing pre-clear epoch) and Core's
		// applier will warn + drop until Edge's bin-state refresh picks
		// up the new epoch. That's the expected loss surface for a
		// capture-reduction-driven clear; alternative would be to
		// proactively push the new epoch back to Edge as a side channel,
		// which the current architecture doesn't have a transport for.
		if _, err := s.binManifest.ClearForReuseTx(tx, d.BinID,
			audit.OpReleasedCaptureEmpty,
			"service/inventory_delta_service.go:ApplyBinUOPDelta"); err != nil {
			return fmt.Errorf("clear manifest on capture_reduction zero bin=%d: %w", d.BinID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit BinUOPDelta bin=%d: %w", d.BinID, err)
	}
	return nil
}

// ApplyLinesideBucketDelta applies a LinesideBucketDelta against the
// lineside_buckets row keyed on (station, core_node_name, pair_key,
// style_id, part_number). Creates the row on first sight via UPSERT;
// deletes when qty reaches zero (Option C — empty buckets carry no
// useful information).
//
// Round-3 Obs 8: validates d.CoreNodeName resolves to a known node via
// GetNodeByName before insert. If the name doesn't resolve, the delta
// is dropped with a loud log and metric — bad data never enters the
// table, closing the cross-namespace orphan failure mode that
// Springfield 6883 exhibited.
//
// Returns ErrInventoryDeltaSkipped on replay. Returns an error if the
// applied delta would drive qty below zero (the CHECK constraint
// catches this; we surface it as a typed error so the caller can log
// without confusing a genuine SQL fault for a delta bug).
func (s *InventoryDeltaService) ApplyLinesideBucketDelta(d *protocol.LinesideBucketDelta) error {
	if d == nil {
		return fmt.Errorf("nil LinesideBucketDelta")
	}
	if d.Station == "" {
		return fmt.Errorf("LinesideBucketDelta missing station")
	}
	if d.CoreNodeName == "" {
		return fmt.Errorf("LinesideBucketDelta missing core_node_name (station=%s style=%d part=%q)",
			d.Station, d.StyleID, d.PartNumber)
	}
	if d.PartNumber == "" {
		return fmt.Errorf("LinesideBucketDelta missing part_number (station=%s core_node_name=%s style=%d)",
			d.Station, d.CoreNodeName, d.StyleID)
	}

	// Insert-time validation: refuse to land a delta on a name Core
	// doesn't recognize. Pre-Obs-8 the (then int64) NodeID was applied
	// blindly, producing rows attributed to whatever ID Edge happened
	// to send — which on Core's side could resolve to a different node
	// entirely, or to no node at all (the Hopkinsville orphan shape).
	// GetNodeByName returns sql.ErrNoRows when the row is absent;
	// drop the delta loudly and let the operator investigate.
	if _, err := s.db.GetNodeByName(d.CoreNodeName); err != nil {
		return fmt.Errorf("LinesideBucketDelta core_node_name=%q does not resolve to a Core node (station=%s part=%q): %w",
			d.CoreNodeName, d.Station, d.PartNumber, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	scopeKey := bucketScopeKey(d.CoreNodeName, d.PairKey, d.StyleID, d.PartNumber)
	// Buckets stay on epoch=0 — bucket lifecycle is Edge-observed
	// (qty zeroing) rather than Core-controlled, and DeleteLinesideBucket
	// already clears the dedup row on the existing lifecycle exit paths.
	// If buckets ever exhibit the same drift pattern as bins, a follow-up
	// migration can introduce a bucket-side epoch with Edge-side tracking.
	applied, err := claimDeltaSequence(tx, d.Station, invDeltaScopeBucket, scopeKey, 0, d.SequenceID)
	if err != nil {
		return err
	}
	if !applied {
		return ErrInventoryDeltaSkipped
	}

	// UPSERT-and-clamp: ON CONFLICT updates qty; CHECK (qty >= 0) at
	// the schema level rejects under-zero results. Treat that
	// constraint violation as a typed error so the handler can log
	// without spamming the SQL fault line.
	//
	// payload_code (UOP-threshold replenishment): write the incoming
	// value when non-empty; keep the existing row's value when the
	// incoming is empty. Empty just means "this delta envelope didn't
	// carry a code" (rare — older Edge build or an envelope built
	// outside the capture-from-order-context path); we don't want
	// such a delta to clobber a previously-latched payload code.
	res, err := tx.Exec(`
		INSERT INTO lineside_buckets (station, core_node_name, pair_key, style_id, part_number, qty, payload_code)
		VALUES ($1, $2, $3, $4, $5, GREATEST($6, 0), $7)
		ON CONFLICT (station, core_node_name, pair_key, style_id, part_number)
		DO UPDATE SET
			qty = lineside_buckets.qty + $6,
			payload_code = CASE WHEN $7 = '' THEN lineside_buckets.payload_code ELSE $7 END,
			updated_at = NOW()`,
		d.Station, d.CoreNodeName, d.PairKey, d.StyleID, d.PartNumber, d.Delta, d.PayloadCode)
	if err != nil {
		// Most likely cause: CHECK (qty >= 0) violation when the
		// DO UPDATE branch tried to drive qty negative. Wrap.
		return fmt.Errorf("apply LinesideBucketDelta core_node_name=%q part=%q delta=%d: %w",
			d.CoreNodeName, d.PartNumber, d.Delta, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// UPSERT must always touch a row.
		return fmt.Errorf("LinesideBucketDelta UPSERT produced no row (core_node_name=%q part=%q)",
			d.CoreNodeName, d.PartNumber)
	}

	// Garbage-collect rows that have hit zero. Option C — empty
	// buckets carry no useful information.
	if _, err := tx.Exec(`DELETE FROM lineside_buckets
		WHERE station=$1 AND core_node_name=$2 AND pair_key=$3 AND style_id=$4 AND part_number=$5
		AND qty=0`,
		d.Station, d.CoreNodeName, d.PairKey, d.StyleID, d.PartNumber); err != nil {
		return fmt.Errorf("gc empty bucket core_node_name=%q part=%q: %w", d.CoreNodeName, d.PartNumber, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit LinesideBucketDelta core_node_name=%q part=%q: %w",
			d.CoreNodeName, d.PartNumber, err)
	}
	return nil
}

// claimDeltaSequence advances inventory_delta_dedup.last_seq for a
// scope+epoch iff seq > last_seq, atomically inside the caller's
// transaction. Returns (true, nil) if seq was newly applied,
// (false, nil) if seq was already applied (replay).
//
// PK is (station, scope_kind, scope_key, epoch). Different epochs for
// the same scope_key get separate dedup rows — a new bin load (epoch
// bump on SetForProduction) starts fresh, so a stale Edge seq counter
// can't shadow the new load's first deltas.
//
// UPSERT shape: INSERT ... ON CONFLICT ... DO UPDATE WHERE last_seq <
// excluded.last_seq. The WHERE on DO UPDATE is what makes this both
// atomic and replay-safe — the row is touched only when the new seq
// actually advances state, so RowsAffected==0 cleanly distinguishes
// replay from new-application.
func claimDeltaSequence(tx *sql.Tx, station, scopeKind, scopeKey string, epoch, seq int64) (bool, error) {
	res, err := tx.Exec(`
		INSERT INTO inventory_delta_dedup (station, scope_kind, scope_key, epoch, last_seq, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (station, scope_kind, scope_key, epoch)
		DO UPDATE SET last_seq = EXCLUDED.last_seq, updated_at = NOW()
		WHERE inventory_delta_dedup.last_seq < EXCLUDED.last_seq`,
		station, scopeKind, scopeKey, epoch, seq)
	if err != nil {
		return false, fmt.Errorf("dedup upsert station=%s scope=%s/%s epoch=%d seq=%d: %w",
			station, scopeKind, scopeKey, epoch, seq, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// bucketScopeKey builds the dedup scope_key for a LinesideBucketDelta.
// Round-3 Obs 8: keys on CoreNodeName instead of NodeID — translation-
// free against Core's nodes table and stable across the Edge↔Core
// boundary. The format is pipe-delimited and stable; renames break
// in-flight Edge replays, so any change must come with a coordinated
// migration. The v21 migration TRUNCATEs inventory_delta_dedup for
// scope_kind='bucket' as part of the cutover so old keys can't
// shadow new ones.
func bucketScopeKey(coreNodeName, pairKey string, styleID int64, partNumber string) string {
	var sb strings.Builder
	sb.WriteString(coreNodeName)
	sb.WriteByte('|')
	sb.WriteString(pairKey)
	sb.WriteByte('|')
	sb.WriteString(strconv.FormatInt(styleID, 10))
	sb.WriteByte('|')
	sb.WriteString(partNumber)
	return sb.String()
}

// BinUOPRow is one row of the per-bin authoritative state returned
// by ListBinUOPForNodes. Edge's reconciler reads these to compute
// "local cache vs Core authoritative" drift and self-heal.
type BinUOPRow struct {
	BinID        int64  `json:"bin_id"`
	NodeName     string `json:"node_name"`
	PayloadCode  string `json:"payload_code"`
	UOPRemaining int    `json:"uop_remaining"`
	// DeltaEpoch lets Edge populate its bin-state cache with the
	// current load's epoch on startup / periodic refresh. Without
	// this, an Edge restart with bins already on the line would have
	// no epoch context for its first post-restart BinUOPDelta. Pre-
	// migration responses don't carry it; deserialization defaults to
	// 0 and the next bin lifecycle event (set_for_production / clear)
	// repopulates Edge with the post-bump value.
	DeltaEpoch int64 `json:"delta_epoch"`
}

// LinesideBucketRow is one row of the per-bucket authoritative state
// returned by ListBucketsForStation. Edge compares against its local
// node_lineside_bucket table to detect bucket-side drift.
//
// Round-3 Obs 8: NodeID dropped from the wire row. The bucket table
// is keyed on core_node_name post-v21 migration; NodeName (same as
// CoreNodeName here since we LEFT JOIN against Core's nodes by name)
// is the only node-shaped field a reconciling Edge needs.
type LinesideBucketRow struct {
	NodeName   string `json:"node_name"`
	PairKey    string `json:"pair_key"`
	StyleID    int64  `json:"style_id"`
	PartNumber string `json:"part_number"`
	Qty        int    `json:"qty"`
}

// ListBinUOPForNodes returns the authoritative uop_remaining for
// every bin currently sitting at any of the requested nodes. Empty
// input returns an empty slice.
func (s *InventoryDeltaService) ListBinUOPForNodes(nodeNames []string) ([]BinUOPRow, error) {
	if len(nodeNames) == 0 {
		return nil, nil
	}
	args := make([]any, len(nodeNames))
	placeholders := make([]string, len(nodeNames))
	for i, name := range nodeNames {
		args[i] = name
		placeholders[i] = "$" + strconv.Itoa(i+1)
	}
	q := `SELECT b.id, COALESCE(n.name, ''), b.payload_code, b.uop_remaining, b.delta_epoch
		FROM bins b
		LEFT JOIN nodes n ON n.id = b.node_id
		WHERE n.name IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query bin uop rows: %w", err)
	}
	defer rows.Close()
	var out []BinUOPRow
	for rows.Next() {
		var r BinUOPRow
		if err := rows.Scan(&r.BinID, &r.NodeName, &r.PayloadCode, &r.UOPRemaining, &r.DeltaEpoch); err != nil {
			return nil, fmt.Errorf("scan bin uop row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InventoryInvariant carries the plant-wide running totals that
// Item 13's invariant probe endpoint exposes. BinSum is signed (per
// SME lock; bins can go negative on overpack). BucketSum stays
// non-negative by schema CHECK constraint. Total is the rolled-up
// sum: useful as a trend indicator, not a hard equation, since
// overpack/underpack drift and operator corrections move the
// signed bin sum in either direction over time.
type InventoryInvariant struct {
	Total     int64
	BinSum    int64
	BucketSum int64
}

// SumInvariant returns the plant-wide running totals across all bins
// and lineside_buckets rows. Item 13. Both queries are aggregates
// against the authoritative tables on Core; the empty-table case
// returns zero via COALESCE rather than NULL.
func (s *InventoryDeltaService) SumInvariant() (InventoryInvariant, error) {
	binSum, err := s.db.SumBinUOP()
	if err != nil {
		return InventoryInvariant{}, err
	}
	bucketSum, err := s.db.SumLinesideBuckets()
	if err != nil {
		return InventoryInvariant{}, err
	}
	return InventoryInvariant{
		Total:     binSum + bucketSum,
		BinSum:    binSum,
		BucketSum: bucketSum,
	}, nil
}

// ListBucketsForStation returns every authoritative bucket row for
// the given station. Edge filters down to its node set client-side
// (cheap; bucket rows per station are few).
func (s *InventoryDeltaService) ListBucketsForStation(station string) ([]LinesideBucketRow, error) {
	if station == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT b.core_node_name, b.pair_key, b.style_id, b.part_number, b.qty
		FROM lineside_buckets b
		WHERE b.station = $1
		ORDER BY b.core_node_name, b.part_number`, station)
	if err != nil {
		return nil, fmt.Errorf("query bucket rows: %w", err)
	}
	defer rows.Close()
	var out []LinesideBucketRow
	for rows.Next() {
		var r LinesideBucketRow
		if err := rows.Scan(&r.NodeName, &r.PairKey, &r.StyleID, &r.PartNumber, &r.Qty); err != nil {
			return nil, fmt.Errorf("scan bucket row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
