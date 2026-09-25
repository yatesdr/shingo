package uop

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"

	"shingocore/store"
	"shingocore/store/audit"
	"shingocore/store/bins"
	"shingocore/store/messaging"
)

// Core-side count apply service. Receives BinUOPDelta and
// LinesideBucketLevel envelopes from Edge and guards order against
// inventory_delta_dedup. A bin delta is validated against the bin row and
// applies to bins.uop_remaining what the message's running net says has not
// landed yet (effectiveDelta). A bucket level sets Core's mirror row of one
// lineside pile to the Edge's level.
//
// Dedup scope keys (stable; renames break in-flight Edge replays):
//
//   - bin scope:          strconv(BinID)
//   - bucket_level scope: "<CoreNodeName>|<PayloadCode>|<State>"
//
// Either-order arrival tolerance: capture-on-release fires both a bin
// delta and one bucket level per part, atomically on Edge's outbox tx.
// Core's handler ordering is independent — the dedup table guards each
// scope independently, so a level arriving before its sibling bin delta
// still applies cleanly.

const (
	// invDeltaScopeBin / invDeltaScopeBucketLevel — scope_kind values for the
	// inventory_delta_dedup table.
	//
	// NOT a Core-internal partition, which is what this comment used to claim.
	// Edge writes scope_kind when it allocates a sequence-id and Core dedups on
	// the value it receives, so the two sides must agree; a rename on one side
	// alone stops deduplication silently. Single-sourced in protocol/ for that
	// reason.
	invDeltaScopeBin         = protocol.InvDeltaScopeBin
	invDeltaScopeBucketLevel = protocol.InvDeltaScopeBucketLevel
)

// ErrInventoryDeltaSkipped indicates the message was not applied and that this
// is not a failure: it was at or below its scope's high-water mark (a
// duplicate, a late or requeued message, or a station whose counter went
// backward), or it carried a retired generation. The applier wraps it with
// which of those it was, for the log. Callers treat it as a successful skip —
// not an error to propagate to a 4xx/5xx response.
var ErrInventoryDeltaSkipped = errors.New("inventory delta skipped")

// ManifestClearer is the narrow interface InventoryDeltaService uses
// to fire ClearForReuse atomically inside the delta-apply transaction
// when a capture_reduction drives uop_remaining to zero, and to start a new
// generation when a station's count stream went backward. The signature
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
	ClearForReuseTx(tx *sql.Tx, binID int64, binTypeID *int64, op, source string, by protocol.Declarer) (int64, error)

	// RebaseAfterEdgeRollbackTx bumps the bin's generation, count unchanged,
	// when the station counting it went backward (see atOrBelowHighWater).
	// The bump announces Core's number, which the station adopts.
	RebaseAfterEdgeRollbackTx(tx *sql.Tx, binID int64, payloadCode, station string) (int64, error)
}

// InventoryDeltaService applies BinUOPDelta envelopes against the
// authoritative bins table and LinesideBucketLevel envelopes against
// Core's lineside_buckets mirror, ordered by inventory_delta_dedup.
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
	// reply for that carrier carried, and when it was queued. In memory and
	// deliberately not persisted — a Core restart may re-send one reply per
	// carrier, which is cheap, and the alternative is a table whose only job is
	// to suppress a message that costs nothing to repeat.
	//
	// One reply per generation per repairWindow. The reply is fire-and-forget:
	// it can be lost in transit, and the station may be an older build that
	// does not know the message at all. Either way the discarded counts keep
	// arriving — 3,200 in a day at one plant — and a reply per discarded count
	// would be a flood aimed at something that is not listening. So a reply
	// holds back the next one for the same (bin, generation) for the window,
	// and a lost reply is re-sent on the first discard after it. A new
	// generation is answered at once.
	//
	// Bounded: markRepaired deletes every entry older than the window each time
	// it writes, so the map holds at most the carriers answered in the last
	// window, plus one.
	//
	// NOT keyed off bins.anomaly_at. That column is a latch: it is set on the
	// first drop and stays set, so using it as the gate would suppress the
	// repair forever after the first one.
	repairedMu sync.Mutex
	repaired   map[int64]repairedReply
}

// repairWindow is how long a reply for one (bin, generation) holds back the
// next. At a plant's tick rate a station that ignored or lost a reply is told
// again within a minute; one that is not listening costs one message a minute
// per carrier.
const repairWindow = 60 * time.Second

