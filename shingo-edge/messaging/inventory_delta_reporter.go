// inventory_delta_reporter.go — bin-as-truth signed-delta accumulator.
//
// Edge accumulates per-bin and per-bucket signed UOP changes from the
// PLC tick path and the operator release path, then periodically
// flushes the running totals as BinUOPDelta / LinesideBucketDelta
// envelopes through the existing outbox.
//
// Concurrency: sync.Map for the per-scope accumulator with a
// per-entry sync.Mutex protecting the composite metadata. Hot path
// is RecordBin / RecordBucket; flush goroutine ranges across the map
// and atomically swaps each entry's running delta to zero. The
// mutex is contended only when record and flush hit the same scope
// simultaneously — vanishingly rare given PLC tick cadence is
// O(seconds).
//
// Restore-on-failure: if the outbox enqueue fails for an envelope,
// the swept delta is folded back into the live entry. Mirrors
// production_reporter.go's restoreSnapshot pattern. Forensics
// preference: never lose a count change to a transient outbox blip.
//
// Flush triggers: periodic timer (5s default, YAML-configurable),
// plus OrderRelease envelope sent (consume side), bin-loader confirms
// a load (produce/manual_swap side), A/B active-pull state flip
// (paired-node runtime flip), and BinPickedUp arrival (SEND PARTIAL
// BACK pickup window).
package messaging

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingoedge/store"
)

const (
	// invDeltaScopeBin / invDeltaScopeBucket — scope_kind values used
	// when allocating sequence-ids and when Core dedups. Stable
	// strings; renames must come with a coordinated migration on
	// both sides.
	invDeltaScopeBin    = "bin"
	invDeltaScopeBucket = "bucket"

	// defaultInventoryDeltaInterval is the periodic flush cadence used
	// when the caller leaves Interval unset. 5s matches plan
	// Decision #2; YAML-configurable per the rollout plan.
	defaultInventoryDeltaInterval = 5 * time.Second
)

// binDeltaEntry is the per-bin accumulator. Exported fields are written
// only under mu; the running delta is read-and-zeroed atomically by
// the flush goroutine and incremented in place by the record path.
type binDeltaEntry struct {
	mu          sync.Mutex
	binID       int64
	delta       int
	payloadCode string
	reason      protocol.BinUOPDeltaReason
	windowStart time.Time
	windowEnd   time.Time
}

// bucketDeltaEntry is the per-bucket accumulator. Composite key is
// (nodeID, pairKey, styleID, partNumber); these fields are immutable
// for the lifetime of an entry (a different composite key produces a
// different sync.Map entry).
type bucketDeltaEntry struct {
	mu          sync.Mutex
	nodeID      int64
	pairKey     string
	styleID     int64
	partNumber  string
	delta       int
	reason      protocol.LinesideBucketDeltaReason
	windowStart time.Time
	windowEnd   time.Time
}

// InventoryDeltaReporter accumulates BinUOPDelta and
// LinesideBucketDelta count changes and flushes them to the outbox on
// a periodic cadence. Mirrors ProductionReporter's lifecycle (Start /
// Stop) and outbox plumbing.
type InventoryDeltaReporter struct {
	db        *store.DB
	stationID string
	interval  time.Duration

	// Two sync.Maps. Keys are stable strings:
	//   bin scope:    strconv(BinID)
	//   bucket scope: bucketScopeKey(...) — pipe-delimited composite
	bins    sync.Map // map[string]*binDeltaEntry
	buckets sync.Map // map[string]*bucketDeltaEntry

	// pendingBinIDs / pendingBucketKeys track scopes with deltas
	// recorded but not yet enqueued to the outbox. The reconciler
	// queries these via IsPendingBinDelta / IsPendingBucketDelta and
	// skips healing scopes that are still in flight — without the
	// guard, a reconciliation pass that races with an unflushed
	// accumulator would stomp the local cache with a stale Core read
	// (Core hasn't seen the delta yet). pendingMu guards both maps;
	// kept separate from flushMu so the record path doesn't contend
	// with periodic flushes.
	pendingMu         sync.Mutex
	pendingBinIDs     map[int64]struct{}
	pendingBucketKeys map[pendingBucketKey]struct{}

	// flush serializes flush passes against each other (Stop's final
	// flush must not race with the periodic loop). RecordBin /
	// RecordBucket do not take this lock.
	flushMu sync.Mutex

	// flushFailures counts EnqueueOutbox failures across the bin and
	// bucket flush paths. Item 9 surfaces this via the reconciler
	// metrics HTTP endpoint — sustained non-zero growth signals the
	// outbox is wedged (full disk, schema mismatch, etc.) and the
	// pending-delta guard will start blocking releases.
	flushFailures atomic.Int64

	stopOnce sync.Once
	stopCh   chan struct{}

	// flushed is closed once after every flush attempt completes.
	// Tests use TriggerFlush to drive a synchronous flush; production
	// code does not read this channel.
	flushSignal chan struct{}

	// now is overridable for tests. Production callers leave it nil
	// and the reporter uses time.Now().UTC().
	now func() time.Time

	DebugLog DebugLogFunc
}

