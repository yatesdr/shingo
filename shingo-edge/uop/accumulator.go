// accumulator.go — bin-as-truth signed-delta accumulator, and the lineside
// pile levels.
//
// Internal implementation of UOP delta accumulation. The public surface
// is in mutator.go; this file owns the per-scope sync.Map state, the
// periodic flush goroutine, and the outbox enqueue path.
//
// Bins accumulate signed deltas. Lineside piles do not: a pile write marks its
// (core node, payload, state) dirty, and the flush reads that row's level from
// the database and sends it (flushBuckets). Reading at flush, rather than
// carrying a qty from the write, is what keeps the level right when a capture
// on the HTTP goroutine races a drain on the poll goroutine, and when two local
// nodes share one core name.
//
// Concurrency: sync.Map for the per-scope accumulator with a
// per-entry sync.Mutex protecting the composite metadata. Hot path
// is recordBin / markBucket; flush goroutine ranges across the map
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
	"fmt"
	"log"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/store"
)

const (
	// invDeltaScopeBin / invDeltaScopeBucketLevel — scope_kind values used
	// when allocating sequence-ids and when Core guards order. Renames must
	// come with a coordinated migration on both sides, which is why the
	// strings are single-sourced in protocol/ rather than spelled here
	// and again in shingo-core/uop.
	invDeltaScopeBin         = protocol.InvDeltaScopeBin
	invDeltaScopeBucketLevel = protocol.InvDeltaScopeBucketLevel

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

	// netted is the part of delta the scope's durable net ALREADY holds: a
	// flush whose seq UPSERT (which adds to the net) succeeded and whose outbox
	// INSERT then failed. The retry adds only delta - netted, or the net would
	// count that window twice and Core would apply it twice. nettedEpoch is the
	// scope row it went into; a different epoch is a different row, which
	// holds none of it.
	netted      int
	nettedEpoch int64
}

// bucketLevelEntry is one pile level the Edge owes Core: a dirty flag and the
// drains of the flush window, keyed by (core node, payload, state). It holds
// no qty of its own; the flush reads the level (see flushBuckets).
type bucketLevelEntry struct {
	mu sync.Mutex
	// nodeID is a local process node carrying the key, kept so a flush can
	// resolve a core_node_name the mark did not carry.
	nodeID       int64
	coreNodeName string
	payloadCode  string
	state        protocol.LinesideBucketState

	// marks counts every write to the key; sentMarks is marks as of the
	// last level enqueued. The key is dirty while they differ, so a write
	// that lands while a flush is sending stays dirty for the next one.
	marks     uint64
	sentMarks uint64
	// drained is the consume drains recorded since the last level enqueued,
	// for Core's drain ledger (LinesideBucketLevel.Drained).
	drained     int
	lastTouched time.Time

	// sent / sentQty are the last level enqueued for the key: what Core
	// holds once it has applied everything ahead of it in the outbox. The
	// lineside report states the bucket as that (Pending.Bucket).
	sent    bool
	sentQty int

	// evicted: see binDeltaEntry.evicted — same lost-update guard
	// (R68-1) for the bucket eviction path.
	evicted bool
}

