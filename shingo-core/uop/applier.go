package uop

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shingo/protocol"

	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/messaging"
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
	//
	// binTypeID is always nil on this path (auto-clear on UOP zero);
	// the parameter exists so the interface matches BinManifestService.
	ClearForReuseTx(tx *sql.Tx, binID int64, binTypeID *int64, op, source string) (int64, error)
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
	// Reason-split dropped-delta counters (P2-C6). Process-lifetime, atomic;
	// int64 first so they stay 64-bit aligned for atomic ops on 32-bit builds.
	// Read via DroppedDeltaCounts / AnomalySummary. Counters only — they change
	// no apply behavior; they make the two silent drop paths measurable so the
	// inventory page can show a rejected-delta rate instead of a bare log line.
	droppedStaleEpoch      int64
	droppedPayloadMismatch int64

	db          *store.DB
	binManifest ManifestClearer

	// announce addresses the reply sent when a count is discarded for
	// carrying a generation that has ended — see repairEpoch.
	announce messaging.EpochAnnounce

	// repairedMu/repaired is the debounce: bin id → the generation the last
	// reply for that carrier carried. Process-lifetime and deliberately not
	// persisted — a Core restart may re-send one reply per carrier, which is
	// cheap, and the alternative is a table whose only job is to suppress a
	// message that costs nothing to repeat.
	//
	// One reply per generation. The reply is fire-and-forget and the station
	// may be an older build that does not know the message at all, in which
	// case the discarded counts keep arriving — 3,200 in a day at one plant —
	// and a reply per discarded count would be a flood aimed at something that
	// is not listening. If the station never adopts, the next reset makes a new
	// generation and the reply goes out again.
	//
	// NOT keyed off bins.anomaly_at. That column is a latch: it is set on the
	// first drop and stays set, so using it as the gate would suppress the
	// repair forever after the first one.
	repairedMu sync.Mutex
	repaired   map[int64]int64
}

// NewInventoryDeltaService constructs the delta apply service.
// binManifest can be nil for tests that don't exercise the
// capture-reduction-to-zero trigger; production callers MUST pass a
// real service so the dual-write retirement is complete.
//
// announce is where the reply to a discarded count goes. An unwired one
// (zero value) disables the repair and logs when it would have fired.
func NewInventoryDeltaService(db *store.DB, binManifest ManifestClearer, announce messaging.EpochAnnounce) *InventoryDeltaService {
	return &InventoryDeltaService{
		db:          db,
		binManifest: binManifest,
		announce:    announce,
		repaired:    make(map[int64]int64),
	}
}

// repairEpoch replies to a discarded count with the generation that is
// current, on the caller's transaction. Reports whether a reply was queued.
//
// THE REPLY CARRIES THE GENERATION AND NOTHING ELSE. Nobody declared a count
// here. Core noticed a count arrive stamped with a generation that had ended,
// which proves the station is behind and says nothing whatever about how many
// parts are in the carrier — Core's own number is behind by exactly the counts
// it has been discarding. The station is the authority on what is happening at
// the slot; Core is the authority on what the carrier is. So the stamp goes
// down, and the truth comes back up in the counts that now land.
//
// That is also why this cannot ride the ordinary adjustment message: its count
// field has no absent value, so an adjustment sent to carry only a generation
// says "zero", and the station would write it. See protocol.BinEpochRefresh.
func (s *InventoryDeltaService) repairEpoch(tx *sql.Tx, binID, currentEpoch int64) (bool, error) {
	if !s.announce.Wired() {
		log.Printf("stale-epoch drop bin=%d: no announce topic wired, cannot tell the station "+
			"it is behind — every count it reports for this carrier will keep being discarded", binID)
		return false, nil
	}
	if s.alreadyRepaired(binID, currentEpoch) {
		return false, nil
	}
	var nodeName string
	if err := tx.QueryRow(
		`SELECT COALESCE((SELECT n.name FROM nodes n WHERE n.id = b.node_id), '')
		 FROM bins b WHERE b.id=$1`, binID).Scan(&nodeName); err != nil {
		return false, fmt.Errorf("resolve node for epoch repair bin=%d: %w", binID, err)
	}
	if nodeName == "" {
		// The carrier is at no node, so no station is modelling it and there is
		// nobody the reply could be for.
		return false, nil
	}
	if err := s.announce.Send(tx, protocol.SubjectBinEpochRefresh, &protocol.BinEpochRefresh{
		BinID:        binID,
		CoreNodeName: nodeName,
		Epoch:        currentEpoch,
	}); err != nil {
		return false, fmt.Errorf("send epoch refresh bin=%d epoch=%d: %w", binID, currentEpoch, err)
	}
	return true, nil
}