// NewInventoryDeltaReporter constructs a reporter for the given Edge
// identity. Caller wires DebugLog and Interval (or leaves them at
// their zero values for the production defaults) before calling Start.
func NewInventoryDeltaReporter(db *store.DB, stationID string) *InventoryDeltaReporter {
	return &InventoryDeltaReporter{
		db:                db,
		stationID:         stationID,
		interval:          defaultInventoryDeltaInterval,
		stopCh:            make(chan struct{}),
		flushSignal:       make(chan struct{}, 1),
		pendingBinIDs:     make(map[int64]struct{}),
		pendingBucketKeys: make(map[pendingBucketKey]struct{}),
	}
}

// SetInterval overrides the periodic flush cadence. Intended for the
// composition root reading the YAML config; unsafe to call after
// Start (the running goroutine will not pick up the change).
func (r *InventoryDeltaReporter) SetInterval(d time.Duration) {
	if d > 0 {
		r.interval = d
	}
}

// Start begins the periodic flush loop. Idempotent: a second Start
// after Stop is a no-op (the stopOnce / stopCh contract assumes a
// single lifecycle per reporter instance).
func (r *InventoryDeltaReporter) Start() {
	go r.loop()
}

// Stop halts the periodic loop and runs one final flush so any
// accumulated deltas in flight at shutdown still reach the outbox.
// Idempotent.
func (r *InventoryDeltaReporter) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
		r.Flush()
	})
}

// RecordBin accumulates a signed delta against a specific bin under
// the given reason. payloadCode is required (Core validates it
// against the bin row); window timestamps are taken from the
// reporter's clock.
//
// Multiple deltas in the same window for the same bin sum into a
// single envelope on the next flush; reason is the most recent value
// recorded — if a window mixes consume_tick with capture_reduction
// (rare; release transitions don't typically overlap with steady-
// state ticks) the audit trail at Core uses the latest reason. A
// future refinement could split per-reason at the cost of one
// SequenceID per (bin, reason).
func (r *InventoryDeltaReporter) RecordBin(binID int64, payloadCode string, delta int, reason protocol.BinUOPDeltaReason) {
	if delta == 0 {
		return
	}
	if binID <= 0 {
		return
	}
	key := strconv.FormatInt(binID, 10)
	now := r.clock()

	v, _ := r.bins.LoadOrStore(key, &binDeltaEntry{
		binID:       binID,
		payloadCode: payloadCode,
		windowStart: now,
	})
	e := v.(*binDeltaEntry)

	e.mu.Lock()
	if e.delta == 0 {
		// First contribution to this window — anchor the start.
		e.windowStart = now
		e.payloadCode = payloadCode
	}
	e.delta += delta
	e.reason = reason
	e.windowEnd = now
	e.mu.Unlock()

	r.pendingMu.Lock()
	r.pendingBinIDs[binID] = struct{}{}
	r.pendingMu.Unlock()

	r.DebugLog.Log("inventory_delta: bin=%d delta=%+d reason=%s payload=%q",
		binID, delta, reason, payloadCode)
}

