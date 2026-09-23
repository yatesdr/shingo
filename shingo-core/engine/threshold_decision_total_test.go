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
// (NoteSwapRequestContradiction), and the two notification doors through
// evaluateRebuiltBindings (Resync here). They must all judge the same payload
// against the same number, and that number is the one the configured R1 mode
// names. The persisted used_edge_reports stamp must say which one decided.
//
// The fixture is the SNF3 shape in miniature: the ledger holds 150 at the line
// node, the Edge reports that node drained to 10, and the threshold is 100. So
// the two totals straddle the trigger, and whichever one a path decides off is
// visible in whether it fires and in the reading it fires with.

const decisionThreshold = 100

// decisionFixture is one monitored binding with a ledger bin of ledgerUOP at the
// line node, and, when edgeUOP >= 0, a FRESH Edge report of edgeUOP for that
// node.
func decisionFixture(t *testing.T, mode, payload string, ledgerUOP, edgeUOP int) (*ThresholdMonitor, *fireLog, thresholdEntry) {
	t.Helper()
	db := testDB(t)
	sink := &logSink{}
	eng := newLoggingEngine(t, db, sink)
	m := eng.thresholdMonitor
	m.linesideMode = mode
	fires := captureThresholdFires(t, eng)

	b := stationBinding(t, eng, "PLANT.DT", "SLN_DT", payload, 18)
	b.threshold = decisionThreshold
	registerBinding(t, db, b)

	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, payload, sd.LineNode.ID, "BIN-"+payload)
	_, err := db.Exec(`UPDATE bins SET uop_remaining=$1 WHERE id=$2`, ledgerUOP, bin.ID)
	testutil.MustNoErr(t, err, "set ledger uop")
	if edgeUOP >= 0 {
		testutil.MustNoErr(t, db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station: b.stationID, CoreNodeName: sd.LineNode.Name, PayloadCode: payload,
			BinCount: 1, BinUOP: edgeUOP, ReportedAt: time.Now().UTC(),
		}), "upsert fresh edge report")
	}
	return m, fires, b
}

// monitorPayload makes the binding visible to the paths that start from the
// monitor's own binding set rather than from a door that rebuilds it.
func monitorPayload(m *ThresholdMonitor, b thresholdEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.thresholdsByPayload[b.payloadCode] = []thresholdEntry{b}
}

// the four entry points, each driven through its production door.
var decisionEntryPoints = []struct {
	name  string
	tag   string
	drive func(m *ThresholdMonitor, b thresholdEntry)
}{
	{"evaluatePayload", "EP", func(m *ThresholdMonitor, b thresholdEntry) {
		monitorPayload(m, b)
		m.OnBinUOPDelta(b.payloadCode, -1)
	}},
	{"startupSweep", "SS", func(m *ThresholdMonitor, _ thresholdEntry) {
		m.startupSweep(context.Background())
	}},
	{"NoteSwapRequestContradiction", "SW", func(m *ThresholdMonitor, b thresholdEntry) {
		monitorPayload(m, b)
		m.NoteSwapRequestContradiction(b.payloadCode)
	}},
	{"evaluateRebuiltBindings", "RB", func(m *ThresholdMonitor, b thresholdEntry) {
		m.Resync(b.stationID)
	}},
}

// usedEdgeStamps returns the used_edge_reports stamp of every threshold episode
// for the payload, oldest first.
func usedEdgeStamps(t *testing.T, m *ThresholdMonitor, payload string) []bool {
	t.Helper()
	rows, err := m.eng.db.Query(`SELECT used_edge_reports FROM demand_origins
		WHERE kind = 'threshold' AND payload_code = $1 ORDER BY opened_at`, payload)
	testutil.MustNoErr(t, err, "read used_edge_reports")
	defer rows.Close()
	var out []bool
	for rows.Next() {
		var v bool
		testutil.MustNoErr(t, rows.Scan(&v), "scan used_edge_reports")
		out = append(out, v)
	}
	return out
}

