// accumulator.go — bin-as-truth signed-delta accumulator.
//
// Internal implementation of UOP delta accumulation. The public surface
// is in mutator.go; this file owns the per-scope sync.Map state, the
// periodic flush goroutine, the outbox enqueue path, and the
// restore-on-failure semantics.
//
// Concurrency: sync.Map for the per-scope accumulator with a
// per-entry sync.Mutex protecting the composite metadata. Hot path
// is recordBin / recordBucket; flush goroutine ranges across the map
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
package uop

import (
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
	// when the caller leaves interval unset. 5s matches the original
	// rollout plan; YAML-configurable.
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
//
// payloadCode (UOP-threshold replenishment) carries the payload this
// bucket's parts belong to. Latched on first non-empty recordBucket
// call for the key; subsequent calls with the same key only overwrite
// when they bring a non-empty value (a downstream caller that doesn't
// have the payload handy shouldn't be able to wipe one that's already
// set). Empty on the wire = "unknown" — Core's UPSERT preserves the
// existing payload_code.
type bucketDeltaEntry struct {
	mu sync.Mutex
	// nodeID is Edge's local process_nodes.id — kept so flush-time
	// logging and the louder drain diagnostic still work, but NOT on
	// the wire post-Round-3-Obs-8.
	nodeID int64
	// coreNodeName is the cross-system identifier Core resolves to
	// nodes.id at apply time. Populated by the engine when recording
	// the delta; Edge no longer leaks its local int64 namespace.
	coreNodeName string
	pairKey      string
	styleID      int64
	partNumber   string
	payloadCode  string
	delta        int
	reason       protocol.LinesideBucketDeltaReason
	windowStart  time.Time
	windowEnd    time.Time
}

// accumulator accumulates BinUOPDelta and LinesideBucketDelta count
// changes and flushes them to the outbox on a periodic cadence.
// Package-private — exposed through Mutator (see mutator.go).
type accumulator struct {
	db        *store.DB
	stationID string
	interval  time.Duration

	// Two sync.Maps. Keys are stable strings:
	//   bin scope:    strconv(BinID)
	//   bucket scope: bucketScopeKey(...) — pipe-delimited composite
	bins    sync.Map // map[string]*binDeltaEntry
	buckets sync.Map // map[string]*bucketDeltaEntry

	// flushMu serializes flush passes against each other (stop's final
	// flush must not race with the periodic loop). recordBin /
	// recordBucket do not take this lock.
	flushMu sync.Mutex

	// flushFailures counts EnqueueOutbox failures across the bin and
	// bucket flush paths. Surfaces via FlushFailures() for outbox-health
	// dashboards — sustained non-zero growth signals the outbox is
	// wedged (full disk, schema mismatch, Kafka unavailable, etc.).
	flushFailures atomic.Int64

	stopOnce sync.Once
	stopCh   chan struct{}

	// flushSignal is published-to once after every flush attempt completes.
	// Tests use the channel to drive a synchronous flush; production
	// code does not read this channel.
	flushSignal chan struct{}

	// now is overridable for tests. Production callers leave it nil
	// and the accumulator uses time.Now().UTC().
	now func() time.Time

	debugLog DebugLogFunc
}

// newAccumulator constructs an accumulator for the given Edge identity.
// Caller wires debugLog and interval via the Mutator wrapper before
// calling start.
func newAccumulator(db *store.DB, stationID string) *accumulator {
	return &accumulator{
		db:          db,
		stationID:   stationID,
		interval:    defaultInventoryDeltaInterval,
		stopCh:      make(chan struct{}),
		flushSignal: make(chan struct{}, 1),
	}
}

// setInterval overrides the periodic flush cadence. Intended for the
// composition root reading the YAML config; unsafe to call after
// start (the running goroutine will not pick up the change).
func (r *accumulator) setInterval(d time.Duration) {
	if d > 0 {
		r.interval = d
	}
}

// start begins the periodic flush loop. Idempotent: a second start
// after stop is a no-op (the stopOnce / stopCh contract assumes a
// single lifecycle per accumulator instance).
func (r *accumulator) start() {
	go r.loop()
}

// stop halts the periodic loop and runs one final flush so any
// accumulated deltas in flight at shutdown still reach the outbox.
// Idempotent.
func (r *accumulator) stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
		r.flush()
	})
}

