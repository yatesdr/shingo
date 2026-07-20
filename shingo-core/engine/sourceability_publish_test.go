//go:build docker

package engine

import (
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/eventbus"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store/plantclaims"
)

// TestSourceabilityMonitor_PublishOnChangeOnly drives the real monitor against a
// real DB and asserts the outbound feed's contract: a full recompute publishes a
// snapshot, a recompute with nothing changed publishes NOTHING (no steady-state
// chatter), and a real change publishes a delta with the moved style.
func TestSourceabilityMonitor_PublishOnChangeOnly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "BIN-A", storageNode.ID, "src")

	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "SNF2",
		[]plantclaims.StyleRow{{ProcessID: "SNF2", StyleID: "A"}},
		[]plantclaims.ClaimRow{{ProcessID: "SNF2", StyleID: "A", CoreNodeName: storageNode.Name, PayloadCode: "BIN-A"}},
		0), "seed mirror")

	eng := newTestEngine(t, db, simulator.New())
	m := eng.SourceabilityMonitor()

	var (
		mu      sync.Mutex
		reports []protocol.SourcingStateReport
	)
	m.publishFn = func(r protocol.SourcingStateReport) {
		mu.Lock()
		reports = append(reports, r)
		mu.Unlock()
	}
	drain := func() []protocol.SourcingStateReport {
		mu.Lock()
		defer mu.Unlock()
		out := reports
		reports = nil
		return out
	}

	styleKey := []plantclaims.ProcessKey{{ProcessID: "SNF2", StyleID: "A"}}

	// Full recompute → a snapshot carrying the green style + its reason.
	m.recomputeAll()
	snap := drain()
	if len(snap) != 1 || !snap[0].Snapshot {
		t.Fatalf("full recompute published %+v, want one snapshot", snap)
	}
	s, ok := findWireState(snap[0].States, "SNF2", "A")
	if !ok || s.Status != "green" {
		t.Fatalf("snapshot state = %+v, want SNF2/A green", snap[0].States)
	}
	if s.Reason == "" {
		t.Errorf("green state has no reason sentence")
	}

	// Nothing changed → a dirty recompute publishes nothing (change-only).
	m.recomputeKeys(styleKey)
	if got := drain(); len(got) != 0 {
		t.Fatalf("steady-state recompute published %d reports, want 0", len(got))
	}

	// Claim the bin → pool drains → the style flips to red → one delta.
	if _, err := db.DB.Exec(`UPDATE bins SET claimed_by = 999 WHERE id = $1`, bin.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}
	m.recomputeKeys(styleKey)
	delta := drain()
	if len(delta) != 1 || delta[0].Snapshot {
		t.Fatalf("change published %+v, want one delta", delta)
	}
	d, ok := findWireState(delta[0].States, "SNF2", "A")
	if !ok || d.Status != "red" {
		t.Fatalf("delta state = %+v, want SNF2/A red", delta[0].States)
	}
	if len(d.Missing) != 1 || d.Missing[0] != "BIN-A" {
		t.Errorf("delta missing = %v, want [BIN-A]", d.Missing)
	}
}

func findWireState(states []protocol.SourcingState, process, style string) (protocol.SourcingState, bool) {
	for _, s := range states {
		if s.ProcessID == process && s.StyleID == style {
			return s, true
		}
	}
	return protocol.SourcingState{}, false
}

// TestSourceabilityMonitor_SSEOnVerdictChangeOnly pins the trigger the /sourcing
// page refreshes on. The page previously reloaded on `connected` (an infinite
// loop — load, connect, reload, connect) and on bin-update (a pool read that
// feeds a verdict rather than the verdict itself), so it pulsed forever on an
// idle plant. Field-observed at Springfield.
//
// The contract now: EventSourcingUpdated fires when a verdict MOVES and at no
// other time. Critically, a steady-state full recompute must stay silent even
// though it still publishes a wire snapshot every cycle for late-joining edges —
// following that cadence would refresh the page on a timer, which is the same
// bug wearing a different hat.
func TestSourceabilityMonitor_SSEOnVerdictChangeOnly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "BIN-A", storageNode.ID, "src")

	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "SNF2",
		[]plantclaims.StyleRow{{ProcessID: "SNF2", StyleID: "A"}},
		[]plantclaims.ClaimRow{{ProcessID: "SNF2", StyleID: "A", CoreNodeName: storageNode.Name, PayloadCode: "BIN-A"}},
		0), "seed mirror")

	eng := newTestEngine(t, db, simulator.New())
	m := eng.SourceabilityMonitor()
	m.publishFn = func(protocol.SourcingStateReport) {} // silence the wire

	var (
		mu     sync.Mutex
		events int
	)
	eventbus.SubscribeTyped(eng.Events,
		func(eventbus.TypedEvent[EventType, SourcingUpdatedEvent]) {
			mu.Lock()
			events++
			mu.Unlock()
		}, EventSourcingUpdated)

	// eventbus.Emit runs subscribers inline, so a recompute has finished
	// emitting by the time it returns — no synchronisation needed here.
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return events
	}
	reset := func() {
		mu.Lock()
		events = 0
		mu.Unlock()
	}

	styleKey := []plantclaims.ProcessKey{{ProcessID: "SNF2", StyleID: "A"}}

	// Settle first. Engine startup runs its own recompute, so the monitor may
	// already hold the current verdict before this test touches it — asserting
	// on "the first recompute" would be asserting on startup ordering.
	m.recomputeAll()
	reset()

	// THE IDLE CASE. Nothing about the plant changed. Neither a dirty recompute
	// nor a full one may emit, or the page reloads on a timer forever.
	reset()
	m.recomputeKeys(styleKey)
	m.recomputeAll()
	m.recomputeAll()
	if got := count(); got != 0 {
		t.Fatalf("idle recomputes emitted %d sourcing-update events, want 0 — "+
			"an idle plant must not refresh the page", got)
	}

	// A real change → exactly one emit.
	reset()
	if _, err := db.DB.Exec(`UPDATE bins SET claimed_by = 999 WHERE id = $1`, bin.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}
	m.recomputeKeys(styleKey)
	if got := count(); got != 1 {
		t.Fatalf("verdict change emitted %d events, want exactly 1", got)
	}

	// And settling at the new verdict is silent again.
	reset()
	m.recomputeKeys(styleKey)
	m.recomputeAll()
	if got := count(); got != 0 {
		t.Fatalf("post-change idle emitted %d events, want 0", got)
	}
}