// RecordBucket accumulates a signed delta against a specific lineside
// bucket. NEVER called from manual_swap nodes — the plan locks
// "manual swap nodes never emit bucket deltas" because they have no
// PLC and their count-change events are operator actions on the bin,
// not the bucket.
func (r *InventoryDeltaReporter) RecordBucket(nodeID int64, pairKey string, styleID int64, partNumber string, delta int, reason protocol.LinesideBucketDeltaReason) {
	if delta == 0 {
		return
	}
	if nodeID <= 0 || partNumber == "" {
		return
	}
	key := bucketScopeKey(nodeID, pairKey, styleID, partNumber)
	now := r.clock()

	v, _ := r.buckets.LoadOrStore(key, &bucketDeltaEntry{
		nodeID:      nodeID,
		pairKey:     pairKey,
		styleID:     styleID,
		partNumber:  partNumber,
		windowStart: now,
	})
	e := v.(*bucketDeltaEntry)

	e.mu.Lock()
	if e.delta == 0 {
		e.windowStart = now
	}
	e.delta += delta
	e.reason = reason
	e.windowEnd = now
	e.mu.Unlock()

	r.pendingMu.Lock()
	r.pendingBucketKeys[pendingBucketKey{nodeID: nodeID, styleID: styleID, partNumber: partNumber}] = struct{}{}
	r.pendingMu.Unlock()

	r.DebugLog.Log("inventory_delta: bucket node=%d part=%q delta=%+d reason=%s",
		nodeID, partNumber, delta, reason)
}

// Flush performs one synchronous flush pass. Boundary triggers call
// this in addition to the periodic loop:
//
//   - OrderRelease envelope sent (consume-side)
//   - Bin-loader confirms a load (produce / manual_swap)
//   - A/B cycling active-pull state flip on a paired node
//   - BinPickedUp arrival (SEND PARTIAL BACK pickup window)
//
// Safe to call from any goroutine; serialized via flushMu.
func (r *InventoryDeltaReporter) Flush() {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.flushBins()
	r.flushBuckets()

	// Non-blocking publish to flushSignal so tests blocking on
	// "wait for flush" wake up exactly once. Production code doesn't
	// read this channel.
	select {
	case r.flushSignal <- struct{}{}:
	default:
	}
}

func (r *InventoryDeltaReporter) loop() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.Flush()
		}
	}
}

