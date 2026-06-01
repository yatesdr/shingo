// tick.go — PLC tick path delta emission verbs.
//
// The three tick verbs (Consumed, Produced, Fallthrough) wrap the
// RecordBin / RecordBucket calls that the engine's wiring_counter_delta.go
// path emits today. Each verb locks in its reason taxonomy:
//
//   - Consumed: consume_drain (bucket) + consume_tick (bin)
//   - Produced: produce_tick (bin only — produce nodes don't drain lineside)
//   - Fallthrough: consume_drain (bucket) + ab_fallthrough (bin)
//
// Verbs do not own the hold-and-replay handling, lineside drain,
// payloadCode resolution, or cache write. Engine continues to compute
// drains via drainLinesideFirst, resolves (binID, payloadCode) via
// binAtNode, holds ticks in pending_uop_delta when no bin is bound and
// replays them on bind, and runs the auto-reorder / auto-relief
// threshold checks against the post-tick cache value.
//
// Why this shape: the value-add at the verb boundary is the reason
// taxonomy lock-in (one verb invocation maps to one set of reasons —
// no risk of a future caller picking the wrong reason). Folding the
// hold-and-replay handling and lineside drain into the verb requires
// more dependency surface (runtimeReader, bucket store drain methods)
// and belongs in a follow-up that audits the runtime cache write path
// end-to-end.
package uop

import "shingo/protocol"

// LinesideDrain describes the result of decrementing one lineside
// bucket on a consume tick: how many UOP came out and which style the
// matched bucket carries. Round-3 A* dropped style_id from the Drain
// WHERE clause, so the bucket's style may differ from the caller's
// claim.StyleID (cutover window — bucket captured under style A
// drains while the process is now running style B). Carrying the
// matched StyleID through the wire envelope keeps Core's dedup
// scope_key (<NodeID>|<PairKey>|<StyleID>|<PartNumber>) consistent
// with what Core has on file for that bucket.
type LinesideDrain struct {
	Qty     int
	StyleID int64
}

// TickEvent carries the resolved tick context to the emission verbs.
// Engine populates it from the existing per-tick state.
type TickEvent struct {
	NodeID  int64
	StyleID int64
	PairKey string

	// CoreNodeName is the cross-system identifier the wire envelope
	// uses (Round-3 Obs 8). Engine resolves this from the process
	// node row that drives the tick — see emitConsumeTickDeltas.
	// Bucket deltas with an empty CoreNodeName are dropped at flush
	// rather than emitted into the (now Core-rejected) cross-namespace
	// translation hole.
	CoreNodeName string

	// BinID + PayloadCode resolved via engine.binAtNode. BinID == 0
	// when no bin is at the slot (gap window with active_bin_id nil);
	// in that case the bin delta is skipped — the bucket drain still
	// emits because parts physically left lineside regardless.
	BinID       int64
	PayloadCode string

	// BinEpoch is the bin's load-lifecycle epoch as resolved against
	// Core (via FetchNodeBins on tick time, or — once we add a local
	// bin-state cache — read from there). Threaded through to
	// recordBin so the outgoing BinUOPDelta carries the right
	// generation. Zero is the pre-migration / unknown sentinel; Core
	// applies it against the pre-migration cohort.
	BinEpoch int64

	// Drains is the per-part lineside-bucket drain map computed by
	// engine.drainLinesideFirst. Each non-zero entry emits one
	// consume_drain bucket delta (signed -qty) keyed on the matched
	// bucket's StyleID rather than the caller's claim.StyleID.
	Drains map[string]LinesideDrain

	// BinRemainder is the portion of the tick delta that flows to the
	// bin counter after lineside drain. Emits one bin delta when > 0.
	BinRemainder int
}

// Consumed emits the per-tick bucket drain + bin consumption deltas
// for one active-pull consume node. Today's caller is
// emitConsumeTickDeltas at wiring_counter_delta.go:247. Bucket deltas
// use ReasonConsumeDrain; bin delta uses ReasonConsumeTick.
//
// Both signs are negative — parts left the slot. Bucket drains are
// always negative qty (operator pulled parts away); bin remainder is
// also negative (cell consumed parts from the bin).
//
// Skips the bin delta when BinID == 0 (no bin at node — gap window
// with active_bin_id nil). The bucket drain still emits because the
// physical lineside change is independent of which bin is at the
// slot.
func (m *Mutator) Consumed(ev TickEvent) error {
	for part, d := range ev.Drains {
		if d.Qty > 0 {
			styleID := d.StyleID
			if styleID == 0 {
				// Defensive: pre-A* tests / fixtures may populate
				// Drains without a per-part style. Fall back to the
				// tick's own StyleID so the dedup key stays scoped to
				// something meaningful instead of zero.
				styleID = ev.StyleID
			}
			m.acc.recordBucket(ev.NodeID, ev.CoreNodeName, ev.PairKey, styleID, part, ev.PayloadCode, -d.Qty, protocol.ReasonConsumeDrain)
		}
	}
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		m.acc.recordBin(ev.BinID, ev.PayloadCode, -ev.BinRemainder, protocol.ReasonConsumeTick, ev.BinEpoch)
	}
	return nil
}

// Produced emits the per-tick bin production delta. Today's caller is
// handleProduceTick at wiring_counter_delta.go:188. Bin delta uses
// ReasonProduceTick with a positive sign (parts added to the bin).
//
// Produce nodes don't drain lineside — produce_tick is the only
// emission shape. Drains and BinRemainder are unused for this verb;
// the engine passes them as 0/nil by convention. (Kept on TickEvent
// for type symmetry; could split into a separate ProduceEvent later
// if the shape diverges further.)
//
// Skips emission when BinID == 0 (no bin at node) or BinRemainder
// (the positive delta, passed via BinRemainder for symmetry) is 0.
func (m *Mutator) Produced(ev TickEvent) error {
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		m.acc.recordBin(ev.BinID, ev.PayloadCode, ev.BinRemainder, protocol.ReasonProduceTick, ev.BinEpoch)
	}
	return nil
}

// Fallthrough emits the per-tick bucket drain + bin fallback deltas
// for the A/B fallback path (no active-pull consume node visible at
// tick time). Today's caller is emitFallthroughDeltas at
// wiring_counter_delta.go:268.
//
// Bucket deltas use ReasonConsumeDrain — physical bucket changes
// happen regardless of which side of the A/B pair attribution lands
// on. Bin delta uses ReasonABFallthrough — Core's dashboards
// distinguish this from ConsumeTick so the "no active pull node"
// condition surfaces in alerts.
//
// Note the double emission in one verb invocation — Risk 4 in the
// refactor plan called this out specifically. Reason-taxonomy
// preservation requires both reasons in the same logical operation.
func (m *Mutator) Fallthrough(ev TickEvent) error {
	for part, d := range ev.Drains {
		if d.Qty > 0 {
			styleID := d.StyleID
			if styleID == 0 {
				styleID = ev.StyleID
			}
			m.acc.recordBucket(ev.NodeID, ev.CoreNodeName, ev.PairKey, styleID, part, ev.PayloadCode, -d.Qty, protocol.ReasonConsumeDrain)
		}
	}
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		m.acc.recordBin(ev.BinID, ev.PayloadCode, -ev.BinRemainder, protocol.ReasonABFallthrough, ev.BinEpoch)
	}
	return nil
}