// accumulator accumulates BinUOPDelta count changes and dirty lineside pile
// levels, and flushes them to the outbox on a periodic cadence.
// Package-private — exposed through Mutator (see mutator.go).
type accumulator struct {
	db        *store.DB
	stationID string
	interval  time.Duration

	// Two sync.Maps. Keys are stable strings:
	//   bin entry:    strconv(BinID), which is also the bin seq scope key
	//   bucket entry: bucketLevelKey(core_node_name, payload, state), which
	//                 is also the level's seq scope key; "#<node id>" stands
	//                 in for a core name the mark did not carry.
	bins    sync.Map // map[string]*binDeltaEntry
	buckets sync.Map // map[string]*bucketLevelEntry

	// flushMu serializes flush passes against each other (stop's final
	// flush must not race with the periodic loop). recordBin /
	// markBucket do not take this lock.
	flushMu sync.Mutex

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
// from the bin-state cache (populated by Core's LoadBin response, by the
// announcement Core sends whenever a generation changes, and by Core's reply
// when a count is discarded for carrying an old one). A fresh entry stores the
// epoch on
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

// markBucket records that a write changed the pile row(s) under (core node,
// payload, state), so the next flush sends that key's level. drained is the
// consume drain the write took (0 for anything but a drain), summed into the
// window for Core's drain ledger. Every writer of node_lineside_bucket calls
// this after its write, including a delete (the level it sends is then 0).
//
// coreNodeName may be empty when the caller could not resolve it; the flush
// resolves it from nodeID. The mark never reads the database.
func (r *accumulator) markBucket(nodeID int64, coreNodeName, payloadCode string, state protocol.LinesideBucketState, drained int) {
	if payloadCode == "" || (coreNodeName == "" && nodeID <= 0) {
		return
	}
	key := bucketLevelKey(coreNodeName, nodeID, payloadCode, state)
	now := r.clock()

	for {
		v, _ := r.buckets.LoadOrStore(key, &bucketLevelEntry{
			nodeID:       nodeID,
			coreNodeName: coreNodeName,
			payloadCode:  payloadCode,
			state:        state,
		})
		e := v.(*bucketLevelEntry)

		e.mu.Lock()
		if e.evicted {
			// Raced evictIdle — retry against a fresh entry. R68-1.
			e.mu.Unlock()
			r.buckets.CompareAndDelete(key, e)
			continue
		}
		e.marks++
		e.drained += drained
		e.lastTouched = now
		e.mu.Unlock()
		break
	}

	r.debugLog.Log("inventory_delta: pile dirty node=%d core=%q payload=%q state=%s drained=%d",
		nodeID, coreNodeName, payloadCode, state, drained)
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
	ticker := clock.Default().NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C():
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
		netAdd := sDelta
		if e.netted != 0 && e.nettedEpoch == sEpoch {
			netAdd = sDelta - e.netted
		}
		e.mu.Unlock()

		// SEND with no lock held. Concurrent recordBin keeps adding
		// to e.delta; that's fine — those additions become part of
		// the next batch. Seq is keyed per-(scope_kind, scope_key,
		// epoch) so a new bin life starts the seq counter fresh
		// even though scope_key is unchanged. The same statement adds
		// the window to the scope's running net and returns it.
		seq, net, err := r.db.AllocateInventoryDeltaSeq(invDeltaScopeBin, k, sEpoch, int64(netAdd))
		if err != nil {
			log.Printf("uop accumulator: allocate bin seq key=%s epoch=%d: %v", k, sEpoch, err)
			return true
		}
		// From here to the enqueue, the net holds this window. Any failure
		// below leaves the entry's delta for the next flush, which must not
		// add it to the net again.
		markNetted := func() {
			e.mu.Lock()
			e.netted, e.nettedEpoch = sDelta, sEpoch
			e.mu.Unlock()
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
				Net:         &net,
			},
		)
		if encErr != nil {
			markNetted()
			log.Printf("uop accumulator: build bin envelope key=%s: %v", k, encErr)
			return true
		}
		data, encErr := env.Encode()
		if encErr != nil {
			markNetted()
			log.Printf("uop accumulator: encode bin envelope key=%s: %v", k, encErr)
			return true
		}
		if _, err := r.db.EnqueueOutbox(data, protocol.SubjectBinUOPDelta); err != nil {
			markNetted()
			log.Printf("ERROR: uop accumulator: enqueue bin envelope key=%s: %v (entry intact, next flush retries)", k, err)
			return true
		}

		// COMMIT: subtract the snapshot from the live entry. Concurrent
		// recordBin calls during the send may have added to e.delta;
		// the subtraction leaves only those new contributions for the
		// next flush. The net now holds exactly what has been enqueued.
		e.mu.Lock()
		e.delta -= sDelta
		e.netted = 0
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

// flushBuckets sends one LinesideBucketLevel per dirty pile key. Same
// send-then-sweep shape as flushBins (nothing is cleared until the enqueue
// succeeds), with one difference: the qty is not carried by the entry but READ
// here, as the sum over every process node with the key's core name
// (lineside.Level). One read per dirty key per flush; a clean key costs
// nothing.
func (r *accumulator) flushBuckets() {
	windowEnd := r.clock()
	r.buckets.Range(func(key, value any) bool {
		k := key.(string)
		e := value.(*bucketLevelEntry)

		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC uop-accumulator-loop flushBuckets-callback bucket=%s: %v\n%s",
					k, rec, debug.Stack())
			}
		}()

		// SNAPSHOT.
		e.mu.Lock()
		if e.marks == e.sentMarks {
			e.mu.Unlock()
			return true
		}
		sMarks, sDrained := e.marks, e.drained
		sNodeID, sCoreNodeName := e.nodeID, e.coreNodeName
		sPayloadCode, sState := e.payloadCode, e.state
		e.mu.Unlock()

		if sCoreNodeName == "" {
			if node, lookupErr := r.db.GetProcessNode(sNodeID); lookupErr == nil && node != nil {
				sCoreNodeName = node.CoreNodeName
			}
		}
		if sCoreNodeName == "" {
			// Core keys a pile by core_node_name and would refuse a level
			// without one. The row it names cannot become resolvable later
			// (the process node is gone or unnamed), so the mark is dropped
			// loudly instead of retried every flush.
			log.Printf("ERROR: uop accumulator: drop pile level key=%s — no core_node_name resolvable for nodeID=%d (process_node row missing?)",
				k, sNodeID)
			e.mu.Lock()
			e.sentMarks = sMarks
			e.drained -= sDrained
			e.mu.Unlock()
			return true
		}

		// SEND. The level is read after the snapshot, so it includes at
		// least every write the snapshot's marks counted.
		level, err := r.db.LinesidePileLevel(sCoreNodeName, sPayloadCode, string(sState))
		if err != nil {
			log.Printf("uop accumulator: read pile level key=%s: %v", k, err)
			return true
		}
		scope := bucketLevelKey(sCoreNodeName, sNodeID, sPayloadCode, sState)
		seq, err := r.db.AllocateInventoryLevelSeq(invDeltaScopeBucketLevel, scope)
		if err != nil {
			log.Printf("uop accumulator: allocate pile level seq key=%s: %v", scope, err)
			return true
		}
		if err := r.enqueueBucketLevel(&protocol.LinesideBucketLevel{
			CoreNodeName: sCoreNodeName,
			PayloadCode:  sPayloadCode,
			State:        sState,
			Qty:          level,
			Drained:      sDrained,
			SequenceID:   seq,
			WindowEnd:    windowEnd,
		}); err != nil {
			log.Printf("ERROR: uop accumulator: enqueue pile level key=%s: %v (entry intact, next flush retries)", scope, err)
			return true
		}

		// COMMIT. A mark that arrived during the send leaves marks ahead of
		// sentMarks, so the key goes out again next flush.
		e.mu.Lock()
		e.sentMarks = sMarks
		e.drained -= sDrained
		e.sent, e.sentQty = true, level
		e.lastTouched = time.Now().UTC()
		e.mu.Unlock()

		r.debugLog.Log("uop accumulator: flushed pile level core=%q payload=%q state=%s qty=%d drained=%d seq=%d",
			sCoreNodeName, sPayloadCode, sState, level, sDrained, seq)
		return true
	})
}

