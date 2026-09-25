//go:build docker

package engine

import (
	"context"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// threshold_decision_total_test.go — WHICH TOTAL EACH FIRE PATH IS JUDGED
// AGAINST.
//
// Four entry points reach checkBindings: the delta hot path (evaluatePayload),
// the boot pass (startupSweep), a human's manual-swap request
// (NoteSwapRequestContradiction), and the two notification doors (Resync
// here). They all judge a payload against one number: Core's count,
// SystemUOPForPayload — the bins and lineside buckets the Edge's deltas keep
// (seat-count round 1 §5; the owner ruled that loaders are a Core function).
//
// The fixture is the SNF3 shape in miniature: the ledger holds 150 at the line
// node, a fresh Edge report says that node drained to 10, and the threshold is
// 100. The report no longer moves the total on any path; it is a checksum,
// compared on ingest (messaging/lineside_divergence_test.go).

const decisionThreshold = 100

// decisionFixture is one monitored binding with a ledger bin of ledgerUOP at the
// line node, and, when edgeUOP >= 0, a FRESH Edge report of edgeUOP for that
// node.
func decisionFixture(t *testing.T, payload string, ledgerUOP, edgeUOP int) (*ThresholdMonitor, *fireLog, thresholdEntry) {
	t.Helper()
	db := testDB(t)
	sink := &logSink{}
	eng := newLoggingEngine(t, db, sink)
	m := eng.thresholdMonitor
	fires := captureThresholdFires(t, eng)

	b := stationBinding(t, eng, "PLANT.DT", "SLN_DT", payload, 18)
	b.threshold = decisionThreshold
	registerBinding(t, db, b)

	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, payload, sd.LineNode.ID, "BIN-"+payload)
	_, err := db.Exec(`UPDATE bins SET uop_remaining=$1 WHERE id=$2`, ledgerUOP, bin.ID)
	testutil.MustNoErr(t, err, "set ledger uop")
	if edgeUOP >= 0 {
		_, err := db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station: b.stationID, CoreNodeName: sd.LineNode.Name, PayloadCode: payload,
			BinCount: 1, BinUOP: edgeUOP, ReportedAt: time.Now().UTC(),
		})
		testutil.MustNoErr(t, err, "upsert fresh edge report")
	}
	return m, fires, b
}

// the four entry points, each driven through its production door. Each reads
// the binding from demand_registry, where decisionFixture registered it.
var decisionEntryPoints = []struct {
	name  string
	tag   string
	drive func(m *ThresholdMonitor, b thresholdEntry)
}{
	{"evaluatePayload", "EP", func(m *ThresholdMonitor, b thresholdEntry) {
		m.OnBinUOPDelta(b.payloadCode, -1)
	}},
	{"startupSweep", "SS", func(m *ThresholdMonitor, _ thresholdEntry) {
		m.startupSweep(context.Background())
	}},
	{"NoteSwapRequestContradiction", "SW", func(m *ThresholdMonitor, b thresholdEntry) {
		m.NoteSwapRequestContradiction(b.payloadCode)
	}},
	{"notification door (Resync)", "RB", func(m *ThresholdMonitor, b thresholdEntry) {
		m.Resync(b.stationID)
	}},
}

// thresholdEpisodes counts the threshold episodes opened for the payload.
func thresholdEpisodes(t *testing.T, m *ThresholdMonitor, payload string) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, m.eng.db.QueryRow(`SELECT count(*) FROM demand_origins
		WHERE kind = 'threshold' AND payload_code = $1`, payload).Scan(&n), "count threshold episodes")
	return n
}

// assertDecision checks one entry point's outcome: whether it fired, the
// reading it fired with, and that it opened exactly one episode.
func assertDecision(t *testing.T, m *ThresholdMonitor, fires *fireLog, b thresholdEntry, wantFire bool, wantUOP int) {
	t.Helper()
	got := fires.count(b.stationID)
	episodes := thresholdEpisodes(t, m, b.payloadCode)
	if !wantFire {
		if got != 0 || episodes != 0 {
			t.Errorf("fired %d time(s) and opened %d episode(s), want neither", got, episodes)
		}
		return
	}
	if got != 1 {
		t.Fatalf("fired %d time(s), want 1", got)
	}
	if hit := fires.find(b.stationID); hit.CurrentUOP != wantUOP {
		t.Errorf("fired off a reading of %d, want %d", hit.CurrentUOP, wantUOP)
	}
	if episodes != 1 {
		t.Fatalf("opened %d episode(s), want 1", episodes)
	}
}

// EVERY ENTRY POINT DECIDES OFF CORE'S COUNT, report or no report: fire below
// at the ledger's reading, hold above. (The used_edge_reports stamp this pin
// also read went with its column, v132: no Edge-adjusted total exists to
// decide, so the stamp could only ever say false.)
//
// Verify-red at the base on the rows with a report: under the default
// edge_reports mode the ledger at 150 with a report of 10 fired off 10 on all
// four paths, and the ledger at 50 with a report of 170 held on all four.
func TestDecisionTotal_EveryPathReadsCoresCount(t *testing.T) {
	t.Parallel()
	for _, ep := range decisionEntryPoints {
		for _, c := range []struct {
			tag      string
			ledger   int
			edge     int
			wantFire bool
		}{
			{"L50", 50, -1, true},
			{"L150", 150, -1, false},
			{"L150E10", 150, 10, false},
			{"L50E170", 50, 170, true},
		} {
			t.Run(ep.name+"/"+c.tag, func(t *testing.T) {
				t.Parallel()
				m, fires, b := decisionFixture(t, "PANEL-DT-"+c.tag+"-"+ep.tag, c.ledger, c.edge)
				ep.drive(m, b)
				assertDecision(t, m, fires, b, c.wantFire, c.ledger)
			})
		}
	}
}
