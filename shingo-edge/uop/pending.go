package uop

import "strconv"

// pending.go — what the accumulator holds that no flushed net carries yet, for
// the lineside report.
//
// The report states each seat's count AS OF the carrier's FlushedSeq: the
// runtime count minus the counts recorded here and not yet in the scope's net.
// A tick moves the runtime row at once and reaches the outbox up to one flush
// interval later; subtracting what is still here makes the reported count the
// one Core will hold once it has applied every seq up to FlushedSeq, with no
// flush and no statement added.
//
// CONSISTENCY. WithPending takes this snapshot and runs the caller under
// flushMu, the lock every flush holds from its seq allocation to its in-memory
// commit. So no flush lands between the caller's read (FlushedSeq, from
// inventory_delta_seq) and the snapshot (what is not yet flushed) — a count is
// never subtracted twice or not at all across a flush — nor between the read
// and the caller's own outbox enqueue, so every delta flushed before the
// snapshot is ahead of the report in the outbox and every later one behind it.
// The other writer is the record path, which does not take flushMu; the engine
// closes that side by holding its own count lock across each tick's database
// write and record, and across this call (engine.Engine.countMu).

// Pending is the accumulator's unflushed counts at one instant.
type Pending struct {
	bins    map[int64]pendingBin
	buckets map[pendingBucketKey]int
}

type pendingBin struct {
	epoch int64
	delta int
}

type pendingBucketKey struct {
	nodeID  int64
	payload string
}

// Bin is the carrier's count recorded under this generation and not yet in its
// scope's net. A pending entry of another generation holds counts the
// runtime's current count does not include, so it contributes nothing.
func (p Pending) Bin(binID, epoch int64) int {
	b, ok := p.bins[binID]
	if !ok || b.epoch != epoch {
		return 0
	}
	return b.delta
}

// Bucket is the unflushed bucket delta at a node for a part, summed over the
// node's bucket scopes (pair and style).
func (p Pending) Bucket(nodeID int64, payload string) int {
	return p.buckets[pendingBucketKey{nodeID: nodeID, payload: payload}]
}

// WithPending snapshots the unflushed counts and runs fn with them, both under
// flushMu, so fn's reads and writes see the same flushes as the snapshot. No
// statement is issued here beyond what fn issues. fn runs with the flush lock
// held: keep it to the report's one SELECT and one enqueue.
func (m *Mutator) WithPending(fn func(Pending) error) error {
	m.acc.flushMu.Lock()
	defer m.acc.flushMu.Unlock()
	return fn(m.acc.pending())
}

// pending snapshots every entry's unflushed part. The caller holds flushMu.
// What a failed enqueue left in the net (netted) is already in the scope's
// net, so it is not pending.
func (r *accumulator) pending() Pending {
	p := Pending{bins: map[int64]pendingBin{}, buckets: map[pendingBucketKey]int{}}
	r.bins.Range(func(key, value any) bool {
		e := value.(*binDeltaEntry)
		e.mu.Lock()
		d := e.delta
		if e.netted != 0 && e.nettedEpoch == e.epoch {
			d -= e.netted
		}
		binID, epoch := e.binID, e.epoch
		e.mu.Unlock()
		if d != 0 {
			if binID == 0 {
				binID, _ = strconv.ParseInt(key.(string), 10, 64)
			}
			p.bins[binID] = pendingBin{epoch: epoch, delta: d}
		}
		return true
	})
	r.buckets.Range(func(_, value any) bool {
		e := value.(*bucketDeltaEntry)
		e.mu.Lock()
		d := e.delta - e.netted
		k := pendingBucketKey{nodeID: e.nodeID, payload: e.payloadCode}
		e.mu.Unlock()
		if d != 0 {
			p.buckets[k] += d
		}
		return true
	})
	return p
}