// enqueueBucketLevel encodes one level and puts it on the outbox.
func (r *accumulator) enqueueBucketLevel(lvl *protocol.LinesideBucketLevel) error {
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectLinesideBucketLevel,
		protocol.Address{Role: protocol.RoleEdge, Station: r.stationID},
		protocol.Address{Role: protocol.RoleCore},
		lvl,
	)
	if err != nil {
		return fmt.Errorf("build envelope: %w", err)
	}
	data, err := env.Encode()
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	_, err = r.db.EnqueueOutbox(data, protocol.SubjectLinesideBucketLevel)
	return err
}

// evictIdle removes entries from the bins/buckets sync.Maps whose
// delta is zero (for a pile key: clean) and whose last touch was more than
// maxIdle ago. A bin entry whose netted is non-zero is kept: its records summed to zero after a
// failed enqueue, and the part the scope's net already holds must still be
// subtracted from the next window, which a fresh entry would not know.
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
		idle := e.delta == 0 && e.netted == 0 && !e.lastTouched.IsZero() && e.lastTouched.Before(cutoff)
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
		e := value.(*bucketLevelEntry)
		e.mu.Lock()
		idle := e.marks == e.sentMarks && e.drained == 0 && !e.lastTouched.IsZero() && e.lastTouched.Before(cutoff)
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

// bucketLevelKey is a pile level's key: "<core_node_name>|<payload>|<state>",
// which is also its seq scope_key (InvDeltaScopeBucketLevel), byte-identical
// to the key Core guards the level's order on. A mark with no core name keys
// under "#<local node id>" in its place; the flush resolves the name and sends
// under the real key.
//
// The pipe-delimited format is stable; a rename must come with a coordinated
// migration on both sides.
func bucketLevelKey(coreNodeName string, nodeID int64, payloadCode string, state protocol.LinesideBucketState) string {
	var sb strings.Builder
	if coreNodeName != "" {
		sb.WriteString(coreNodeName)
	} else {
		sb.WriteByte('#')
		sb.WriteString(strconv.FormatInt(nodeID, 10))
	}
	sb.WriteByte('|')
	sb.WriteString(payloadCode)
	sb.WriteByte('|')
	sb.WriteString(string(state))
	return sb.String()
}
