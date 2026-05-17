// backfill.go — bucket backfill for fresh Core deployments.
//
// One-shot backfill that seeds Core's authoritative lineside_buckets
// table from Edge's local node_lineside_bucket state on first boot
// against a fresh Core.
//
// Why only buckets? bins.uop_remaining on Core has been authoritative
// for as long as bins have existed — every release / delivery /
// cycle-count path writes it via BinManifestService. Buckets are
// different: lineside_buckets is the authoritative table for the
// per-station per-part counts and starts empty on a fresh Core;
// backfill seeds it from Edge's local state so the delta-apply path
// has real state to do capture/drain math against.
//
// Online-safe: the accumulator's flush path serializes via outbox;
// concurrent ticks continue emitting deltas during backfill.
// Sequence-id allocation per-scope means a tick that races a backfill
// row gets its own SequenceID and Core's dedup table picks the higher
// value. Net effect: at-least-once delivery of the seed value,
// idempotent because the seed is always "the qty as observed at this
// moment."
//
// Moved from engine/uop_backfill.go in Phase 3a.
package uop

import (
	"fmt"
	"log"

	"shingo/protocol"
)

// Backfill walks every local node_lineside_bucket row this station
// knows about and emits a LinesideBucketDelta(capture_fill, +qty)
// per row. Returns the count of deltas recorded.
//
// The deltas land in the accumulator and ship via the next flush.
// Caller can pass force=true to flush synchronously before returning
// — useful for the typical "flip the flag right after backfill"
// sequence so Core's authoritative table has the seed before delta
// apply switches target.
//
// Errors during the per-node bucket fetch are logged and the
// backfill continues with the remaining nodes (best-effort
// completeness over fail-fast).
func (m *Mutator) Backfill(force bool) (int, error) {
	if m.nodes == nil || m.buckets == nil {
		return 0, fmt.Errorf("uop mutator: backfill requires node and bucket store deps")
	}
	nodes, err := m.nodes.ListProcessNodes()
	if err != nil {
		return 0, fmt.Errorf("list process nodes: %w", err)
	}

	emitted := 0
	for _, n := range nodes {
		buckets, err := m.buckets.ListLinesideBuckets(n.ID)
		if err != nil {
			log.Printf("uop backfill: list buckets node=%d: %v", n.ID, err)
			continue
		}
		for _, b := range buckets {
			if b.Qty <= 0 {
				continue
			}
			// Existing uop_backfill ships the bucket's current qty as a
			// capture_fill — Core hasn't seen this bucket yet. Payload
			// code isn't on the local Bucket row (lineside_buckets row
			// is keyed on node + style + part_number only) so we ship
			// empty here. Core's UPSERT keeps any previously-latched
			// payload_code; going-forward capture_fill events from
			// capture.go carry the resolved code from the order
			// context, so the row self-heals on the first real delta.
			m.acc.recordBucket(b.NodeID, b.PairKey, b.StyleID, b.PartNumber, "",
				b.Qty, protocol.ReasonCaptureFill)
			emitted++
		}
	}

	if force {
		m.acc.flush()
	}
	log.Printf("uop backfill: emitted %d bucket seed deltas", emitted)
	return emitted, nil
}
