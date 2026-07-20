//go:build docker

package engine

import (
	"sync"
	"testing"

	"shingo/protocol"
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
