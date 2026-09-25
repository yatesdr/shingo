// tick.go — PLC tick path emission verbs.
//
// The three tick verbs (Consumed, Produced, Fallthrough) wrap what the
// engine's wiring_counter_delta.go path records per tick. Each verb locks in
// its reason taxonomy:
//
//   - Consumed: a pile drain (level dirty + Drained) + consume_tick (bin)
//   - Produced: produce_tick (bin only — produce nodes don't drain lineside)
//   - Fallthrough: a pile drain + ab_fallthrough (bin)
//
// Verbs do not own the hold-and-replay handling, lineside drain,
// payloadCode resolution, or cache write. Engine computes the drains via
// drainLinesideFirst (which has already written them to the pile rows),
// resolves (binID, payloadCode) via binAtNode, holds ticks in
// pending_uop_delta when no bin is bound and replays them on bind.
//
// A drain is consumption; the pile's level goes out on the next flush with the
// window's drains in Drained, for Core's drain ledger.
package uop

import "shingo/protocol"

// TickEvent carries the resolved tick context to the emission verbs.
// Engine populates it from the existing per-tick state.
type TickEvent struct {
	NodeID int64

	// CoreNodeName is the cross-system identifier a pile level is keyed by
	// at Core. Engine resolves this from the process node row that drives
	// the tick — see emitConsumeTickDeltas.
	CoreNodeName string

	// BinID + PayloadCode resolved via engine.binAtNode. BinID == 0
	// when no bin is at the slot (gap window with active_bin_id nil);
	// in that case the bin delta is skipped — the pile drain still
	// goes out because parts physically left lineside regardless.
	BinID       int64
	PayloadCode string

	// BinEpoch is the bin's load-lifecycle epoch. Threaded through to
	// recordBin so the outgoing BinUOPDelta carries the right generation.
	// Zero is the pre-migration / unknown sentinel.
	BinEpoch int64

	// Drains is the per-part qty engine.drainLinesideFirst took from the
	// node's active piles this tick. Each non-zero entry marks that pile's
	// level dirty with the drain.
	Drains map[string]int

	// BinRemainder is the portion of the tick delta that flows to the
	// bin counter after lineside drain. Emits one bin delta when > 0.
	BinRemainder int
}

// markDrains marks each drained pile's level dirty with its drain.
func (m *Mutator) markDrains(ev TickEvent) {
	for part, qty := range ev.Drains {
		if qty > 0 {
			m.acc.markBucket(ev.NodeID, ev.CoreNodeName, part, protocol.LinesideBucketActive, qty)
		}
	}
}

// Consumed records the per-tick pile drains + bin consumption for one
// active-pull consume node. Today's caller is emitConsumeTickDeltas in
// wiring_counter_delta.go. The bin delta uses ReasonConsumeTick, negative
// (the cell consumed parts from the bin).
//
// Skips the bin delta when BinID == 0 (no bin at node — gap window with
// active_bin_id nil). The pile drains still go out because the physical
// lineside change is independent of which bin is at the slot.
func (m *Mutator) Consumed(ev TickEvent) error {
	m.markDrains(ev)
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		m.acc.recordBin(ev.BinID, ev.PayloadCode, -ev.BinRemainder, protocol.ReasonConsumeTick, ev.BinEpoch)
	}
	return nil
}

// Produced records the per-tick bin production delta. Today's caller is
// handleProduceTick in wiring_counter_delta.go. Bin delta uses
// ReasonProduceTick with a positive sign (parts added to the bin).
//
// Produce nodes don't drain lineside — produce_tick is the only
// emission shape. Drains is unused for this verb.
//
// Skips emission when BinID == 0 (no bin at node) or BinRemainder
// (the positive delta, passed via BinRemainder for symmetry) is 0.
func (m *Mutator) Produced(ev TickEvent) error {
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		m.acc.recordBin(ev.BinID, ev.PayloadCode, ev.BinRemainder, protocol.ReasonProduceTick, ev.BinEpoch)
	}
	return nil
}

// Fallthrough records the per-tick pile drains + bin fallback delta for the
// A/B fallback path (no active-pull consume node visible at tick time).
// Today's caller is emitFallthroughDeltas in wiring_counter_delta.go.
//
// The pile drains are the same as Consumed's — piles physically drain
// regardless of which side of the A/B pair attribution lands on. The bin
// delta uses ReasonABFallthrough so Core's dashboards distinguish the "no
// active pull node" condition from ConsumeTick.
func (m *Mutator) Fallthrough(ev TickEvent) error {
	m.markDrains(ev)
	if ev.BinRemainder > 0 && ev.BinID > 0 {
		m.acc.recordBin(ev.BinID, ev.PayloadCode, -ev.BinRemainder, protocol.ReasonABFallthrough, ev.BinEpoch)
	}
	return nil
}