// assertDecision checks one entry point's outcome: whether it fired, the
// reading it fired with, and the stamp on the one episode it opened.
func assertDecision(t *testing.T, m *ThresholdMonitor, fires *fireLog, b thresholdEntry, wantFire bool, wantUOP int, wantStamp bool) {
	t.Helper()
	got := fires.count(b.stationID)
	stamps := usedEdgeStamps(t, m, b.payloadCode)
	if !wantFire {
		if got != 0 || len(stamps) != 0 {
			t.Errorf("fired %d time(s) and opened %d episode(s), want neither", got, len(stamps))
		}
		return
	}
	if got != 1 {
		t.Fatalf("fired %d time(s), want 1", got)
	}
	if hit := fires.find(b.stationID); hit.CurrentUOP != wantUOP {
		t.Errorf("fired off a reading of %d, want %d", hit.CurrentUOP, wantUOP)
	}
	if len(stamps) != 1 {
		t.Fatalf("opened %d episode(s), want 1", len(stamps))
	}
	if stamps[0] != wantStamp {
		t.Errorf("used_edge_reports = %v, want %v — the stamp records which total decided", stamps[0], wantStamp)
	}
}

// WITH NO EDGE REPORT THE TWO TOTALS ARE ONE NUMBER, so every entry point in
// either mode decides off the ledger: fire below, hold above, stamp false.
func TestDecisionTotal_LedgerOnlyIsTheSameEverywhere(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{linesideModeEdgeReports, linesideModeLedger} {
		for _, ep := range decisionEntryPoints {
			for _, c := range []struct {
				ledger   int
				wantFire bool
			}{{50, true}, {150, false}} {
				t.Run(mode+"/"+ep.name, func(t *testing.T) {
					t.Parallel()
					payload := "PANEL-DT-L" + itoa(c.ledger) + "-" + ep.tag + "-" + mode[:3]
					m, fires, b := decisionFixture(t, mode, payload, c.ledger, -1)
					ep.drive(m, b)
					assertDecision(t, m, fires, b, c.wantFire, c.ledger, false)
				})
			}
		}
	}
}

// IN LEDGER MODE A FRESH EDGE REPORT DECIDES NOTHING, at any entry point. The
// ledger reads 150, above the trigger, so nothing fires wherever it came in.
func TestDecisionTotal_LedgerModeIgnoresTheEdgeEverywhere(t *testing.T) {
	t.Parallel()
	for _, ep := range decisionEntryPoints {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()
			m, fires, b := decisionFixture(t, linesideModeLedger, "PANEL-DT-LM-"+ep.tag, 150, 10)
			ep.drive(m, b)
			assertDecision(t, m, fires, b, false, 0, false)
		})
	}
}

// IN LEDGER MODE THE STAMP SAYS LEDGER, even when a fresh report exists. The
// ledger reads 50 (below), the Edge reports 10; the ledger decided, at 50, so
// used_edge_reports is false. evaluatePayload used to stamp true here, because
// it passed "a fresh report moved the Edge total" where the column records
// "the Edge total decided".
func TestDecisionTotal_LedgerModeStampsLedgerWithAFreshReport(t *testing.T) {
	t.Parallel()
	for _, ep := range decisionEntryPoints {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()
			m, fires, b := decisionFixture(t, linesideModeLedger, "PANEL-DT-LS-"+ep.tag, 50, 10)
			ep.drive(m, b)
			assertDecision(t, m, fires, b, true, 50, false)
		})
	}
}

// IN EDGE_REPORTS MODE EVERY ENTRY POINT DECIDES OFF THE EDGE-ADJUSTED TOTAL.
// The ledger reads 150 (would hold), the fresh report puts the adjusted total at
// 10 (below), so every path fires off 10 and stamps true.
//
// Only evaluatePayload did this. The boot pass, the manual-swap recheck and the
// notification doors judged against the bare ledger and stamped false by
// construction — so three of four fire paths ignored the configured mode, and
// the manual-swap recheck re-checked a human's "this place is empty" against
// the very number it had just logged as a phantom.
func TestDecisionTotal_EdgeModeDecidesOffTheEdgeEverywhere(t *testing.T) {
	t.Parallel()
	for _, ep := range decisionEntryPoints {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()
			m, fires, b := decisionFixture(t, linesideModeEdgeReports, "PANEL-DT-EM-"+ep.tag, 150, 10)
			ep.drive(m, b)
			assertDecision(t, m, fires, b, true, 10, true)
		})
	}
}

// AND THE OTHER DIRECTION: the ledger reads 50 (would fire), the fresh report
// puts the adjusted total at 170 (holds). In edge_reports mode nothing fires,
// wherever it came in.
func TestDecisionTotal_EdgeModeHoldsOffTheEdgeEverywhere(t *testing.T) {
	t.Parallel()
	for _, ep := range decisionEntryPoints {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()
			m, fires, b := decisionFixture(t, linesideModeEdgeReports, "PANEL-DT-EH-"+ep.tag, 50, 170)
			ep.drive(m, b)
			assertDecision(t, m, fires, b, false, 0, false)
		})
	}
}