// alreadyRepaired reports whether a reply for this carrier's current
// generation has already gone out — see the repaired map's comment.
func (s *InventoryDeltaService) alreadyRepaired(binID, epoch int64) bool {
	s.repairedMu.Lock()
	defer s.repairedMu.Unlock()
	return s.repaired[binID] == epoch
}

// markRepaired records a queued reply. Called after the commit, so a
// transaction that rolls back does not suppress the next attempt.
func (s *InventoryDeltaService) markRepaired(binID, epoch int64) {
	s.repairedMu.Lock()
	defer s.repairedMu.Unlock()
	if s.repaired == nil {
		s.repaired = make(map[int64]int64)
	}
	s.repaired[binID] = epoch
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
func (s *InventoryDeltaService) ApplyBinUOPDelta(station string, d *protocol.BinUOPDelta) error {
	if d == nil {
		return fmt.Errorf("nil BinUOPDelta")
	}
	if station == "" {
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

	// Stale-epoch guard. Every Core-side count reset bumps the bin's
	// delta_epoch (service.bumpEpoch), so a delta whose wire epoch is below
	// the bin's current epoch belongs to a retired generation — Edge cached
	// the old epoch and counted against a bin that has since been loaded,
	// cleared, or released. Drop it (applying would corrupt the post-reset
	// count) and record the dropped quantity as a discrepancy observation so
	// it is reportable instead of vanishing silently. The station is then told
	// which generation is current, so its next count lands (repairEpoch).
	//
	// That sentence used to read "Edge re-seeds the new epoch on its next
	// bin-state refresh." There was no bin-state refresh. Nothing on the Edge
	// polled for a generation; the five things that wrote it were all bind
	// points driven by order traffic, and at a plant whose orders are all
	// terminal none of them ever fired. So this branch was a dead end that
	// described itself as self-healing, and that sentence — repeated in the log
	// line an engineer reads while diagnosing — is why the condition was
	// written off as expected noise for a week while half a plant's production
	// counts went into the discrepancy table.
	//
	// The >0 clause is load-bearing: epoch 0 is the bootstrap/unknown
	// sentinel (Edge restart, fresh runtime, the ADD-COLUMN backfill) and
	// must always apply, never drop.
	//
	// Known limitation (bounded and deliberate): dropping a stale-epoch
	// consume delta protects the post-reset count and records the delta as a
	// discrepancy observation. It does not attempt to attribute a late delta
	// between the bin that was released and the bin that succeeds it at the
	// slot, and it does not resolve why a count reaches a release with a
	// non-zero or negative remainder. Reconciling release-time inventory
	// discrepancies — and the related case of a bin released while still
	// being consumed — is a known inventory-accuracy follow-up, intentionally
	// out of scope here. The behavior is observable, not silent: every
	// dropped delta writes a discrepancy audit row.
	var currentEpoch int64
	if err := tx.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, d.BinID).Scan(&currentEpoch); err == nil {
		switch {
		case d.Epoch > 0 && d.Epoch < currentEpoch:
			var before int
			if err := tx.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, d.BinID).Scan(&before); err != nil {
				return fmt.Errorf("read bin %d for stale-epoch audit: %w", d.BinID, err)
			}
			metadata, err := json.Marshal(struct {
				WireEpoch  int64 `json:"wire_epoch"`
				BinEpoch   int64 `json:"bin_epoch"`
				SequenceID int64 `json:"sequence_id"`
				Delta      int   `json:"delta"`
			}{d.Epoch, currentEpoch, d.SequenceID, d.Delta})
			if err != nil {
				return fmt.Errorf("marshal stale-epoch audit metadata bin=%d: %w", d.BinID, err)
			}
			// Observation row: before == after (count unchanged), the dropped
			// delta lives in metadata. AppendBinUOPOverride is the no-paired-
			// write shape and writes the same metadata column as the normal
			// bin_uop_delta rows.
			if err := audit.AppendBinUOPOverride(tx, d.BinID, before, before,
				audit.OpStaleEpochDropped, "service/inventory_delta_service.go:staleEpoch",
				nil, d.PayloadCode, station, metadata); err != nil {
				return err
			}
			// Flag the bin so the bins page surfaces a carrier whose deltas are
			// being refused (P2-C6). Payload-mismatch drops already do this; a
			// stale-epoch drop is just as much a "counts aren't landing" signal.
			// Visibility only — anomaly_at gates no claim or dispatch predicate.
			// COALESCE keeps the first-seen timestamp on repeated drops.
			if _, err := tx.Exec(`UPDATE bins SET anomaly_at=COALESCE(anomaly_at, NOW()) WHERE id=$1`, d.BinID); err != nil {
				return fmt.Errorf("flag anomaly on stale-epoch drop bin=%d: %w", d.BinID, err)
			}
			// ANSWER THE DROP. This is the one point in the system that holds
			// all four facts at once: which carrier, which generation is
			// current, which station is behind, and proof that it is behind.
			// Reply with the current generation and the station's next count
			// lands. The reply rides the SAME transaction as the audit row, so
			// a discarded count is never recorded without the answer that ends
			// the stall going with it.
			repaired, err := s.repairEpoch(tx, d.BinID, currentEpoch)
			if err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit stale-epoch drop bin=%d: %w", d.BinID, err)
			}
			if repaired {
				s.markRepaired(d.BinID, currentEpoch)
			}
			atomic.AddInt64(&s.droppedStaleEpoch, 1)
			log.Printf("BinUOPDelta stale epoch DROPPED bin=%d wire_epoch=%d bin_epoch=%d seq=%d delta=%d — routed to discrepancy audit; epoch refresh sent=%t",
				d.BinID, d.Epoch, currentEpoch, d.SequenceID, d.Delta, repaired)
			return ErrInventoryDeltaSkipped
		case d.Epoch > currentEpoch:
			// Edge ahead of Core is a real anomaly — Core controls epoch via
			// lifecycle handlers, so Edge shouldn't see a higher value before
			// Core writes it. Possible cause: a stale Core read or a corrupt
			// Edge cache; log but still apply since the new epoch isn't worse
			// than continuing.
			log.Printf("WARN: BinUOPDelta future epoch bin=%d wire_epoch=%d bin_epoch=%d seq=%d — Edge ahead of Core",
				d.BinID, d.Epoch, currentEpoch, d.SequenceID)
		}
	}

	applied, err := claimDeltaSequence(tx, station, invDeltaScopeBin, scopeKey, d.Epoch, d.SequenceID)
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
		anomalyFlagged  bool
	)
	err = tx.QueryRow(`SELECT payload_code, uop_remaining, anomaly_at IS NOT NULL FROM bins WHERE id=$1`,
		d.BinID).Scan(&havePayloadCode, &valueBefore, &anomalyFlagged)
	if err == sql.ErrNoRows {
		return fmt.Errorf("BinUOPDelta target bin %d does not exist", d.BinID)
	}
	if err != nil {
		return fmt.Errorf("read bin %d: %w", d.BinID, err)
	}

	// Produce-tick identity binding: a produce tick is physical proof of what
	// the press is filling the bin with — stronger evidence than any label
	// typed at a load screen — and a produce count must never freeze on a
	// label disagreement (HK 2026-07-16: stale hand-typed labels on the
	// press fronts froze counting for 480+544 parts after cutover). Two
	// cases when the wire payload differs from the bin's label:
	//
	//   - count == 0: routine first-delta bind. The designed blank fresh
	//     carrier (or a stale label on an empty carrier) takes the produced
	//     payload. Quiet audit row; no anomaly.
	//   - count != 0: the tote already holds units under another label —
	//     rebind to what is physically being produced, KEEP COUNTING (the
	//     tote's unit total stays correct), and flag the bin anomaly with an
	//     observation row recording the old label and the units aboard at
	//     the flip. The anomaly is a "cycle count me later" marker for the
	//     mixed contents — it gates nothing (anomaly_at feeds no claim or
	//     dispatch predicate).
	//
	// Deliberately NO delta_epoch bump in either case: there is no retired
	// count stream to fence off, and a bump opens a stale-epoch drop window
	// until Edge's next bin-state refresh (observed live 2026-07-16 14:02Z).
	// Non-produce reasons keep the hard reject below — a consume drain
	// against a wrong-labeled bin is the ALN_001 corruption case.
	if d.Reason == protocol.ReasonProduceTick && d.PayloadCode != "" &&
		havePayloadCode != d.PayloadCode {
		if valueBefore == 0 {
			if _, err := tx.Exec(`UPDATE bins SET payload_code=$1 WHERE id=$2`, d.PayloadCode, d.BinID); err != nil {
				return fmt.Errorf("bind payload on first delta bin=%d: %w", d.BinID, err)
			}
			metadata, merr := json.Marshal(struct {
				OldPayload string `json:"old_payload"`
				SequenceID int64  `json:"sequence_id"`
			}{havePayloadCode, d.SequenceID})
			if merr != nil {
				return fmt.Errorf("marshal first-delta-bind audit metadata bin=%d: %w", d.BinID, merr)
			}
			if err := audit.AppendBinUOPOverride(tx, d.BinID, valueBefore, valueBefore,
				audit.OpPayloadBoundFirstDelta, "service/inventory_delta_service.go:firstDeltaBind",
				nil, d.PayloadCode, station, metadata); err != nil {
				return err
			}
		} else {
			// Anomaly write rides the rebind UPDATE — same tx, same row; a
			// separate s.db write here would block on this tx's own row lock.
			if _, err := tx.Exec(`UPDATE bins SET payload_code=$1, anomaly_at=COALESCE(anomaly_at, NOW()) WHERE id=$2`,
				d.PayloadCode, d.BinID); err != nil {
				return fmt.Errorf("rebind payload with inventory bin=%d: %w", d.BinID, err)
			}
			metadata, merr := json.Marshal(struct {
				OldPayload        string `json:"old_payload"`
				InventoryAtRebind int    `json:"inventory_at_rebind"`
				SequenceID        int64  `json:"sequence_id"`
				Delta             int    `json:"delta"`
			}{havePayloadCode, valueBefore, d.SequenceID, d.Delta})
			if merr != nil {
				return fmt.Errorf("marshal rebind audit metadata bin=%d: %w", d.BinID, merr)
			}
			if err := audit.AppendBinUOPOverride(tx, d.BinID, valueBefore, valueBefore,
				audit.OpPayloadReboundWithInventory, "service/inventory_delta_service.go:reboundWithInventory",
				nil, d.PayloadCode, station, metadata); err != nil {
				return err
			}
			log.Printf("BinUOPDelta payload REBOUND with inventory bin=%d %q→%q units_aboard=%d seq=%d — counting continues; bin flagged for cycle count",
				d.BinID, havePayloadCode, d.PayloadCode, valueBefore, d.SequenceID)
		}
		havePayloadCode = d.PayloadCode
	}

	if d.PayloadCode != "" && havePayloadCode != "" && d.PayloadCode != havePayloadCode {
		// Non-produce mismatch: dropping the count is correct — never let a
		// consume/capture delta land on inventory it doesn't describe
		// (ALN_001). But make the drop VISIBLE: pre-fix the only signal was
		// this returned error in the core_handler log. The observability
		// writes go through s.db, NOT this tx — the return below rolls the
		// tx back so the dedup seq stays unconsumed (replay semantics
		// unchanged), and the tx holds no bins-row lock on this path.
		s.recordRejectedDelta(station, d, havePayloadCode, valueBefore, anomalyFlagged)
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
		d.PayloadCode, station, string(metadata),
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
		// The new generation is returned and discarded here because the
		// clear announces it itself, from inside the bump, in this same
		// transaction (service.BinManifestService.bumpEpoch).
		//
		// This comment used to say the opposite: that the next count would
		// be dropped until "Edge's bin-state refresh" picked up the new
		// generation, that this was "the expected loss surface", and that
		// pushing the generation back "the current architecture doesn't
		// have a transport for". None of the three was true. There was no
		// bin-state refresh; the loss was not expected by anyone who had
		// measured it — half of one plant's production counts; and there
		// were two transports, one of which the enclosing function is
		// already holding open.
		if _, err := s.binManifest.ClearForReuseTx(tx, d.BinID, nil,
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

// recordRejectedDelta makes a payload-mismatch drop visible: one
// bin_uop_audit observation row PER dropped delta (before == after, the
// dropped quantity in metadata — the same shape as OpStaleEpochDropped, so
// the discrepancy ledger can reconstruct the missing total for a later
// cycle count), plus the bin's anomaly flag, set once, so the bins page
// shows a bin whose counts are being refused.
//
// Runs on s.db, outside the caller's transaction — the reject path rolls
// that tx back deliberately (a rejected delta must not consume its dedup
// sequence). Best-effort: a failed observability write logs and never
// masks the reject itself. This path can never gate dispatch or the press:
// anomaly_at is a visibility timestamp, read by the bins page only — it
// feeds no claim predicate (BinUnavailableReason does not read it).
func (s *InventoryDeltaService) recordRejectedDelta(station string, d *protocol.BinUOPDelta, havePayloadCode string, valueBefore int, anomalyFlagged bool) {
	metadata, err := json.Marshal(struct {
		WirePayload string `json:"wire_payload"`
		BinPayload  string `json:"bin_payload"`
		SequenceID  int64  `json:"sequence_id"`
		Delta       int    `json:"delta"`
	}{d.PayloadCode, havePayloadCode, d.SequenceID, d.Delta})
	if err != nil {
		log.Printf("marshal rejected-delta audit metadata bin=%d: %v", d.BinID, err)
		return
	}
	if err := audit.AppendBinUOPOverride(s.db.DB, d.BinID, valueBefore, valueBefore,
		audit.OpPayloadMismatchDropped, "service/inventory_delta_service.go:payloadMismatch",
		nil, d.PayloadCode, station, metadata); err != nil {
		log.Printf("audit rejected delta bin=%d: %v", d.BinID, err)
	}
	if !anomalyFlagged {
		if err := s.db.MarkBinAnomaly(d.BinID); err != nil {
			log.Printf("mark anomaly for rejected delta bin=%d: %v", d.BinID, err)
		}
	}
	atomic.AddInt64(&s.droppedPayloadMismatch, 1)
}

// DroppedDeltaCounts returns the process-lifetime dropped-delta tallies split by
// reason (P2-C6). Counters only — reading them changes nothing. Payload-mismatch
// is the rate worth alarming on (a bin whose label was reassigned underneath an
// in-flight delta); stale-epoch churn is expected noise after a Core-side reset.
func (s *InventoryDeltaService) DroppedDeltaCounts() (staleEpoch, payloadMismatch int64) {
	return atomic.LoadInt64(&s.droppedStaleEpoch), atomic.LoadInt64(&s.droppedPayloadMismatch)
}

// AnomalyDeltaSummary is the read-only rollup behind the inventory page's
// "N rejected deltas · N stale staged bins" banner line (P2-C6). All four
// fields are pure observability: the drop counters are process-lifetime tallies,
// the two bin counts are live queries.
type AnomalyDeltaSummary struct {
	DroppedStaleEpoch      int64 `json:"dropped_stale_epoch"`
	DroppedPayloadMismatch int64 `json:"dropped_payload_mismatch"`
	// RejectedDeltaBins is the count of non-retired bins flagged anomaly_at —
	// carriers whose deltas are being refused (payload mismatch or stale epoch).
	RejectedDeltaBins int `json:"rejected_delta_bins"`
	// StaleStagedBins is the count of bins parked `staged` past their OWN
	// staging TTL (staged_expires_at < NOW). Uses the bin's configured expiry,
	// not a fixed age threshold; nil-TTL (permanent) staged bins are excluded.
	StaleStagedBins int `json:"stale_staged_bins"`
}

// AnomalySummary computes the read-only anomaly rollup for the inventory page.
// Never mutates; safe to call on every poll.
func (s *InventoryDeltaService) AnomalySummary() (AnomalyDeltaSummary, error) {
	out := AnomalyDeltaSummary{
		DroppedStaleEpoch:      atomic.LoadInt64(&s.droppedStaleEpoch),
		DroppedPayloadMismatch: atomic.LoadInt64(&s.droppedPayloadMismatch),
	}
	if err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM bins WHERE anomaly_at IS NOT NULL AND status != 'retired'),
		(SELECT COUNT(*) FROM bins WHERE status='staged' AND staged_expires_at IS NOT NULL AND staged_expires_at < NOW())
	`).Scan(&out.RejectedDeltaBins, &out.StaleStagedBins); err != nil {
		return out, fmt.Errorf("anomaly summary counts: %w", err)
	}
	return out, nil
}

// RejectedDeltaBin names ONE carrier whose deltas are being refused, for the
// inventory page's drill-down behind the "N rejected deltas" banner. It answers
// the operator's "which part / which carrier / why" — the summary only gives a
// count.
type RejectedDeltaBin struct {
	BinID       int64      `json:"bin_id"`
	BinLabel    string     `json:"bin_label"`
	NodeName    string     `json:"node_name"`
	PayloadCode string     `json:"payload_code"`
	AnomalyAt   time.Time  `json:"anomaly_at"`
	Reason      string     `json:"reason"`         // stale_epoch_dropped | payload_mismatch_dropped | "" if no audit row
	LastReject  *time.Time `json:"last_reject_at"` // most recent drop of either reason
	DropCount   int        `json:"drop_count"`     // total drops recorded for this bin
}

// RejectedDeltaDetail lists every non-retired bin flagged anomaly_at — the
// carriers whose BinUOPDeltas the applier is dropping (stale epoch or payload
// mismatch) — with the node, payload/part, when it was flagged, the latest drop
// reason + time, and how many drops it has logged. Pure read; ordered newest
// flag first. This is the click target behind the summary count so the operator
// can see WHICH carrier to cycle-count instead of just a number.
func (s *InventoryDeltaService) RejectedDeltaDetail() ([]RejectedDeltaBin, error) {
	rows, err := s.db.Query(`SELECT b.id, COALESCE(b.label,''), COALESCE(n.name,''),
		COALESCE(b.payload_code,''), b.anomaly_at,
		(SELECT a.op FROM bin_uop_audit a
		   WHERE a.bin_id=b.id AND a.op IN ('stale_epoch_dropped','payload_mismatch_dropped')
		   ORDER BY a.applied_at DESC LIMIT 1),
		(SELECT MAX(a.applied_at) FROM bin_uop_audit a
		   WHERE a.bin_id=b.id AND a.op IN ('stale_epoch_dropped','payload_mismatch_dropped')),
		(SELECT COUNT(*) FROM bin_uop_audit a
		   WHERE a.bin_id=b.id AND a.op IN ('stale_epoch_dropped','payload_mismatch_dropped'))
		FROM bins b
		LEFT JOIN nodes n ON n.id=b.node_id
		WHERE b.anomaly_at IS NOT NULL AND b.status != 'retired'
		ORDER BY b.anomaly_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("rejected-delta detail: %w", err)
	}
	defer rows.Close()
	var out []RejectedDeltaBin
	for rows.Next() {
		var b RejectedDeltaBin
		var reason sql.NullString
		var last sql.NullTime
		if err := rows.Scan(&b.BinID, &b.BinLabel, &b.NodeName, &b.PayloadCode,
			&b.AnomalyAt, &reason, &last, &b.DropCount); err != nil {
			return nil, fmt.Errorf("scan rejected-delta row: %w", err)
		}
		b.Reason = reason.String
		if last.Valid {
			b.LastReject = &last.Time
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ApplyLinesideBucketDelta applies a LinesideBucketDelta against the
// lineside_buckets row keyed on (core_node_name, pair_key, style_id,
// part_number). Creates the row on first sight via UPSERT; deletes when
// qty reaches zero (Option C — empty buckets carry no useful information).
//
// TWO USES OF station IN ONE FUNCTION, AND THEY ARE NOT THE SAME KIND OF
// THING — this is the distinction v65 turns on:
//
//   - claimDeltaSequence(tx, station, ...) — station STAYS. SequenceID is an
//     Edge-local counter, so "which edge's counter space" is exactly what
//     makes the at-most-once guard correct. Per-edge identity FIXES this one:
//     two edges sharing a station id today share a sequence space they are
//     each numbering independently.
//   - The lineside_buckets row itself — station is NOT a predicate. The row is
//     a physical fact about a Core node, and the reporting edge is not part of
//     where the parts are.
//
// A change that treated both the same way would get one of them wrong.
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
func (s *InventoryDeltaService) ApplyLinesideBucketDelta(station string, d *protocol.LinesideBucketDelta) error {
	if d == nil {
		return fmt.Errorf("nil LinesideBucketDelta")
	}
	if station == "" {
		return fmt.Errorf("LinesideBucketDelta missing station")
	}
	if d.CoreNodeName == "" {
		return fmt.Errorf("LinesideBucketDelta missing core_node_name (station=%s style=%d part=%q)",
			station, d.StyleID, d.PartNumber)
	}
	if d.PartNumber == "" {
		return fmt.Errorf("LinesideBucketDelta missing part_number (station=%s core_node_name=%s style=%d)",
			station, d.CoreNodeName, d.StyleID)
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
			d.CoreNodeName, station, d.PartNumber, err)
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
	applied, err := claimDeltaSequence(tx, station, invDeltaScopeBucket, scopeKey, 0, d.SequenceID)
	if err != nil {
		return err
	}
	if !applied {
		return ErrInventoryDeltaSkipped
	}

	// A reduction (negative delta) only makes sense against an existing bucket.
	// On the first-sight INSERT path GREATEST(delta,0) clamps it to a 0 row,
	// silently dropping the reduction (R22-1). Surface that case as an error —
	// symmetric with the existing-row underflow the CHECK below rejects — so the
	// count can't quietly drift up.
	if d.Delta < 0 {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM lineside_buckets
			WHERE core_node_name=$1 AND pair_key=$2 AND style_id=$3 AND part_number=$4)`,
			d.CoreNodeName, d.PairKey, d.StyleID, d.PartNumber).Scan(&exists); err != nil {
			return fmt.Errorf("check bucket exists for negative LinesideBucketDelta (core_node_name=%q part=%q): %w",
				d.CoreNodeName, d.PartNumber, err)
		}
		if !exists {
			return fmt.Errorf("LinesideBucketDelta reduction of %d for non-existent bucket (core_node_name=%q part=%q)",
				d.Delta, d.CoreNodeName, d.PartNumber)
		}
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
	//
	// STATION IS WRITTEN, NEVER MATCHED ON (v65). The conflict target is the
	// physical bucket — node, pair, style, part — and the station rides along
	// as "who last reported this". Matching on it would mean a bucket reported
	// by a second edge inserts a SECOND row for one physical place, which the
	// station-blind SUM in SystemUOPForPayload would then count twice.
	res, err := tx.Exec(`
		INSERT INTO lineside_buckets (station, core_node_name, pair_key, style_id, part_number, qty, payload_code)
		VALUES ($1, $2, $3, $4, $5, GREATEST($6, 0), $7)
		ON CONFLICT (core_node_name, pair_key, style_id, part_number)
		DO UPDATE SET
			qty = lineside_buckets.qty + $6,
			payload_code = CASE WHEN $7 = '' THEN lineside_buckets.payload_code ELSE $7 END,
			station = $1,
			updated_at = NOW()`,
		station, d.CoreNodeName, d.PairKey, d.StyleID, d.PartNumber, d.Delta, d.PayloadCode)
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
	// Station-free, matching the conflict target above. A station-scoped GC is
	// how an emptied bucket survives its own emptying once two edges exist:
	// the edge that zeroed it is not the edge whose station is on the row, so
	// the DELETE matches nothing and a qty=0 row lingers as an orphan.
	if _, err := tx.Exec(`DELETE FROM lineside_buckets
		WHERE core_node_name=$1 AND pair_key=$2 AND style_id=$3 AND part_number=$4
		AND qty=0`,
		d.CoreNodeName, d.PairKey, d.StyleID, d.PartNumber); err != nil {
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

// ListBucketsForNodes returns every authoritative bucket row at the given
// Core nodes. This is the drift reconciler's read.
//
// IT USED TO FILTER ON `station`, AND THAT WAS THE SIXTH STATION-KEYED SITE.
// After v65 `station` on a bucket row is the LAST REPORTER, not an ownership
// claim, so the old filter answered "buckets some edge most recently
// mentioned" while the caller was asking "buckets at the nodes I own". Those
// coincide only while every edge shares one station string — which is the
// condition the identity change ends. With distinct ids, a bucket at one of
// edge A's nodes that edge B last touched becomes invisible to A's
// reconciliation: the drift detector stops seeing exactly the drift it exists
// for, and silently, because an empty result and a clean result look the same.
//
// IT WAS DEFERRED ON THE GROUNDS THAT FIXING IT WOULD CHANGE WHAT EDGE SENDS.
// It does not. Edge already sends the node set on this same request —
// CoreClient.FetchUOPState puts BOTH `station=` and `nodes=` on the query
// string, and its only caller (Engine.BucketBackfillNeeded) builds `nodes`
// from ListProcessNodes()'s core_node_name, which is the literal definition of
// "the nodes I own". Both halves of the answer were already on the wire; only
// the server was reading the wrong one. So this is a server-side correction
// with no protocol change and no Edge deploy ordering attached to it.
//
// The shape was already sitting next to it: ListBinUOPForNodes takes node
// names, and the SAME handler builds that list for the bins half of the same
// response.
func (s *InventoryDeltaService) ListBucketsForNodes(names []string) ([]LinesideBucketRow, error) {
	if len(names) == 0 {
		return nil, nil
	}
	// Explicit placeholders rather than pq.Array, matching ListBinUOPForNodes
	// directly above: this package takes a bare *sql.DB and does not import the
	// driver package, and the two halves of one response should not disagree
	// about how a node list is bound.
	args := make([]any, len(names))
	placeholders := make([]string, len(names))
	for i, name := range names {
		args[i] = name
		placeholders[i] = "$" + strconv.Itoa(i+1)
	}
	rows, err := s.db.Query(`SELECT b.core_node_name, b.pair_key, b.style_id, b.part_number, b.qty
		FROM lineside_buckets b
		WHERE b.core_node_name IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY b.core_node_name, b.part_number`, args...)
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
