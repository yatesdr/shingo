package uop

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// sumBinDeltas drains every queued BinUOPDelta for binID from the outbox
// and returns the signed sum.
func sumBinDeltas(t *testing.T, a *accumulator, binID int64) int {
	t.Helper()
	msgs, err := a.db.ListPendingOutbox(1_000_000)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	total := 0
	for _, m := range msgs {
		if m.MsgType != protocol.SubjectBinUOPDelta {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &data), "decode data")
		var p protocol.BinUOPDelta
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &p), "decode payload")
		if p.BinID == binID {
			total += p.Delta
		}
	}
	return total
}

// TestAccumulator_EvictDoesNotLoseRacingDelta pins R68-1. The lost-update
// window: a delta==0 entry goes idle and is deleted from the map in the
// gap between evictIdle's idle check and a concurrent recordBin writing
// into that same entry pointer. The recordHook seam fires the eviction at
// exactly that point — after recordBin has the entry pointer but before it
// takes the entry lock — so the race is reproduced deterministically
// rather than by chance. Pre-fix this loses the delta (sum 0); with the
// evicted-flag retry the recordBin lands its delta in a fresh entry.
func TestAccumulator_EvictDoesNotLoseRacingDelta(t *testing.T) {
	db := newReporterTestDB(t)
	a := newAccumulator(db, "ALN_001")
	// Pin the clock so the seeded entry's lastTouched is in the distant
	// past and evictIdle(0) treats it as idle on sight.
	a.now = func() time.Time { return time.Unix(0, 0).UTC() }

	const binID int64 = 7
	key := strconv.FormatInt(binID, 10)

	// Seed an idle (delta==0, old lastTouched) entry — the state a bin
	// reaches right after a flush commits and before it's touched again.
	a.bins.Store(key, &binDeltaEntry{
		binID:       binID,
		payloadCode: "PART-A",
		epoch:       1,
		lastTouched: a.now(),
	})

	// When recordBin grabs the seeded entry, fire the eviction once —
	// dead center of the race window.
	a.recordHook = func() {
		a.recordHook = nil // one-shot
		a.evictIdle(0)
	}

	a.recordBin(binID, "PART-A", 5, protocol.ReasonProduceTick, 1)
	a.flush()

	if got := sumBinDeltas(t, a, binID); got != 5 {
		t.Fatalf("flushed delta sum = %d, want 5 — recordBin's delta was lost to a racing eviction", got)
	}
}