// recordBin accumulates a signed delta against a specific bin under
// the given reason. payloadCode is required (Core validates it
// against the bin row); window timestamps are taken from the
// accumulator's clock.
//
// Multiple deltas in the same window for the same bin sum into a
// single envelope on the next flush; reason is the most recent value
// recorded — if a window mixes consume_tick with capture_reduction
// (rare; release transitions don't typically overlap with steady-
// state ticks) the audit trail at Core uses the latest reason. A
// future refinement could split per-reason at the cost of one
// SequenceID per (bin, reason).
func (r *accumulator) recordBin(binID int64, payloadCode string, delta int, reason protocol.BinUOPDeltaReason) {
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

	r.debugLog.Log("inventory_delta: bin=%d delta=%+d reason=%s payload=%q",
		binID, delta, reason, payloadCode)
}

// recordBucket accumulates a signed delta against a specific lineside
// bucket. NEVER called from manual_swap nodes — the plan locks
// "manual swap nodes never emit bucket deltas" because they have no
// PLC and their count-change events are operator actions on the bin,
// not the bucket.
//
// coreNodeName is the cross-system identifier that goes on the wire
// (Round-3 Obs 8). Edge's local nodeID stays only for the in-memory
// dedup key and flush-time logging — Core no longer sees Edge's
// process_nodes.id namespace.
func (r *accumulator) recordBucket(nodeID int64, coreNodeName, pairKey string, styleID int64, partNumber, payloadCode string, delta int, reason protocol.LinesideBucketDeltaReason) {
	if delta == 0 {
		return
	}
	if nodeID <= 0 || partNumber == "" {
		return
	}
	key := bucketScopeKey(nodeID, pairKey, styleID, partNumber)
	now := r.clock()

	v, _ := r.buckets.LoadOrStore(key, &bucketDeltaEntry{
		nodeID:       nodeID,
		coreNodeName: coreNodeName,
		pairKey:      pairKey,
		styleID:      styleID,
		partNumber:   partNumber,
		payloadCode:  payloadCode,
		windowStart:  now,
	})
	e := v.(*bucketDeltaEntry)

	e.mu.Lock()
	if e.delta == 0 {
		e.windowStart = now
	}
	// Only overwrite payloadCode with a non-empty value; an unset
	// caller must not wipe a previously-latched one. This mirrors
	// Core's UPSERT policy on the apply side.
	if payloadCode != "" {
		e.payloadCode = payloadCode
	}
	e.delta += delta
	e.reason = reason
	e.windowEnd = now
	e.mu.Unlock()

	r.debugLog.Log("inventory_delta: bucket node=%d part=%q payload=%q delta=%+d reason=%s",
		nodeID, partNumber, payloadCode, delta, reason)
}

// flush performs one synchronous flush pass. Boundary triggers call
// this in addition to the periodic loop:
//
//   - OrderRelease envelope sent (consume-side)
//   - Bin-loader confirms a load (produce / manual_swap)
//   - A/B cycling active-pull state flip on a paired node
//   - BinPickedUp arrival (SEND PARTIAL BACK pickup window)
//
// Safe to call from any goroutine; serialized via flushMu.
func (r *accumulator) flush() {
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

func (r *accumulator) loop() {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.flush()
		}
	}
}

// flushBins ranges across the bin accumulator, sweeps each entry's
// running delta, allocates a SequenceID, and enqueues the envelope.
// Restores the delta on enqueue failure so a transient outbox blip
// doesn't drop count changes.
func (r *accumulator) flushBins() {
	r.bins.Range(func(key, value any) bool {
		k := key.(string)
		e := value.(*binDeltaEntry)

		// Sweep: capture and zero the running delta + window in one
		// critical section so concurrent recordBin starts a fresh
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
			log.Printf("uop accumulator: allocate bin seq key=%s: %v", k, err)
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
			log.Printf("uop accumulator: build bin envelope key=%s: %v", k, encErr)
			r.restoreBinDelta(e, sDelta, sReason, sWindowStart, sWindowEnd, sPayloadCode)
			return true
		}
		data, encErr := env.Encode()
		if encErr != nil {
			log.Printf("uop accumulator: encode bin envelope key=%s: %v", k, encErr)
			r.restoreBinDelta(e, sDelta, sReason, sWindowStart, sWindowEnd, sPayloadCode)
			return true
		}
		if _, err := r.db.EnqueueOutbox(data, protocol.SubjectBinUOPDelta); err != nil {
			r.flushFailures.Add(1)
			log.Printf("ERROR: uop accumulator: enqueue bin envelope key=%s, restoring: %v", k, err)
			r.restoreBinDelta(e, sDelta, sReason, sWindowStart, sWindowEnd, sPayloadCode)
			return true
		}

		r.debugLog.Log("uop accumulator: flushed bin=%d delta=%+d seq=%d reason=%s",
			sBinID, sDelta, seq, sReason)
		return true
	})
}

