// accumulator.go — bin-as-truth signed-delta accumulator.
//
// Internal implementation of UOP delta accumulation. The public surface
// is in mutator.go; this file owns the per-scope sync.Map state, the
// periodic flush goroutine, and the outbox enqueue path.
//
// Concurrency: sync.Map for the per-scope accumulator with a
// per-entry sync.Mutex protecting the composite metadata. Hot path
// is recordBin / recordBucket; flush goroutine ranges across the map
// using send-then-sweep: snapshot the entry's state, do the DB work
// without the lock, commit a subtract on success. The mutex is
// contended only briefly during snapshot and commit.
//
// Send-then-sweep correctness: the entry is never mutated until
// enqueue succeeds. On any failure (allocate-seq, encode, enqueue)
// or panic, the entry is unchanged and the next flush picks up the
// same delta — plus any concurrent additions during the failed
// attempt — as one batch. No restore logic, no needsRestore sentinel
// discipline, no risk of dropping windowStart on concurrent writes.
//
// Flush triggers: periodic timer (5s default, YAML-configurable),
// plus OrderRelease envelope sent (consume side), bin-loader confirms
// a load (produce/manual_swap side), A/B active-pull state flip
// (paired-node runtime flip), and BinPickedUp arrival (SEND PARTIAL
// BACK pickup window).
package uop