// flushBins ranges across the bin accumulator, sweeps each entry's
// running delta, allocates a SequenceID, and enqueues the envelope.
// Restores the delta on enqueue failure so a transient outbox blip
// doesn't drop count changes.
func (r *InventoryDeltaReporter) flushBins() {
	r.bins.Range(func(key, value any) bool {
		k := key.(string)
		e := value.(*binDeltaEntry)

		// Sweep: capture and zero the running delta + window in one
		// critical section so concurrent RecordBin starts a fresh
		// window cleanly. Extract fields explicitly rather than
		// copying the struct — the embedded sync.Mutex must not be
		// duplicated.
		e.mu.Lock()
		if e.delta == 0 {
			e.mu.Unlock()
			return true
		}
		sBinID := e.binID
		sDelta := e.delta
		sPayloadCode := e.payloadCode
		sReason := e.reason
		sWindowStart := e.windowStart
		sWindowEnd := e.windowEnd
		e.delta = 0
		e.windowStart = time.Time{}
		e.windowEnd = time.Time{}
		e.mu.Unlock()

		seq, err := r.db.AllocateInventoryDeltaSeq(invDeltaScopeBin, k)
		if err != nil {
			log.Printf("inventory_delta_reporter: allocate bin seq key=%s: %v", k, err)
			r.restoreBinDelta(e, sDelta, sReason, sWindowStart, sWindowEnd, sPayloadCode)
			return true
		}

		env, encErr := protocol.NewDataEnvelope(
			protocol.SubjectBinUOPDelta,
			protocol.Address{Role: protocol.RoleEdge, Station: r.stationID},
			protocol.Address{Role: protocol.RoleCore},
			&protocol.BinUOPDelta{
				Station:     r.stationID,
				BinID:       sBinID,
				PayloadCode: sPayloadCode,
				Delta:       sDelta,
				Reason:      sReason,
				SequenceID:  seq,
				WindowStart: sWindowStart,
				WindowEnd:   sWindowEnd,
			},
		)
		if encErr != nil {
			log.Printf("inventory_delta_reporter: build bin envelope key=%s: %v", k, encErr)
			r.restoreBinDelta(e, sDelta, sReason, sWindowStart, sWindowEnd, sPayloadCode)
			return true
		}
		data, encErr := env.Encode()
		if encErr != nil {
			log.Printf("inventory_delta_reporter: encode bin envelope key=%s: %v", k, encErr)
			r.restoreBinDelta(e, sDelta, sReason, sWindowStart, sWindowEnd, sPayloadCode)
			return true
		}
		if _, err := r.db.EnqueueOutbox(data, protocol.SubjectBinUOPDelta); err != nil {
			r.flushFailures.Add(1)
			log.Printf("ERROR: inventory_delta_reporter: enqueue bin envelope key=%s, restoring: %v", k, err)
			r.restoreBinDelta(e, sDelta, sReason, sWindowStart, sWindowEnd, sPayloadCode)
			return true
		}
		// Enqueue confirmed; clear pending only if no new delta
		// accumulated during the enqueue. Hold the entry mutex while
		// checking so a concurrent RecordBin can't slip a +delta in
		// between the empty-check and the pending-delete (which would
		// strand the new delta as "not pending" until the next flush
		// — letting the reconciler heal during a real in-flight
		// window).
		e.mu.Lock()
		drained := e.delta == 0
		e.mu.Unlock()
		if drained {
			r.pendingMu.Lock()
			delete(r.pendingBinIDs, sBinID)
			r.pendingMu.Unlock()
		}

		r.DebugLog.Log("inventory_delta_reporter: flushed bin=%d delta=%+d seq=%d reason=%s",
			sBinID, sDelta, seq, sReason)
		return true
	})
}

// flushBuckets is the bucket-side mirror of flushBins.
func (r *InventoryDeltaReporter) flushBuckets() {
	r.buckets.Range(func(key, value any) bool {
		k := key.(string)
		e := value.(*bucketDeltaEntry)

		e.mu.Lock()
		if e.delta == 0 {
			e.mu.Unlock()
			return true
		}
		sNodeID := e.nodeID
		sPairKey := e.pairKey
		sStyleID := e.styleID
		sPartNumber := e.partNumber
		sDelta := e.delta
		sReason := e.reason
		sWindowStart := e.windowStart
		sWindowEnd := e.windowEnd
		e.delta = 0
		e.windowStart = time.Time{}
		e.windowEnd = time.Time{}
		e.mu.Unlock()

		seq, err := r.db.AllocateInventoryDeltaSeq(invDeltaScopeBucket, k)
		if err != nil {
			log.Printf("inventory_delta_reporter: allocate bucket seq key=%s: %v", k, err)
			r.restoreBucketDelta(e, sDelta, sReason, sWindowStart, sWindowEnd)
			return true
		}

		env, encErr := protocol.NewDataEnvelope(
			protocol.SubjectLinesideBucketDelta,
			protocol.Address{Role: protocol.RoleEdge, Station: r.stationID},
			protocol.Address{Role: protocol.RoleCore},
			&protocol.LinesideBucketDelta{
				Station:     r.stationID,
				NodeID:      sNodeID,
				PairKey:     sPairKey,
				StyleID:     sStyleID,
				PartNumber:  sPartNumber,
				Delta:       sDelta,
				Reason:      sReason,
				SequenceID:  seq,
				WindowStart: sWindowStart,
				WindowEnd:   sWindowEnd,
			},
		)
		if encErr != nil {
			log.Printf("inventory_delta_reporter: build bucket envelope key=%s: %v", k, encErr)
			r.restoreBucketDelta(e, sDelta, sReason, sWindowStart, sWindowEnd)
			return true
		}
		data, encErr := env.Encode()
		if encErr != nil {
			log.Printf("inventory_delta_reporter: encode bucket envelope key=%s: %v", k, encErr)
			r.restoreBucketDelta(e, sDelta, sReason, sWindowStart, sWindowEnd)
			return true
		}
		if _, err := r.db.EnqueueOutbox(data, protocol.SubjectLinesideBucketDelta); err != nil {
			r.flushFailures.Add(1)
			log.Printf("ERROR: inventory_delta_reporter: enqueue bucket envelope key=%s, restoring: %v", k, err)
			r.restoreBucketDelta(e, sDelta, sReason, sWindowStart, sWindowEnd)
			return true
		}
		// Symmetric to flushBins: only clear pending if no new delta
		// accumulated during enqueue.
		e.mu.Lock()
		drained := e.delta == 0
		e.mu.Unlock()
		if drained {
			r.pendingMu.Lock()
			delete(r.pendingBucketKeys, pendingBucketKey{nodeID: sNodeID, styleID: sStyleID, partNumber: sPartNumber})
			r.pendingMu.Unlock()
		}

		r.DebugLog.Log("inventory_delta_reporter: flushed bucket node=%d part=%q delta=%+d seq=%d reason=%s",
			sNodeID, sPartNumber, sDelta, seq, sReason)
		return true
	})
}