// repairedReply is one debounce entry: the generation replied with, and when.
type repairedReply struct {
	epoch int64
	at    time.Time
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
		repaired:    make(map[int64]repairedReply),
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
// generation went out within repairWindow — see the repaired map's comment.
func (s *InventoryDeltaService) alreadyRepaired(binID, epoch int64) bool {
	s.repairedMu.Lock()
	defer s.repairedMu.Unlock()
	r, ok := s.repaired[binID]
	return ok && r.epoch == epoch && clock.Now().Sub(r.at) < repairWindow
}

// markRepaired records a queued reply, and drops every entry whose window has
// passed. Called after the commit, so a transaction that rolls back does not
// suppress the next attempt.
func (s *InventoryDeltaService) markRepaired(binID, epoch int64) {
	s.repairedMu.Lock()
	defer s.repairedMu.Unlock()
	now := clock.Now()
	if s.repaired == nil {
		s.repaired = make(map[int64]repairedReply)
	}
	for id, r := range s.repaired {
		if now.Sub(r.at) >= repairWindow {
			delete(s.repaired, id)
		}
	}
	s.repaired[binID] = repairedReply{epoch: epoch, at: now}
}

// ApplyBinUOPDelta applies a BinUOPDelta against bins.uop_remaining.
// Every station writes the authoritative column directly — there is
// no per-station routing or staging table.
//
// Returns ErrInventoryDeltaSkipped (wrapped, saying why) when the message is at
// or below its scope's last applied seq, or carries a retired generation.
// Returns a wrapped error when the bin doesn't exist or the payload code
// mismatches. A refused message consumes neither its seq nor its net, so
// with the running net its parts land with the next message that is
// accepted; without it (an older Edge) they are lost, and the refusal row is
// the only record. No reconciler exists to catch it.
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

	// ONE READ, BEFORE ANY GUARD: the bin's generation, and Core's cursor in
	// this message's OWN scope — (station, bin, wire epoch). The bin row is
	// locked here, which every apply path took anyway at its UPDATE, so no two
	// applies for this bin interleave between this read and the claim below
	// and the cursor is exact. Folded into the statement that already read the
	// epoch: no statement added.
	currentEpoch, cur, err := readBinScope(tx, station, scopeKey, d)
	if err != nil {
		return err
	}

	// At or below the high-water mark: this scope has already applied this seq
	// or a later one. Judged HERE, before the stale-epoch guard, because a
	// redelivered message is a duplicate whatever has happened to the bin
	// since, and redelivery is expected once Core commits after handling. A
	// capture_reduction that emptied the bin bumped its generation on the way;
	// its redelivery carries the old one, and checking the epoch first
	// recorded it as a stale drop — a permanent exception, the anomaly flag
	// and an epoch refresh for a count that had already landed.
	if cur.found && d.SequenceID <= cur.lastSeq {
		if done, err := s.atOrBelowHighWater(tx, station, d, currentEpoch, cur); done {
			return err
		}
	}

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
		// The permanent exceptions ledger (v93) — this drop is one of the
		// four durable kinds. Same transaction, same metadata blob.
		if err := audit.AppendBinUOPException(tx, audit.ExcStaleEpoch, d.BinID,
			d.PayloadCode, station, nil, clock.Now().UTC(), &before, &before, nil,
			nil, audit.OpStaleEpochDropped, metadata); err != nil {
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
		return fmt.Errorf("%w: generation %d is retired (bin at %d); recorded as a stale drop",
			ErrInventoryDeltaSkipped, d.Epoch, currentEpoch)
	case d.Epoch > currentEpoch:
		// Edge ahead of Core is a real anomaly — Core controls epoch via
		// lifecycle handlers, so Edge shouldn't see a higher value before
		// Core writes it. Possible cause: a stale Core read or a corrupt
		// Edge cache; log but still apply since the new epoch isn't worse
		// than continuing.
		log.Printf("WARN: BinUOPDelta future epoch bin=%d wire_epoch=%d bin_epoch=%d seq=%d — Edge ahead of Core",
			d.BinID, d.Epoch, currentEpoch, d.SequenceID)
	}

	// THE APPLY RULE (SYNTH-round2 §3). The message carries its scope's running
	// net; Core applies what it has not applied yet. A message lost, reordered,
	// requeued or muted above is carried by the next one that lands here.
	eff := effectiveDelta(cur, d.Delta, d.Net)
	applied, err := claimDeltaSequence(tx, station, invDeltaScopeBin, scopeKey, d.Epoch, d.SequenceID, d.Net, d.WindowEnd)
	if err != nil {
		return err
	}
	if !applied {
		// The bin lock makes this unreachable for one station; kept so a
		// broken invariant skips rather than double-applies.
		return fmt.Errorf("%w: seq %d did not advance its scope past last_seq", ErrInventoryDeltaSkipped, d.SequenceID)
	}

	// Validate target bin and (optionally) payload code. payload_code
	// mismatch indicates the bin's payload was reassigned underneath
	// us — the in-flight delta no longer corresponds to the count
	// it was attributing change to. Reject loudly.
	var (
		havePayloadCode string
		valueBefore     int
		anomalyFlagged  bool
		binNodeID       sql.NullInt64
	)
	err = tx.QueryRow(`SELECT payload_code, uop_remaining, anomaly_at IS NOT NULL, node_id FROM bins WHERE id=$1`,
		d.BinID).Scan(&havePayloadCode, &valueBefore, &anomalyFlagged, &binNodeID)
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
	// count stream to fence off, and a bump would retire a generation that is
	// still running — the station's next count would arrive stamped with it and
	// be discarded (observed live 2026-07-16 14:02Z). The drop window that
	// followed such a bump is now answered rather than merely opened
	// (repairEpoch), but the reason not to bump here is unchanged and is the
	// stronger one: nothing about this path starts the carrier a new life.
	// Non-produce reasons keep the hard reject below — a consume drain
	// against a wrong-labeled bin is the ALN_001 corruption case.
	if d.Reason == protocol.ReasonProduceTick && d.PayloadCode != "" &&
		havePayloadCode != d.PayloadCode {
		if valueBefore == 0 {
			if _, err := tx.Exec(`UPDATE bins b SET payload_code=$1,
				undeclared_carrier_at = `+undeclaredCarrierStampSQL+`
				WHERE b.id=$2`, d.PayloadCode, d.BinID); err != nil {
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
			if _, err := tx.Exec(`UPDATE bins b SET payload_code=$1,
				anomaly_at=COALESCE(b.anomaly_at, NOW()),
				undeclared_carrier_at = `+undeclaredCarrierStampSQL+`
				WHERE b.id=$2`,
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
		// writes go through s.db, NOT this tx, and the tx is rolled back
		// FIRST: it holds the bins-row lock readBinScope took, which the
		// anomaly UPDATE on s.db would wait behind. The rollback also leaves
		// the dedup seq and applied_net unconsumed, so the refused count is
		// held in the scope's net and lands with the next accepted message.
		_ = tx.Rollback()
		s.recordRejectedDelta(station, d, havePayloadCode, valueBefore, anomalyFlagged)
		return fmt.Errorf("BinUOPDelta payload mismatch bin=%d wire=%q have=%q",
			d.BinID, d.PayloadCode, havePayloadCode)
	}

	if _, err := tx.Exec(`UPDATE bins SET uop_remaining = uop_remaining + $1
		WHERE id=$2`, eff, d.BinID); err != nil {
		return fmt.Errorf("apply BinUOPDelta bin=%d delta=%d: %w", d.BinID, eff, err)
	}

	// Audit metadata via json.Marshal — Item 14 cleanup (D7). The
	// previous fmt.Sprintf approach broke when the reason string
	// carried a quote character (the format-string-as-JSON-template
	// approach has no escaping). Typed marshal handles every JSON
	// edge case correctly and matches the pattern in
	// bin_manifest.AuditReleaseOverride.
	// wire_epoch and bin_epoch use the SAME JSON keys the stale-epoch drop
	// branch above writes, so one query answers the question across both
	// outcomes.
	//
	// Until 2026-08-22 the epoch was recorded ONLY when a delta was dropped, so
	// a delta that ARRIVED on a stale generation and was applied left no trace
	// of which generation it carried — and "did a late delta ever land on the
	// wrong generation" was unanswerable from the ledger by construction. That
	// matters more since bin_uop_delta stopped expiring: a delta can now arrive
	// arbitrarily late, and the epoch is the only thing that says whether that
	// was harmless.
	//
	// delta IS WHAT WAS APPLIED (the running net's effective delta), because
	// every reader that sums this key — the daily roll-up above all — must sum
	// what moved the count. wire_delta is what the message said; healed is the
	// difference, the parts earlier messages of the scope carried that never
	// landed on their own. net is absent on a message from an Edge without it.
	// The seq stays under sequence_id, the key the drop rows share.
	metadata, err := json.Marshal(struct {
		Reason      string    `json:"reason"`
		Delta       int       `json:"delta"`
		WireDelta   int       `json:"wire_delta"`
		Healed      int       `json:"healed"`
		Net         *int64    `json:"net,omitempty"`
		SequenceID  int64     `json:"sequence_id"`
		WireEpoch   int64     `json:"wire_epoch"`
		BinEpoch    int64     `json:"bin_epoch"`
		WindowStart time.Time `json:"window_start"`
		WindowEnd   time.Time `json:"window_end"`
	}{
		Reason:      string(d.Reason),
		Delta:       eff,
		WireDelta:   d.Delta,
		Healed:      eff - d.Delta,
		Net:         d.Net,
		SequenceID:  d.SequenceID,
		WireEpoch:   d.Epoch,
		BinEpoch:    currentEpoch,
		WindowStart: d.WindowStart,
		WindowEnd:   d.WindowEnd,
	})
	if err != nil {
		return fmt.Errorf("marshal BinUOPDelta audit metadata bin=%d: %w", d.BinID, err)
	}
	// node_id is the bin's node as of this delta, taken from the bins row this
	// tx already read — the grain a per-node consumption rate needs, and the
	// only chance to record it: a later join reports where the bin is NOW, not
	// where it was when the tick landed. A carrier standing nowhere writes
	// NULL, which is the honest value rather than a gap; there is no node to
	// name, and its last or next one would put a place on a count that did not
	// happen there.
	// reason is a real column since v119 so the consumption rate can filter on
	// it without a JSON path no index covers. metadata keeps the same key — it
	// is the audit record; the column is what a WHERE clause sees.
	if _, err := tx.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, node_id, reason)
		VALUES ($1, $2, $3, 'bin_uop_delta', 'service/inventory_delta_service.go', $4, $5, $6, $7, $8)`,
		d.BinID, valueBefore, valueBefore+eff,
		d.PayloadCode, station, string(metadata), binNodeID, string(d.Reason),
	); err != nil {
		return fmt.Errorf("audit BinUOPDelta bin=%d: %w", d.BinID, err)
	}

	// The permanent exceptions ledger (v93): a crossing (>= 0 to < 0) opens a
	// negative_crossing row; a recovery (< 0 back to >= 0) closes the open
	// one and folds its deepest. Continuations (still negative) write
	// nothing — they are the same excursion. This is the event-time half of
	// what the backfill derived; after the 90-day retention lands, these rows
	// are the only durable record of the negatives.
	newValue := valueBefore + eff
	switch {
	case valueBefore >= 0 && newValue < 0:
		if err := audit.AppendBinUOPException(tx, audit.ExcNegativeCrossing, d.BinID,
			d.PayloadCode, station, nil, clock.Now().UTC(), &valueBefore, &newValue, nil,
			nil, "bin_uop_delta", metadata); err != nil {
			return err
		}
	case valueBefore < 0 && newValue >= 0:
		if err := audit.RecoverBinUOPOpenCrossing(tx, d.BinID, clock.Now().UTC()); err != nil {
			return err
		}
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
	// go through ClearForReuse directly. Idempotent because the
	// high-water check at the top already turned away a redelivery.
	if d.Reason == protocol.ReasonCaptureReduction && newValue <= 0 && s.binManifest != nil {
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
		// A delta stream reaching zero clears the carrier. Nobody declared
		// anything — the applier concluded it — so the station must not bind an
		// empty slot to it.
		if _, err := s.binManifest.ClearForReuseTx(tx, d.BinID, nil,
			audit.OpReleasedCaptureEmpty,
			"service/inventory_delta_service.go:ApplyBinUOPDelta",
			protocol.DeclaredByLifecycle); err != nil {
			return fmt.Errorf("clear manifest on capture_reduction zero bin=%d: %w", d.BinID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit BinUOPDelta bin=%d: %w", d.BinID, err)
	}
	return nil
}

// recordRejectedDelta makes a payload-mismatch drop visible: one
// bin_uop_ledger observation row PER dropped delta (before == after, the
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
	// The permanent exceptions ledger (v93), same best-effort contract as the
	// row above: on s.db outside the rolled-back tx, and a failed write logs
	// rather than masks the reject.
	if err := audit.AppendBinUOPException(s.db.DB, audit.ExcPayloadMismatch, d.BinID,
		d.PayloadCode, station, nil, clock.Now().UTC(), &valueBefore, &valueBefore, nil,
		nil, audit.OpPayloadMismatchDropped, metadata); err != nil {
		log.Printf("audit rejected-delta exception bin=%d: %v", d.BinID, err)
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

// undeclaredCarrierStampSQL keeps bins.undeclared_carrier_at true to the
// carrier rule on the two produce-tick payload writes below.
//
// A PRODUCE-TICK REBIND IS A PAYLOAD WRITE, and the finding is derived from the
// payload. Without this the identity binding could move a carrier onto a part
// its type is not declared to hold and leave the bin unflagged until its next
// finalize — a window in which every sourcing reader refuses the bin and no
// surface says why. It rides the UPDATE that is already here: zero added
// statements on the delta path, which is the only reason it is affordable at
// all (this runs under a produce tick).
//
// COALESCE, so a carrier already flagged keeps its original stamp, and the ELSE
// arm clears the flag when a rebind moves the carrier onto something it may
// hold. Judged against the NEW payload ($1) and the bin's unchanged type.
var undeclaredCarrierStampSQL = `CASE WHEN ` +
	bins.UndeclaredCarrierRuleSQL("$1", "b.bin_type_id") +
	` THEN COALESCE(b.undeclared_carrier_at, NOW()) ELSE NULL END`

// AnomalyDeltaSummary is the read-only rollup behind the inventory page's
// "N rejected deltas · N stale staged bins" banner line (P2-C6). Every field is
// pure observability: the drop counters are process-lifetime tallies, the three
// bin counts are live queries.
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
	// UndeclaredCarrierBins is the count of carriers holding a payload their
	// type is not declared to carry — findings the produce door recorded rather
	// than refusing, because the parts were already in the bin.
	//
	// THIS IS THE WATCHER FOR A RULE THAT NO LONGER REFUSES. Every one of these
	// bins counts as stock and no sourcing reader will ever fetch it, so an
	// unwatched flag would be the silent hole the refusal used to close. The
	// same stamp is listed per carrier on /material-flags and named in the
	// sourcing reason; this is the count, and it composes the one fragment all
	// three share so they cannot disagree.
	//
	// NOT scoped to non-retired, unlike RejectedDeltaBins. A retired carrier
	// carrying a payload is itself a finding, and the flag is cleared by the
	// clear that retirement ought to involve.
	UndeclaredCarrierBins int `json:"undeclared_carrier_bins"`
}

// AnomalySummary computes the read-only anomaly rollup for the inventory page.
// Never mutates; safe to call on every poll.
func (s *InventoryDeltaService) AnomalySummary() (AnomalyDeltaSummary, error) {
	out := AnomalyDeltaSummary{
		DroppedStaleEpoch:      atomic.LoadInt64(&s.droppedStaleEpoch),
		DroppedPayloadMismatch: atomic.LoadInt64(&s.droppedPayloadMismatch),
	}
	// staged_expires_at is written from a Go value on the injected clock, so it is
	// compared against that clock and not the database's NOW() (§R.98 stage D).
	// The sweep that acts on this column (bins.ReleaseExpiredStaged) already does;
	// this page did not, so the two could tell an operator opposite things about
	// the same bin the moment the domains diverge.
	//
	// The undeclared-carrier count is a THIRD SUBQUERY IN THE SAME ROUND TRIP,
	// not a second call: this endpoint is polled by the inventory page and a
	// watcher that costs a query per refresh is a watcher somebody eventually
	// turns off.
	if err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM bins WHERE anomaly_at IS NOT NULL AND status != 'retired'),
		(SELECT COUNT(*) FROM bins WHERE status='staged' AND staged_expires_at IS NOT NULL AND staged_expires_at < $1::timestamptz),
		(SELECT COUNT(*) FROM bins b WHERE `+bins.BinInUndeclaredCarrierSQL+`)
	`, clock.Now().UTC()).Scan(&out.RejectedDeltaBins, &out.StaleStagedBins, &out.UndeclaredCarrierBins); err != nil {
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
		(SELECT a.op FROM bin_uop_ledger a
		   WHERE a.bin_id=b.id AND a.op IN ('stale_epoch_dropped','payload_mismatch_dropped')
		   ORDER BY a.applied_at DESC LIMIT 1),
		(SELECT MAX(a.applied_at) FROM bin_uop_ledger a
		   WHERE a.bin_id=b.id AND a.op IN ('stale_epoch_dropped','payload_mismatch_dropped')),
		(SELECT COUNT(*) FROM bin_uop_ledger a
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

// ApplyLinesideBucketLevel sets Core's mirror of one lineside pile row to the
// level the Edge sent: the row (core_node_name, payload_code, state) after the
// change, with a Qty of 0 meaning the row is gone.
//
// THE EDGE IS THE PILE'S ONLY WRITER. Core stores the level under a seq guard
// and never writes a pile of its own, so there is nothing to refuse and
// nothing to orphan: a lost or reordered level is replaced by the row's next
// one, and the Edge re-sends every row's level at boot. On-hand counts the
// active rows only (SystemUOPForPayload); a stranded row is a count-anomaly
// record from a cutover.
//
// TWO USES OF station, AND THEY ARE NOT THE SAME KIND OF THING (v65):
//
//   - The dedup row — station STAYS. SequenceID is an Edge-local counter, so
//     "which edge's counter space" is what makes the order guard correct.
//   - The lineside_buckets row — station is written, never matched on. The row
//     is a physical fact about a Core node; the reporting edge is not part of
//     where the parts are.
//
// ORDER. A level at or below the row's high-water seq is skipped (a duplicate
// or a late message; the level that passed it is newer). The exception is a
// seq that went backward with a LATER window end: a restored Edge numbering
// new levels with old seqs. That level is applied, and the dedup row is
// re-anchored to its seq and window so the restored Edge's next levels apply.
//
// THE DRAIN LEDGER. Drained is the sum of the consume drains the row took in
// the Edge's flush window. When it is positive on an active row, one
// lineside_drain_ledger row records it (before = Qty + Drained, after = Qty):
// the consumption rate's drain arm. A pull and a strand are not consumption,
// and the Edge sends them with Drained 0.
//
// Round-3 Obs 8: the core node name must resolve to a Core node, or the level
// is refused with a loud error, so bad data never enters the table.
//
// Returns ErrInventoryDeltaSkipped (wrapped) for a level at or below its row's
// high-water seq that is not a restored Edge.
func (s *InventoryDeltaService) ApplyLinesideBucketLevel(station string, l *protocol.LinesideBucketLevel) error {
	nodeID, err := s.validateBucketLevel(station, l)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	scopeKey := bucketLevelScopeKey(l.CoreNodeName, l.PayloadCode, l.State)
	applied, cur, err := claimBucketSequence(tx, station, scopeKey, l.SequenceID, l.WindowEnd)
	if err != nil {
		return err
	}
	if !applied {
		if !cur.wentBackward(l.SequenceID, l.WindowEnd) {
			return fmt.Errorf("%w: seq %d at or below last_seq %d: a duplicate or a late level",
				ErrInventoryDeltaSkipped, l.SequenceID, cur.lastSeq)
		}
		if err := reanchorBucketSequence(tx, station, scopeKey, l.SequenceID, l.WindowEnd); err != nil {
			return err
		}
		log.Printf("LinesideBucketLevel EDGE RESTORED station=%s node=%s payload=%s state=%s seq=%d last_seq=%d "+
			"window_end=%s applied_window_end=%s — the station's counter went backward; level applied, seq re-anchored",
			station, l.CoreNodeName, l.PayloadCode, l.State, l.SequenceID, cur.lastSeq,
			l.WindowEnd.Format(time.RFC3339Nano), cur.appliedWindowEnd.Time.Format(time.RFC3339Nano))
	}

	if err := setBucketLevel(tx, station, l); err != nil {
		return err
	}

	if l.Drained > 0 && l.State == protocol.LinesideBucketActive {
		if _, err := tx.Exec(`INSERT INTO lineside_drain_ledger (node_id, payload_code, before_qty, after_qty)
			VALUES ($1, $2, $3, $4)`,
			nodeID, l.PayloadCode, l.Qty+l.Drained, l.Qty); err != nil {
			return fmt.Errorf("audit lineside drain core_node_name=%q payload=%q: %w",
				l.CoreNodeName, l.PayloadCode, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit LinesideBucketLevel core_node_name=%q payload=%q state=%s: %w",
			l.CoreNodeName, l.PayloadCode, l.State, err)
	}
	return nil
}

// validateBucketLevel checks a level's fields and resolves its core node name
// to the node id the drain ledger records.
func (s *InventoryDeltaService) validateBucketLevel(station string, l *protocol.LinesideBucketLevel) (int64, error) {
	switch {
	case l == nil:
		return 0, fmt.Errorf("nil LinesideBucketLevel")
	case station == "":
		return 0, fmt.Errorf("LinesideBucketLevel missing station")
	case l.CoreNodeName == "":
		return 0, fmt.Errorf("LinesideBucketLevel missing core_node_name (station=%s payload=%q)", station, l.PayloadCode)
	case l.PayloadCode == "":
		return 0, fmt.Errorf("LinesideBucketLevel missing payload_code (station=%s core_node_name=%s)", station, l.CoreNodeName)
	case l.State != protocol.LinesideBucketActive && l.State != protocol.LinesideBucketStranded:
		return 0, fmt.Errorf("LinesideBucketLevel state %q is neither %q nor %q (station=%s core_node_name=%s payload=%q)",
			l.State, protocol.LinesideBucketActive, protocol.LinesideBucketStranded, station, l.CoreNodeName, l.PayloadCode)
	case l.Qty < 0 || l.Drained < 0:
		return 0, fmt.Errorf("LinesideBucketLevel qty %d / drained %d below zero (station=%s core_node_name=%s payload=%q)",
			l.Qty, l.Drained, station, l.CoreNodeName, l.PayloadCode)
	}
	node, err := s.db.GetNodeByName(l.CoreNodeName)
	if err != nil {
		return 0, fmt.Errorf("LinesideBucketLevel core_node_name=%q does not resolve to a Core node (station=%s payload=%q): %w",
			l.CoreNodeName, station, l.PayloadCode, err)
	}
	return node.ID, nil
}

// setBucketLevel writes one level in one statement: a positive Qty upserts the
// row to it, 0 deletes the row. The conflict target is the physical pile, never
// the station, which rides along as the last reporter.
func setBucketLevel(tx *sql.Tx, station string, l *protocol.LinesideBucketLevel) error {
	var err error
	if l.Qty > 0 {
		_, err = tx.Exec(`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (core_node_name, payload_code, state)
			DO UPDATE SET qty = EXCLUDED.qty, station = EXCLUDED.station, updated_at = NOW()`,
			station, l.CoreNodeName, l.PayloadCode, string(l.State), l.Qty)
	} else {
		_, err = tx.Exec(`DELETE FROM lineside_buckets
			WHERE core_node_name=$1 AND payload_code=$2 AND state=$3`,
			l.CoreNodeName, l.PayloadCode, string(l.State))
	}
	if err != nil {
		return fmt.Errorf("set lineside bucket level core_node_name=%q payload=%q state=%s qty=%d: %w",
			l.CoreNodeName, l.PayloadCode, l.State, l.Qty, err)
	}
	return nil
}

// scopeCursor is Core's position in one count-message scope before the
// message in hand: whether a dedup row exists, the highest seq applied, the
// running net that seq carried (NULL when the row has only ever applied
// messages without one), and the latest Edge-time window end applied.
type scopeCursor struct {
	found            bool
	lastSeq          int64
	appliedNet       sql.NullInt64
	appliedWindowEnd sql.NullTime
}

// effectiveDelta is the apply rule (SYNTH-round2 §3):
//
//	eff = row absent ? net : (applied_net IS NULL ? delta : net - applied_net)
//
// and delta when the message carries no net (an Edge built before it), which
// is exactly the old behaviour.
//
// The NULL arm is the mixed-version anchor. A row that exists has applied
// deltas from messages without a net, so the net of the first message that
// has one covers counts the row already holds; that message applies its own
// delta and the claim anchors applied_net to its net. An absent row has
// applied nothing, so the whole net is new.
func effectiveDelta(cur scopeCursor, wireDelta int, net *int64) int {
	switch {
	case net == nil:
		return wireDelta
	case !cur.found:
		return int(*net)
	case !cur.appliedNet.Valid:
		return wireDelta
	default:
		return int(*net - cur.appliedNet.Int64)
	}
}

// wentBackward reports the rollback shape (SYNTH-round2 S6): a message at or
// below the scope's last applied seq whose window ends AFTER every window
// applied. A duplicate carries the same window; a late or reordered message
// carries an earlier one. Only a station whose seq table went backward — a
// restore or a reinstall — numbers new counts with old seqs.
//
// The wire time is truncated to Postgres's microsecond before comparing, or a
// duplicate's nanoseconds would read as later than the stored copy of itself.
// A Pi clock stepped back by more than the backup's age hides a rollback (a
// miss, never a false alarm).
func (c scopeCursor) wentBackward(seq int64, windowEnd time.Time) bool {
	return c.found && seq <= c.lastSeq && c.appliedWindowEnd.Valid &&
		windowEnd.Truncate(time.Microsecond).After(c.appliedWindowEnd.Time)
}

// readBinScope reads the bin's current generation and Core's cursor in the
// message's own scope, (station, bin, wire epoch), in one statement, and locks
// the bin row. The lock is what the apply takes anyway at its UPDATE; taking
// it here serializes applies for the bin across this read and the claim, so
// the cursor the apply rule uses is the one the claim advances.
func readBinScope(tx *sql.Tx, station, scopeKey string, d *protocol.BinUOPDelta) (int64, scopeCursor, error) {
	var (
		epoch   int64
		cur     scopeCursor
		lastSeq sql.NullInt64
	)
	err := tx.QueryRow(`SELECT b.delta_epoch, dd.last_seq, dd.applied_net, dd.applied_window_end
		FROM bins b
		LEFT JOIN inventory_delta_dedup dd
		  ON dd.station=$2 AND dd.scope_kind=$3 AND dd.scope_key=$4 AND dd.epoch=$5
		WHERE b.id=$1
		FOR UPDATE OF b`,
		d.BinID, station, invDeltaScopeBin, scopeKey, d.Epoch).Scan(&epoch, &lastSeq, &cur.appliedNet, &cur.appliedWindowEnd)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, cur, fmt.Errorf("BinUOPDelta target bin %d does not exist", d.BinID)
	}
	if err != nil {
		return 0, cur, fmt.Errorf("read bin %d generation and delta scope: %w", d.BinID, err)
	}
	cur.found, cur.lastSeq = lastSeq.Valid, lastSeq.Int64
	return epoch, cur, nil
}

// atOrBelowHighWater decides a message whose seq its scope has already passed.
// done=false means "not decided here": the message carries a retired
// generation and belongs to the stale-epoch guard.
//
//   - Its window is not newer than what was applied: a duplicate, or a late,
//     reordered or requeued message. Skipped with no row. When the scope's
//     messages carry a net this loses nothing — the message that passed it
//     carried its count. When they do not (an older Edge), its count is lost,
//     and the error says so for the log.
//   - Its window IS newer (wentBackward) on the bin's live generation: the
//     station's counter went backward. Recorded as an edge_rollback exception,
//     and the bin's generation is bumped so the station adopts Core's number
//     under a fresh scope. Its count is not applied: a stream that went
//     backward cannot say which of its counts Core already has.
//   - Its window is newer on a RETIRED generation: the stale-epoch guard
//     records it as a stale drop, which is what it is, and tells the station.
//   - Its window is newer on generation 0 or on a generation ahead of Core's:
//     skipped. Bumping cannot fence generation 0 (it always applies), and a
//     generation Core has not issued is not one it can end.
func (s *InventoryDeltaService) atOrBelowHighWater(tx *sql.Tx, station string, d *protocol.BinUOPDelta, currentEpoch int64, cur scopeCursor) (bool, error) {
	backward := cur.wentBackward(d.SequenceID, d.WindowEnd)
	switch {
	case backward && d.Epoch > 0 && d.Epoch == currentEpoch:
		return true, s.recordEdgeRollback(tx, station, d, cur)
	case backward && d.Epoch > 0 && d.Epoch < currentEpoch:
		return false, nil
	case backward:
		return true, fmt.Errorf("%w: seq %d at or below last_seq %d with a newer window, on generation %d (bin at %d): "+
			"the station's counter went backward, and this generation cannot be fenced", ErrInventoryDeltaSkipped,
			d.SequenceID, cur.lastSeq, d.Epoch, currentEpoch)
	case d.Net != nil && cur.appliedNet.Valid:
		return true, fmt.Errorf("%w: seq %d at or below last_seq %d: a duplicate or a late message, carried by the running net",
			ErrInventoryDeltaSkipped, d.SequenceID, cur.lastSeq)
	default:
		return true, fmt.Errorf("%w: seq %d at or below last_seq %d: a duplicate, or a late message whose count no net carries",
			ErrInventoryDeltaSkipped, d.SequenceID, cur.lastSeq)
	}
}

// recordEdgeRollback writes the edge_rollback exception and starts the bin's
// next generation, on the caller's transaction, then commits it. Returns
// ErrInventoryDeltaSkipped (wrapped): the message itself is not applied.
func (s *InventoryDeltaService) recordEdgeRollback(tx *sql.Tx, station string, d *protocol.BinUOPDelta, cur scopeCursor) error {
	if s.binManifest == nil {
		return fmt.Errorf("%w: seq %d at or below last_seq %d with a newer window — the station's counter went backward, "+
			"and no manifest service is wired to start a new generation", ErrInventoryDeltaSkipped, d.SequenceID, cur.lastSeq)
	}
	var appliedNet *int64
	if cur.appliedNet.Valid {
		appliedNet = &cur.appliedNet.Int64
	}
	detail, err := json.Marshal(struct {
		SequenceID       int64     `json:"sequence_id"`
		LastSeq          int64     `json:"last_seq"`
		WireEpoch        int64     `json:"wire_epoch"`
		WindowEnd        time.Time `json:"window_end"`
		AppliedWindowEnd time.Time `json:"applied_window_end"`
		WireDelta        int       `json:"wire_delta"`
		Net              *int64    `json:"net,omitempty"`
		AppliedNet       *int64    `json:"applied_net,omitempty"`
	}{d.SequenceID, cur.lastSeq, d.Epoch, d.WindowEnd, cur.appliedWindowEnd.Time, d.Delta, d.Net, appliedNet})
	if err != nil {
		return fmt.Errorf("marshal edge-rollback detail bin=%d: %w", d.BinID, err)
	}
	var count int
	if err := tx.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, d.BinID).Scan(&count); err != nil {
		return fmt.Errorf("read bin %d for edge-rollback exception: %w", d.BinID, err)
	}
	if err := audit.AppendBinUOPException(tx, audit.ExcEdgeRollback, d.BinID, d.PayloadCode, station, nil,
		clock.Now().UTC(), &count, &count, nil, nil, audit.OpEdgeRollback, detail); err != nil {
		return err
	}
	newEpoch, err := s.binManifest.RebaseAfterEdgeRollbackTx(tx, d.BinID, d.PayloadCode, station)
	if err != nil {
		return fmt.Errorf("rebase bin %d after edge rollback: %w", d.BinID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit edge rollback bin=%d: %w", d.BinID, err)
	}
	log.Printf("BinUOPDelta EDGE ROLLBACK bin=%d station=%s seq=%d last_seq=%d window_end=%s applied_window_end=%s — "+
		"the station's counter went backward; generation %d -> %d, count %d stands",
		d.BinID, station, d.SequenceID, cur.lastSeq, d.WindowEnd.Format(time.RFC3339Nano),
		cur.appliedWindowEnd.Time.Format(time.RFC3339Nano), d.Epoch, newEpoch, count)
	return fmt.Errorf("%w: seq %d: the station's counter went backward; bin %d moved to generation %d",
		ErrInventoryDeltaSkipped, d.SequenceID, d.BinID, newEpoch)
}

// nullWindowEnd is the window end as the dedup row stores it: NULL for a
// message that carried none.
func nullWindowEnd(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}

// nullNet is the running net as the dedup row stores it.
func nullNet(net *int64) sql.NullInt64 {
	if net == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *net, Valid: true}
}

// claimDeltaSequence advances a scope's dedup row past seq, inside the
// caller's transaction: last_seq to seq, applied_net to the net the message
// carried (unchanged when it carried none, so an older Edge's message leaves
// a NULL anchor NULL), and applied_window_end to the later of the stored and
// the carried window end. Returns (true, nil) if the row advanced, (false,
// nil) if seq was not above last_seq.
//
// PK is (station, scope_kind, scope_key, epoch). Different epochs for the same
// scope_key get separate dedup rows — a new bin load (epoch bump on
// SetForProduction) starts fresh, so a stale Edge seq counter can't shadow the
// new load's first deltas.
//
// The DO UPDATE's WHERE keeps last_seq a high-water mark. It is the order
// guard, not a loss mechanism: with the running net, what a message at or
// below it carried is carried by the message that passed it.
func claimDeltaSequence(tx *sql.Tx, station, scopeKind, scopeKey string, epoch, seq int64, net *int64, windowEnd time.Time) (bool, error) {
	res, err := tx.Exec(`
		INSERT INTO inventory_delta_dedup (station, scope_kind, scope_key, epoch, last_seq, applied_net, applied_window_end, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (station, scope_kind, scope_key, epoch)
		DO UPDATE SET last_seq = EXCLUDED.last_seq,
			applied_net = COALESCE(EXCLUDED.applied_net, inventory_delta_dedup.applied_net),
			applied_window_end = GREATEST(EXCLUDED.applied_window_end, inventory_delta_dedup.applied_window_end),
			updated_at = NOW()
		WHERE inventory_delta_dedup.last_seq < EXCLUDED.last_seq`,
		station, scopeKind, scopeKey, epoch, seq, nullNet(net), nullWindowEnd(windowEnd))
	if err != nil {
		return false, fmt.Errorf("dedup upsert station=%s scope=%s/%s epoch=%d seq=%d: %w",
			station, scopeKind, scopeKey, epoch, seq, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// claimBucketSequence advances a lineside bucket level's dedup row (epoch 0)
// past seq, returning the cursor as it stood before the claim, in the same
// statement: the prior row is read in a CTE beside the UPSERT, and both see the
// statement's snapshot, so the read costs no statement. Seq only: a level
// carries no running net, because it replaces the row rather than adding to it.
//
// NO ROW LOCK PRECEDES IT, unlike the bin path, because a pile may have no
// row to lock. Core applies count messages one at a time (one reader
// goroutine per topic, handler inline), so two applies for one scope do not
// run concurrently. If they ever did, the case this can see — the UPSERT
// updated a row the snapshot did not have — is refused rather than applied
// with a wrong cursor, and the row's next level carries the pile.
func claimBucketSequence(tx *sql.Tx, station, scopeKey string, seq int64, windowEnd time.Time) (bool, scopeCursor, error) {
	var (
		cur      scopeCursor
		inserted sql.NullBool
		lastSeq  sql.NullInt64
	)
	err := tx.QueryRow(`
		WITH prior AS (
			SELECT last_seq, applied_window_end FROM inventory_delta_dedup
			WHERE station=$1 AND scope_kind=$2 AND scope_key=$3 AND epoch=0
		), up AS (
			INSERT INTO inventory_delta_dedup (station, scope_kind, scope_key, epoch, last_seq, applied_window_end, updated_at)
			VALUES ($1, $2, $3, 0, $4, $5, NOW())
			ON CONFLICT (station, scope_kind, scope_key, epoch)
			DO UPDATE SET last_seq = EXCLUDED.last_seq,
				applied_window_end = GREATEST(EXCLUDED.applied_window_end, inventory_delta_dedup.applied_window_end),
				updated_at = NOW()
			WHERE inventory_delta_dedup.last_seq < EXCLUDED.last_seq
			RETURNING (xmax = 0) AS inserted
		)
		SELECT (SELECT inserted FROM up), (SELECT last_seq FROM prior), (SELECT applied_window_end FROM prior)`,
		station, invDeltaScopeBucketLevel, scopeKey, seq, nullWindowEnd(windowEnd)).
		Scan(&inserted, &lastSeq, &cur.appliedWindowEnd)
	if err != nil {
		return false, cur, fmt.Errorf("dedup upsert station=%s scope=%s/%s seq=%d: %w",
			station, invDeltaScopeBucketLevel, scopeKey, seq, err)
	}
	cur.found, cur.lastSeq = lastSeq.Valid, lastSeq.Int64
	if inserted.Valid && !inserted.Bool && !cur.found {
		return false, cur, fmt.Errorf("dedup upsert station=%s scope=%s/%s seq=%d: the row appeared under a concurrent apply; "+
			"not applied, the row's next level carries the pile", station, invDeltaScopeBucketLevel, scopeKey, seq)
	}
	return inserted.Valid, cur, nil
}

// reanchorBucketSequence moves a level scope's dedup row DOWN to a restored
// Edge's seq and window, so that Edge's next levels, numbered from where its
// restored counter stands, apply. Only for the wentBackward shape: a lower seq
// with a later window.
func reanchorBucketSequence(tx *sql.Tx, station, scopeKey string, seq int64, windowEnd time.Time) error {
	if _, err := tx.Exec(`UPDATE inventory_delta_dedup
		SET last_seq = $4, applied_window_end = $5, updated_at = NOW()
		WHERE station=$1 AND scope_kind=$2 AND scope_key=$3 AND epoch=0`,
		station, invDeltaScopeBucketLevel, scopeKey, seq, nullWindowEnd(windowEnd)); err != nil {
		return fmt.Errorf("re-anchor dedup station=%s scope=%s/%s seq=%d: %w",
			station, invDeltaScopeBucketLevel, scopeKey, seq, err)
	}
	return nil
}

// bucketLevelScopeKey builds the dedup scope_key for a LinesideBucketLevel:
// "<CoreNodeName>|<PayloadCode>|<State>", the key the Edge allocates the seq
// under (protocol.InvDeltaScopeBucketLevel). Stable: a rename on one side
// alone silently stops ordering the levels.
func bucketLevelScopeKey(coreNodeName, payloadCode string, state protocol.LinesideBucketState) string {
	return coreNodeName + "|" + payloadCode + "|" + string(state)
}

// InventoryInvariant carries the plant-wide running totals that
// Item 13's invariant probe endpoint exposes. BinSum is signed (per
// SME lock; bins can go negative on overpack). BucketSum stays
// non-negative by schema CHECK constraint and sums active piles only.
// Total is the rolled-up
// sum: useful as a trend indicator, not a hard equation, since
// overpack/underpack drift and operator corrections move the
// signed bin sum in either direction over time.
type InventoryInvariant struct {
	Total     int64
	BinSum    int64
	BucketSum int64
}

// SumInvariant returns the plant-wide running totals across all bins
// and the ACTIVE lineside piles (the ones on-hand counts; a stranded row is
// a count anomaly, not stock). Item 13. Both queries are aggregates
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