// flushBuckets is the bucket-side mirror of flushBins.
func (r *accumulator) flushBuckets() {
	r.buckets.Range(func(key, value any) bool {
		k := key.(string)
		e := value.(*bucketDeltaEntry)

		e.mu.Lock()
		if e.delta == 0 {
			e.mu.Unlock()
			return true
		}
		sNodeID := e.nodeID
		sCoreNodeName := e.coreNodeName
		sPairKey := e.pairKey
		sStyleID := e.styleID
		sPartNumber := e.partNumber
		sPayloadCode := e.payloadCode
		sDelta := e.delta
		sReason := e.reason
		sWindowStart := e.windowStart
		sWindowEnd := e.windowEnd
		e.delta = 0
		e.windowStart = time.Time{}
		e.windowEnd = time.Time{}
		e.mu.Unlock()

		// Defensive: if the entry pre-dates the Round-3 Obs 8 change
		// and was buffered without coreNodeName, resolve from the DB.
		// In normal operation recordBucket populates this at write
		// time so we never hit the lookup.
		if sCoreNodeName == "" {
			if node, lookupErr := r.db.GetProcessNode(sNodeID); lookupErr == nil && node != nil {
				sCoreNodeName = node.CoreNodeName
			}
		}
		if sCoreNodeName == "" {
			// Drop the delta loudly rather than emit a wire envelope
			// with an empty CoreNodeName — Core's applier validates
			// the field at insert time and would drop the delta
			// anyway. Logging here surfaces the problem at the
			// source.
			log.Printf("ERROR: uop accumulator: drop bucket delta key=%s — no core_node_name resolvable for nodeID=%d (process_node row missing?)",
				k, sNodeID)
			r.flushFailures.Add(1)
			return true
		}

		seq, err := r.db.AllocateInventoryDeltaSeq(invDeltaScopeBucket, k)
		if err != nil {
			log.Printf("uop accumulator: allocate bucket seq key=%s: %v", k, err)
			r.restoreBucketDelta(e, sDelta, sReason, sWindowStart, sWindowEnd)
			return true
		}

		env, encErr := protocol.NewDataEnvelope(
			protocol.SubjectLinesideBucketDelta,
			protocol.Address{Role: protocol.RoleEdge, Station: r.stationID},
			protocol.Address{Role: protocol.RoleCore},
			&protocol.LinesideBucketDelta{
				Station:      r.stationID,
				CoreNodeName: sCoreNodeName,
				PairKey:      sPairKey,
				StyleID:      sStyleID,
				PartNumber:   sPartNumber,
				PayloadCode:  sPayloadCode,
				Delta:        sDelta,
				Reason:       sReason,
				SequenceID:   seq,
				WindowStart:  sWindowStart,
				WindowEnd:    sWindowEnd,
			},
		)
		if encErr != nil {
			log.Printf("uop accumulator: build bucket envelope key=%s: %v", k, encErr)
			r.restoreBucketDelta(e, sDelta, sReason, sWindowStart, sWindowEnd)
			return true
		}
		data, encErr := env.Encode()
		if encErr != nil {
			log.Printf("uop accumulator: encode bucket envelope key=%s: %v", k, encErr)
			r.restoreBucketDelta(e, sDelta, sReason, sWindowStart, sWindowEnd)
			return true
		}
		if _, err := r.db.EnqueueOutbox(data, protocol.SubjectLinesideBucketDelta); err != nil {
			r.flushFailures.Add(1)
			log.Printf("ERROR: uop accumulator: enqueue bucket envelope key=%s, restoring: %v", k, err)
			r.restoreBucketDelta(e, sDelta, sReason, sWindowStart, sWindowEnd)
			return true
		}

		r.debugLog.Log("uop accumulator: flushed bucket node=%d part=%q delta=%+d seq=%d reason=%s",
			sNodeID, sPartNumber, sDelta, seq, sReason)
		return true
	})
}

func (r *accumulator) restoreBinDelta(e *binDeltaEntry, delta int, reason protocol.BinUOPDeltaReason, windowStart, windowEnd time.Time, payloadCode string) {
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

func (r *accumulator) restoreBucketDelta(e *bucketDeltaEntry, delta int, reason protocol.LinesideBucketDeltaReason, windowStart, windowEnd time.Time) {
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

func (r *accumulator) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now().UTC()
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
