package uop

import (
	"strconv"

	"shingo/protocol"
)

// pending.go — what the accumulator holds that Core does not have yet, for the
// lineside report.
//
// The report states each seat's count AS OF the carrier's FlushedSeq: the
// runtime count minus the counts recorded here and not yet in the scope's net.
// A tick moves the runtime row at once and reaches the outbox up to one flush
// interval later; subtracting what is still here makes the reported count the
// one Core will hold once it has applied every seq up to FlushedSeq, with no
// flush and no statement added.
//
// The bucket term is a level, not a count: it is stated as the last level
// enqueued for the seat's active pile of the payload, which is what Core holds
// once it has applied everything ahead of the report in the outbox. A write
// the flush has not sent yet therefore never reads as a divergence.
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

// Pending is the accumulator's unflushed counts, and the pile levels it has
// sent, at one instant.
type Pending struct {
	bins    map[int64]pendingBin
	buckets map[pendingBucketKey]int
}

type pendingBin struct {
	epoch int64
	delta int
}

type pendingBucketKey struct {
	coreNodeName string
	payload      string
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

// Bucket is the active pile level Core holds for (core node, payload) as of the
// outbox: the last level enqueued, or 0 for a key the accumulator is tracking
// but has not sent since boot (Core has nothing from this Edge for it yet). ok
// is false when the accumulator has no entry for the key, and the caller's own
// read of the table is then the level (nothing has changed it unsent; the boot
// resend marks every row).
func (p Pending) Bucket(coreNodeName, payload string) (qty int, ok bool) {
	qty, ok = p.buckets[pendingBucketKey{coreNodeName: coreNodeName, payload: payload}]
	return qty, ok
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

// pending snapshots every bin entry's unflushed part and every active pile
// key's sent level. The caller holds flushMu. What a failed enqueue left in a
// bin's net (netted) is already in the scope's net, so it is not pending.
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
		e := value.(*bucketLevelEntry)
		e.mu.Lock()
		active, core := e.state == protocol.LinesideBucketActive, e.coreNodeName
		k := pendingBucketKey{coreNodeName: core, payload: e.payloadCode}
		qty := e.sentQty // 0 while never sent
		e.mu.Unlock()
		if active && core != "" {
			p.buckets[k] = qty
		}
		return true
	})
	return p
}