import (
	"log"
	"runtime/debug"
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
	// epoch is the bin's load-lifecycle epoch (Core-authoritative).
	// Edge stamps it on every outgoing BinUOPDelta so Core's dedup
	// PK (station, scope_kind, scope_key, epoch) scopes replay-
	// protection per load instead of per bin identity. The value
	// flows in via recordBin's epoch arg — caller is responsible for
	// passing the bin-state cache's current epoch for the bin.
	epoch       int64
	windowStart time.Time
	windowEnd   time.Time

	// lastTouched is the wall-clock time of the most recent record or
	// successful flush commit. Read by evictIdle — entries with
	// delta==0 and lastTouched > 1h ago are removed from the
	// sync.Map so a long-tail of touched-once bin IDs doesn't grow
	// forever. A deleted entry rematerializes on the next recordBin call.
	lastTouched time.Time

	// evicted is set true (under mu) by evictIdle just before it removes
	// this entry from the map, and checked by recordBin under the same
	// lock so a record that raced the eviction retries against a fresh
	// entry instead of writing a delta into an orphaned object that the
	// map no longer references (the lost-update window — R68-1).
	evicted bool
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
	lastTouched  time.Time

	// evicted: see binDeltaEntry.evicted — same lost-update guard
	// (R68-1) for the bucket eviction path.
	evicted bool
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

	// recordHook, if non-nil, is invoked inside recordBin after
	// LoadOrStore returns the entry but before the entry lock is taken —
	// precisely the window a concurrent evictIdle can delete the entry.
	// Test-only seam (nil in production) used to drive the R68-1
	// lost-update race deterministically.
	recordHook func()

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
// recordBin accumulates a signed delta against a specific bin. epoch is
// the bin's current load-lifecycle epoch — caller threads it through
// from the bin-state cache (populated by Core's LoadBin response and
// the periodic bin-state refresh). A fresh entry stores the epoch on
// first touch; subsequent calls overwrite epoch when the caller
// presents a higher value (a lifecycle bump between two ticks rolls
// the entry's epoch forward so the next flush stamps the new value).
// Caller may pass 0 for pre-epoch wire compatibility — Core treats
// that as the pre-migration cohort.
func (r *accumulator) recordBin(binID int64, payloadCode string, delta int, reason protocol.BinUOPDeltaReason, epoch int64) {
	if delta == 0 {
		return
	}
	if binID <= 0 {
		return
	}
	key := strconv.FormatInt(binID, 10)
	now := r.clock()

	for {
		v, _ := r.bins.LoadOrStore(key, &binDeltaEntry{
			binID:       binID,
			payloadCode: payloadCode,
			epoch:       epoch,
			windowStart: now,
		})
		e := v.(*binDeltaEntry)

		if r.recordHook != nil {
			r.recordHook()
		}

		e.mu.Lock()
		if e.evicted {
			// This entry raced evictIdle and is being removed from the
			// map; writing our delta into it would lose it. Clear the
			// stale pointer (no-op if eviction already deleted it) and
			// retry so LoadOrStore stores a fresh entry. R68-1.
			e.mu.Unlock()
			r.bins.CompareAndDelete(key, e)
			continue
		}
		if e.delta == 0 {
			// First contribution to this window — anchor the start.
			e.windowStart = now
			e.payloadCode = payloadCode
			e.epoch = epoch
		} else if epoch > e.epoch {
			// Mid-window lifecycle bump. The accumulator currently coalesces
			// the older epoch's residual delta into the next flush under
			// the newer epoch — close enough since Edge's tick attribution
			// only resolves to "the active bin at tick time" and the older
			// epoch's deltas at this point are noise (the bin transitioned
			// to a different load-life before they shipped).
			e.epoch = epoch
		}
		e.delta += delta
		e.reason = reason
		e.windowEnd = now
		e.lastTouched = now
		e.mu.Unlock()
		break
	}

	r.debugLog.Log("inventory_delta: bin=%d delta=%+d reason=%s payload=%q epoch=%d",
		binID, delta, reason, payloadCode, epoch)
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

	for {
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
		if e.evicted {
			// Raced evictIdle — retry against a fresh entry. R68-1.
			e.mu.Unlock()
			r.buckets.CompareAndDelete(key, e)
			continue
		}
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
		e.lastTouched = now
		e.mu.Unlock()
		break
	}

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
	// Evict entries idle for over an hour. Cheap (one extra Range,
	// per-entry mu acquire and a comparison) and runs at the flush
	// cadence, not the record cadence, so no impact on the hot
	// path. Bounded by flush interval — if eviction would be
	// expensive on a particular tick the next tick picks up where
	// this one left off (Delete-during-Range is incremental).
	r.evictIdle(time.Hour)

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

// flushBins ranges across the bin accumulator and ships each entry's
// running delta using send-then-sweep: snapshot the entry's state
// under e.mu (no mutation), do the DB work without the lock held,
// commit the subtraction only on enqueue success. On any failure
// or panic the entry is unchanged and the next flush picks up the
// same delta plus any concurrent additions as one batch.
//
// Per-entry panic boundary: a defer recover() inside the Range
// callback logs and continues to the next entry without mutating
// anything. The loop survives; nothing is lost (we never zeroed
// anything in the first place).
func (r *accumulator) flushBins() {
	r.bins.Range(func(key, value any) bool {
		k := key.(string)
		e := value.(*binDeltaEntry)

		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC uop-accumulator-loop flushBins-callback bin=%s: %v\n%s",
					k, rec, debug.Stack())
				// Nothing to restore — send-then-sweep never mutates e
				// until the commit step, and commit only runs on
				// successful enqueue. A panic anywhere before commit
				// leaves the entry intact.
			}
		}()

		// SNAPSHOT under e.mu. Do not mutate e.
		e.mu.Lock()
		if e.delta == 0 {
			e.mu.Unlock()
			return true
		}
		sBinID := e.binID
		sDelta := e.delta
		sPayloadCode := e.payloadCode
		sReason := e.reason
		sEpoch := e.epoch
		sWindowStart := e.windowStart
		sWindowEnd := e.windowEnd
		e.mu.Unlock()

		// SEND with no lock held. Concurrent recordBin keeps adding
		// to e.delta; that's fine — those additions become part of
		// the next batch. Seq is keyed per-(scope_kind, scope_key,
		// epoch) so a new bin life starts the seq counter fresh
		// even though scope_key is unchanged.
		seq, err := r.db.AllocateInventoryDeltaSeq(invDeltaScopeBin, k, sEpoch)
		if err != nil {
			log.Printf("uop accumulator: allocate bin seq key=%s epoch=%d: %v", k, sEpoch, err)
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
				Epoch:       sEpoch,
				WindowStart: sWindowStart,
				WindowEnd:   sWindowEnd,
			},
		)
		if encErr != nil {
			log.Printf("uop accumulator: build bin envelope key=%s: %v", k, encErr)
			return true
		}
		data, encErr := env.Encode()
		if encErr != nil {
			log.Printf("uop accumulator: encode bin envelope key=%s: %v", k, encErr)
			return true
		}
		if _, err := r.db.EnqueueOutbox(data, protocol.SubjectBinUOPDelta); err != nil {
			r.flushFailures.Add(1)
			log.Printf("ERROR: uop accumulator: enqueue bin envelope key=%s: %v (entry intact, next flush retries)", k, err)
			return true
		}

		// COMMIT: subtract the snapshot from the live entry. Concurrent
		// recordBin calls during the send may have added to e.delta;
		// the subtraction leaves only those new contributions for the
		// next flush.
		e.mu.Lock()
		e.delta -= sDelta
		e.lastTouched = time.Now().UTC()
		if e.delta == 0 {
			// No concurrent records during send — full reset.
			e.reason = ""
			e.windowStart = time.Time{}
			e.windowEnd = time.Time{}
		} else {
			// Concurrent records arrived. They occurred at times
			// strictly after sWindowEnd (we captured sWindowEnd from
			// e.windowEnd at snapshot time, and recordBin only ever
			// advances windowEnd forward). Set windowStart to
			// sWindowEnd so the new batch's leading-edge metadata is
			// correct rather than carrying the original batch's
			// stale windowStart forward.
			e.windowStart = sWindowEnd
			// windowEnd is naturally the latest concurrent record's
			// time; leave it alone. reason and payloadCode also stay
			// as recordBin set them.
		}
		e.mu.Unlock()

		r.debugLog.Log("uop accumulator: flushed bin=%d delta=%+d seq=%d epoch=%d reason=%s",
			sBinID, sDelta, seq, sEpoch, sReason)
		return true
	})
}