func (r *InventoryDeltaReporter) restoreBinDelta(e *binDeltaEntry, delta int, reason protocol.BinUOPDeltaReason, windowStart, windowEnd time.Time, payloadCode string) {
	e.mu.Lock()
	if e.delta == 0 {
		// Window was empty — restore the original window bounds too
		// so a subsequent successful flush still reflects the right
		// time range.
		e.windowStart = windowStart
		e.payloadCode = payloadCode
	}
	e.delta += delta
	if e.windowEnd.Before(windowEnd) {
		e.windowEnd = windowEnd
	}
	if e.reason == "" {
		e.reason = reason
	}
	e.mu.Unlock()
}

func (r *InventoryDeltaReporter) restoreBucketDelta(e *bucketDeltaEntry, delta int, reason protocol.LinesideBucketDeltaReason, windowStart, windowEnd time.Time) {
	e.mu.Lock()
	if e.delta == 0 {
		e.windowStart = windowStart
	}
	e.delta += delta
	if e.windowEnd.Before(windowEnd) {
		e.windowEnd = windowEnd
	}
	if e.reason == "" {
		e.reason = reason
	}
	e.mu.Unlock()
}

func (r *InventoryDeltaReporter) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now().UTC()
}

// FlushFailures returns the cumulative count of EnqueueOutbox failures
// since process start. Item 9 surfaces this via the engine's reconciler
// metrics endpoint so dashboards can trend outbox health.
func (r *InventoryDeltaReporter) FlushFailures() int64 {
	return r.flushFailures.Load()
}

// IsPendingBinDelta reports whether the reporter has accumulated or
// in-flight deltas for the given bin id. The reconciler queries this
// before healing a bin's local cache from Core's authoritative read —
// while a delta is in flight, Core's snapshot is stale relative to
// Edge's accumulator (or the outbox), and healing would stomp the
// local value with stale Core state. Skipping the heal lets the next
// pass try once the in-flight window closes.
func (r *InventoryDeltaReporter) IsPendingBinDelta(binID int64) bool {
	r.pendingMu.Lock()
	_, ok := r.pendingBinIDs[binID]
	r.pendingMu.Unlock()
	return ok
}

// IsPendingBucketDelta reports whether the reporter has accumulated or
// in-flight bucket deltas for the given (node, style, part) tuple.
// The pending set keys on these three fields without the pairKey —
// matches the reconciler's query signature, which doesn't carry
// pairKey context, and is conservative (any pair-key for this scope
// counts as pending).
func (r *InventoryDeltaReporter) IsPendingBucketDelta(nodeID, styleID int64, partNumber string) bool {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	_, ok := r.pendingBucketKeys[pendingBucketKey{nodeID: nodeID, styleID: styleID, partNumber: partNumber}]
	return ok
}

