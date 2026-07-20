//go:build docker

package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store/plantclaims"
)

// TestSourceabilityPage_GridClaimsAndQueue verifies the Core page read model: the
// grid verdict comes from the gated snapshot, the drill-in carries per-claim
// free/held pool counts, and the replenishment queue is empty while the at-risk
// tier is dark.
func TestSourceabilityPage_GridClaimsAndQueue(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)

	// Pool for BIN-A: one available bin (free) + one claimed bin (held).
	createTestBinAtNode(t, db, "BIN-A", storageNode.ID, "free-a")
	claimed := createTestBinAtNode(t, db, "BIN-A", storageNode.ID, "held-a")
	if _, err := db.DB.Exec(`UPDATE bins SET claimed_by = 999 WHERE id = $1`, claimed.ID); err != nil {
		t.Fatalf("claim bin: %v", err)
	}

	// SNF2: style A needs BIN-A (satisfiable), style Z needs BIN-Z (missing).
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "SNF2",
		[]plantclaims.StyleRow{{ProcessID: "SNF2", StyleID: "A"}, {ProcessID: "SNF2", StyleID: "Z"}},
		[]plantclaims.ClaimRow{
			{ProcessID: "SNF2", StyleID: "A", CoreNodeName: storageNode.Name, PayloadCode: "BIN-A"},
			{ProcessID: "SNF2", StyleID: "Z", CoreNodeName: storageNode.Name, PayloadCode: "BIN-Z"},
		}, 0), "seed mirror")

	eng := newTestEngine(t, db, simulator.New())
	eng.SourceabilityMonitor().recomputeAll() // refresh the snapshot the page reads

	view, err := eng.SourceabilityPage()
	if err != nil {
		t.Fatalf("SourceabilityPage: %v", err)
	}

	if view.RunningStyleKnown {
		t.Error("RunningStyleKnown should be false (Core has no active-style signal)")
	}
	if len(view.Queue) != 0 {
		t.Errorf("queue = %+v, want empty (at-risk tier dark)", view.Queue)
	}

	var proc *SourcingProcessView
	for i := range view.Processes {
		if view.Processes[i].ProcessID == "SNF2" {
			proc = &view.Processes[i]
		}
	}
	if proc == nil {
		t.Fatalf("no SNF2 process in %+v", view.Processes)
	}

	styles := map[string]SourcingStyleView{}
	for _, s := range proc.Styles {
		styles[s.StyleID] = s
	}
	a, z := styles["A"], styles["Z"]

	if a.Status != "green" {
		t.Errorf("style A status = %q, want green", a.Status)
	}
	if len(a.Claims) != 1 || a.Claims[0].Payload != "BIN-A" {
		t.Fatalf("style A claims = %+v, want one BIN-A claim", a.Claims)
	}
	if a.Claims[0].Free != 1 || a.Claims[0].Held != 1 {
		t.Errorf("BIN-A pool = free %d / held %d, want 1 / 1", a.Claims[0].Free, a.Claims[0].Held)
	}
	// The free bin's real LOCATION is surfaced, not the claim node. Both bins
	// sit at storageNode; the free one must appear there with count 1, and the
	// claim's own node is carried separately as where the material feeds.
	if got := a.Claims[0].FreeLocations; len(got) != 1 || got[0].Node != storageNode.Name || got[0].Count != 1 {
		t.Errorf("BIN-A free locations = %+v, want one %s ×1", got, storageNode.Name)
	}
	if a.Claims[0].FeedsNode != storageNode.Name {
		t.Errorf("BIN-A feeds node = %q, want %q", a.Claims[0].FeedsNode, storageNode.Name)
	}
	if a.Claims[0].HasTTE {
		t.Error("no TTE expected while the at-risk tier is dark")
	}

	if z.Status != "red" {
		t.Errorf("style Z status = %q, want red", z.Status)
	}
	if len(z.Missing) != 1 || z.Missing[0] != "BIN-Z" {
		t.Errorf("style Z missing = %v, want [BIN-Z]", z.Missing)
	}
}