// flushBuckets is the bucket-side mirror of flushBins. Same
// send-then-sweep shape; see flushBins above for the design rationale.
func (r *accumulator) flushBuckets() {
	r.buckets.Range(func(key, value any) bool {
		k := key.(string)
		e := value.(*bucketDeltaEntry)

		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC uop-accumulator-loop flushBuckets-callback bucket=%s: %v\n%s",
					k, rec, debug.Stack())
			}
		}()

		// SNAPSHOT.
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
			// source. Note: this is a permanent drop (we don't
			// retry on the next flush) because the underlying state
			// won't change. Bin delta loss is bounded by the entry's
			// own delta value.
			log.Printf("ERROR: uop accumulator: drop bucket delta key=%s — no core_node_name resolvable for nodeID=%d (process_node row missing?)",
				k, sNodeID)
			r.flushFailures.Add(1)
			// Commit a full reset to clear the unsendable delta.
			e.mu.Lock()
			e.delta -= sDelta
			if e.delta == 0 {
				e.reason = ""
				e.windowStart = time.Time{}
				e.windowEnd = time.Time{}
			} else {
				e.windowStart = sWindowEnd
			}
			e.mu.Unlock()
			return true
		}

		// SEND. Bucket scope stays on epoch=0 — see the matching note
		// in shingo-core/uop/applier.go's ApplyLinesideBucketDelta.
		seq, err := r.db.AllocateInventoryDeltaSeq(invDeltaScopeBucket, k, 0)
		if err != nil {
			log.Printf("uop accumulator: allocate bucket seq key=%s: %v", k, err)
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
			return true
		}
		data, encErr := env.Encode()
		if encErr != nil {
			log.Printf("uop accumulator: encode bucket envelope key=%s: %v", k, encErr)
			return true
		}
		if _, err := r.db.EnqueueOutbox(data, protocol.SubjectLinesideBucketDelta); err != nil {
			r.flushFailures.Add(1)
			log.Printf("ERROR: uop accumulator: enqueue bucket envelope key=%s: %v (entry intact, next flush retries)", k, err)
			return true
		}

		// COMMIT.
		e.mu.Lock()
		e.delta -= sDelta
		e.lastTouched = time.Now().UTC()
		if e.delta == 0 {
			e.reason = ""
			e.windowStart = time.Time{}
			e.windowEnd = time.Time{}
		} else {
			e.windowStart = sWindowEnd
		}
		e.mu.Unlock()

		r.debugLog.Log("uop accumulator: flushed bucket node=%d part=%q delta=%+d seq=%d reason=%s",
			sNodeID, sPartNumber, sDelta, seq, sReason)
		return true
	})
}

// evictIdle removes entries from the bins/buckets sync.Maps whose
// delta is zero and whose last touch was more than maxIdle ago.
// Bounded slow-leak prevention: without eviction every bin or
// bucket key ever recorded would keep its entry forever, growing
// the maps multi-week. Deletion is harmless — LoadOrStore
// recreates the entry on the next record call. Called by flush()
// after the bin/bucket flush passes complete.
//
// Safe to call from the flush goroutine because Range's contract
// allows concurrent Delete by the same goroutine. The per-entry
// lock guards the delta+lastTouched read so a Delete races a
// concurrent recordBin only on the lock acquisition — and the
// post-delete LoadOrStore is the documented way to handle that.
func (r *accumulator) evictIdle(maxIdle time.Duration) {
	cutoff := time.Now().UTC().Add(-maxIdle)
	r.bins.Range(func(key, value any) bool {
		e := value.(*binDeltaEntry)
		e.mu.Lock()
		idle := e.delta == 0 && !e.lastTouched.IsZero() && e.lastTouched.Before(cutoff)
		if idle {
			// Mark before unlocking so a recordBin that acquires the
			// lock next sees the eviction and retries against a fresh
			// entry (R68-1). CompareAndDelete only removes this exact
			// pointer, so a fresh entry a retrying recordBin may have
			// already stored under the same key is left intact.
			e.evicted = true
		}
		e.mu.Unlock()
		if idle {
			r.bins.CompareAndDelete(key, e)
		}
		return true
	})
	r.buckets.Range(func(key, value any) bool {
		e := value.(*bucketDeltaEntry)
		e.mu.Lock()
		idle := e.delta == 0 && !e.lastTouched.IsZero() && e.lastTouched.Before(cutoff)
		if idle {
			e.evicted = true
		}
		e.mu.Unlock()
		if idle {
			r.buckets.CompareAndDelete(key, e)
		}
		return true
	})
}

// Note: there are no restore-on-failure helpers. Send-then-sweep
// never mutates the entry until enqueue success, so there is
// nothing to restore on failure — the entry is already in the
// correct state.

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