// LoadPendingFromOutbox seeds the pending sets from outbox entries that
// were enqueued before the process started but haven't been sent yet.
// Call once at startup, before the reconciler begins. Without it, a
// post-crash recovery would fail to gate the first reconciliation
// pass: deltas already on-disk in the outbox aren't in the in-memory
// pendingBinIDs map, so the reconciler would happily heal a bin whose
// in-flight delta hasn't been applied at Core yet.
//
// A row that fails to decode is skipped (logged but absorbed) rather
// than failing the whole load — a partially-loaded pending set still
// gates the bins it could decode, which is strictly better than empty.
func (r *InventoryDeltaReporter) LoadPendingFromOutbox() error {
	msgs, err := r.db.ListUnsentOutboxByType([]string{
		protocol.SubjectBinUOPDelta,
		protocol.SubjectLinesideBucketDelta,
	})
	if err != nil {
		return err
	}
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	// Outbox payload structure: bytes → Envelope (.Payload is a
	// marshaled Data wrapper) → Data (.Body is the typed delta). Three
	// layers; the typed-delta unmarshal needs Data.Body, not
	// Envelope.Payload directly.
	for _, m := range msgs {
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			log.Printf("inventory_delta_reporter: decode outbox row id=%d: %v", m.ID, err)
			continue
		}
		var data protocol.Data
		if err := json.Unmarshal(env.Payload, &data); err != nil {
			log.Printf("inventory_delta_reporter: decode data wrapper row id=%d: %v", m.ID, err)
			continue
		}
		switch m.MsgType {
		case protocol.SubjectBinUOPDelta:
			var d protocol.BinUOPDelta
			if err := json.Unmarshal(data.Body, &d); err != nil {
				log.Printf("inventory_delta_reporter: decode bin delta row id=%d: %v", m.ID, err)
				continue
			}
			r.pendingBinIDs[d.BinID] = struct{}{}
		case protocol.SubjectLinesideBucketDelta:
			var d protocol.LinesideBucketDelta
			if err := json.Unmarshal(data.Body, &d); err != nil {
				log.Printf("inventory_delta_reporter: decode bucket delta row id=%d: %v", m.ID, err)
				continue
			}
			r.pendingBucketKeys[pendingBucketKey{nodeID: d.NodeID, styleID: d.StyleID, partNumber: d.PartNumber}] = struct{}{}
		}
	}
	return nil
}

// pendingBucketKey is the in-memory key for the pending bucket set —
// node id + style id + part number. Intentionally asymmetric with
// bucketScopeKey (which carries pairKey for Core's dedup table):
//
//   - bucketScopeKey: full (nodeID|pairKey|styleID|partNumber). Stable
//     wire format pinned by Core's inventory_delta_dedup. Do not change.
//   - pendingBucketKey: drops pairKey. The reconciler's IsPendingBucketDelta
//     query signature doesn't carry pairKey context (Core's snapshot
//     row pairs aren't always meaningful Edge-side), so the gate keys
//     on what the caller can supply.
//
// The asymmetry is deliberate. Resist the urge to "fix" the gate to
// match the dedup key — treating any pair as pending for a given
// (node, style, part) scope is the *conservative* gate: false positives
// are no-op skips that the next pass picks up. False negatives (the
// alternative if we missed-keyed) would mean healing during an
// in-flight delta, which is the bug Item 2 was designed to prevent.
type pendingBucketKey struct {
	nodeID     int64
	styleID    int64
	partNumber string
}

// bucketScopeKey builds the dedup scope_key for a LinesideBucketDelta.
// Mirror of the Core helper in service/inventory_delta_service.go;
// must produce byte-identical output. The pipe-delimited format is
// stable; renames break in-flight Edge replays, so any change must
// come with a coordinated migration on both sides.
func bucketScopeKey(nodeID int64, pairKey string, styleID int64, partNumber string) string {
	var sb strings.Builder
	sb.WriteString(strconv.FormatInt(nodeID, 10))
	sb.WriteByte('|')
	sb.WriteString(pairKey)
	sb.WriteByte('|')
	sb.WriteString(strconv.FormatInt(styleID, 10))
	sb.WriteByte('|')
	sb.WriteString(partNumber)
	return sb.String()
}
