//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store/plantclaims"
)

// The brief's acceptance test for Phase 5: force a payload dry, confirm
// EXACTLY one row on the transition and one on recovery — not one per
// 2-minute tick.
//
// The monitor already published on change only (sourceability_publish_test.go
// pins the wire side). The failure this guards against is the persistence
// following the RECOMPUTE cadence instead of the DIFF: at one row per style
// per two minutes, an idle plant writes ~700 rows/day per style and the table
// becomes a sampled metric rather than an event log.
func TestSourceabilityEvents_OneRowPerTransitionNotPerTick(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	bin := createTestBinAtNode(t, db, "BIN-EV", storageNode.ID, "src")

	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "SNF9",
		[]plantclaims.StyleRow{{ProcessID: "SNF9", StyleID: "A"}},
		[]plantclaims.ClaimRow{{ProcessID: "SNF9", StyleID: "A", CoreNodeName: storageNode.Name, PayloadCode: "BIN-EV"}},
		0), "seed mirror")

	eng := newTestEngine(t, db, simulator.New())
	m := eng.SourceabilityMonitor()
	m.publishFn = nil // the wire side is covered elsewhere; this is the persistence
	styleKey := []plantclaims.ProcessKey{{ProcessID: "SNF9", StyleID: "A"}}

	since := time.Now().UTC().Add(-time.Hour)
	events := func() int {
		t.Helper()
		rows, err := eng.SourceabilityEvents(since, "SNF9", "", 200)
		testutil.MustNoErr(t, err, "list sourceability events")
		return len(rows)
	}

	// First observation is a change — there was no prior verdict — so the
	// history has a defined start rather than beginning at the first flap.
	m.recomputeAll()
	first := events()
	if first != 1 {
		t.Fatalf("first observation should write exactly 1 row, got %d", first)
	}

	// THE POINT: five recomputes with nothing moving write nothing.
	for range 5 {
		m.recomputeKeys(styleKey)
	}
	if got := events(); got != first {
		t.Fatalf("steady-state recomputes wrote %d extra rows — the trigger is following the tick, not the diff", got-first)
	}

	// Claim the bin → the pool drains → the style goes unsourceable.
	_, err := db.DB.Exec(`UPDATE bins SET claimed_by = 999 WHERE id = $1`, bin.ID)
	testutil.MustNoErr(t, err, "claim bin")
	m.recomputeKeys(styleKey)

	rows, err := eng.SourceabilityEvents(since, "SNF9", "", 200)
	testutil.MustNoErr(t, err, "list after going red")
	if len(rows) != first+1 {
		t.Fatalf("going unsourceable should write exactly 1 row, got %d total", len(rows))
	}
	red := rows[0] // newest first
	if red.Sourceable {
		t.Error("the style is unsourceable; the row must say so")
	}
	if red.MissingPayload != "BIN-EV" {
		t.Errorf("missing_payload = %q, want BIN-EV — the part someone has to go stock", red.MissingPayload)
	}

	// Still red across more ticks → still nothing new.
	for range 3 {
		m.recomputeKeys(styleKey)
	}
	if got := events(); got != first+1 {
		t.Fatalf("staying red wrote %d extra rows; only TRANSITIONS are events", got-(first+1))
	}

	// Release the bin → recovery. The other half of "went unsourceable at
	// 09:14, recovered 09:41".
	_, err = db.DB.Exec(`UPDATE bins SET claimed_by = NULL WHERE id = $1`, bin.ID)
	testutil.MustNoErr(t, err, "release bin")
	m.recomputeKeys(styleKey)

	rows, err = eng.SourceabilityEvents(since, "SNF9", "", 200)
	testutil.MustNoErr(t, err, "list after recovery")
	if len(rows) != first+2 {
		t.Fatalf("recovery should write exactly 1 row, got %d total", len(rows))
	}
	if !rows[0].Sourceable {
		t.Error("recovered — the row must say sourceable")
	}
	if rows[0].MissingPayload != "" {
		t.Errorf("a recovery has nothing missing, got %q", rows[0].MissingPayload)
	}
}
