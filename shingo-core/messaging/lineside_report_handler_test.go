//go:build docker

package messaging

import (
	"reflect"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/demands"
)

// lineside_report_handler_test.go — which payloads a lineside report hands the
// threshold monitor, per delivery.
//
// OnLinesideReports is not a notification: in edge_reports mode it runs a full
// evaluatePayload (the fire gate) for every payload it is given. The inbox
// dedup does not cover TypeData, so a redelivered envelope reaches this handler
// again, and the only thing that can tell a redelivery from a report is the
// upsert's latest-wins condition.

// linesideMonitor records every OnLinesideReports call. The rest of the
// ThresholdMonitor surface is inert: nothing on this path calls it.
type linesideMonitor struct {
	calls [][]string
}

func (m *linesideMonitor) OnThresholdChanges([]demands.RegistryChange) {}
func (m *linesideMonitor) OnBinUOPDelta(string, int)                   {}
func (m *linesideMonitor) OnBucketApplied(string, string, string, int, protocol.LinesideBucketDeltaReason) {
}
func (m *linesideMonitor) Resync(string) {}
func (m *linesideMonitor) OnLinesideReports(payloadCodes []string) {
	m.calls = append(m.calls, append([]string(nil), payloadCodes...))
}

// since returns the calls made after the first n, which is empty when the
// deliveries since then made no call at all.
func (m *linesideMonitor) since(n int) [][]string { return m.calls[n:] }

func newLinesideService(t *testing.T, db *store.DB) (*CoreDataService, *linesideMonitor) {
	t.Helper()
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	mon := &linesideMonitor{}
	svc.SetThresholdMonitor(mon)
	return svc, mon
}

func linesideEnvelope(station string) *protocol.Envelope {
	return &protocol.Envelope{
		ID:   "env-lineside-" + station,
		Type: protocol.TypeData,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: station},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
}

func linesideEntry(node, payload string, binUOP int) protocol.LinesideLevelEntry {
	return protocol.LinesideLevelEntry{CoreNodeName: node, PayloadCode: payload, BinCount: 1, BinUOP: binUOP}
}

// deliver hands the handler one report and returns the calls it made.
func deliver(svc *CoreDataService, mon *linesideMonitor, env *protocol.Envelope, r *protocol.LinesideLevelReport) [][]string {
	before := len(mon.calls)
	svc.HandleLinesideLevelReport(env, r)
	return mon.since(before)
}

func wantCalls(t *testing.T, what string, got, want [][]string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: OnLinesideReports calls = %v, want %v", what, got, want)
	}
}

// A first report evaluates its payload.
func TestLinesideReport_FirstReportEvaluates(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc, mon := newLinesideService(t, db)

	const station = "stn-lineside-first"
	r := &protocol.LinesideLevelReport{
		Station:    station,
		ReportedAt: time.Now().UTC().Truncate(time.Millisecond),
		Entries:    []protocol.LinesideLevelEntry{linesideEntry("ALN_001", "P-FIRST", 40)},
	}
	wantCalls(t, "first report", deliver(svc, mon, linesideEnvelope(station), r), [][]string{{"P-FIRST"}})
}

// A NEWER report carrying exactly the values already stored still moves the row
// (reported_at advances) and still evaluates. This is what keeps a quiet node
// fresh: a line that has not moved for five minutes is still reporting, and if
// only a value change counted, its row would age past linesideReportStaleness
// and drop the node back to the ledger.
func TestLinesideReport_NewerIdenticalValuesStillMovesAndEvaluates(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc, mon := newLinesideService(t, db)

	const (
		station = "stn-lineside-refresh"
		node    = "ALN_002"
		payload = "P-REFRESH"
	)
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	entries := []protocol.LinesideLevelEntry{linesideEntry(node, payload, 25)}

	deliver(svc, mon, linesideEnvelope(station), &protocol.LinesideLevelReport{Station: station, ReportedAt: t0, Entries: entries})

	t1 := t0.Add(time.Minute)
	got := deliver(svc, mon, linesideEnvelope(station), &protocol.LinesideLevelReport{Station: station, ReportedAt: t1, Entries: entries})
	wantCalls(t, "newer report, identical values", got, [][]string{{payload}})

	rows, err := db.ListLinesideReportsForPayload(payload)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || !rows[0].ReportedAt.UTC().Equal(t1) {
		t.Errorf("rows = %+v, want one row with reported_at %v — the refresh must move the row", rows, t1)
	}
}

// An upsert that fails skips that entry and only that entry: the others are
// still written and still evaluated. The failure is forced with a value the
// INTEGER column cannot hold.
func TestLinesideReport_UpsertErrorSkipsOnlyThatEntry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc, mon := newLinesideService(t, db)

	const station = "stn-lineside-err"
	r := &protocol.LinesideLevelReport{
		Station:    station,
		ReportedAt: time.Now().UTC().Truncate(time.Millisecond),
		Entries: []protocol.LinesideLevelEntry{
			linesideEntry("ALN_003", "P-ERR-BAD", 1<<40),
			linesideEntry("ALN_004", "P-ERR-GOOD", 30),
		},
	}
	wantCalls(t, "one bad entry", deliver(svc, mon, linesideEnvelope(station), r), [][]string{{"P-ERR-GOOD"}})

	if rows, err := db.ListLinesideReportsForPayload("P-ERR-BAD"); err != nil || len(rows) != 0 {
		t.Errorf("bad entry rows = %+v (err %v), want none", rows, err)
	}
}

// The handler's statement cost is one upsert per well-formed entry, whatever
// the delivery turns out to be. A blank entry costs nothing. The monitor here
// is the fake, so this counts the handler's own statements, not the
// evaluation's.
func TestLinesideReport_StatementsPerEnvelope(t *testing.T) {
	t.Parallel()
	_, cfg := testdb.OpenWithConfig(t)
	cdb, counter, err := store.OpenCounting(cfg)
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	t.Cleanup(func() { cdb.Close() })
	svc, mon := newLinesideService(t, cdb)

	const station = "stn-lineside-count"
	r := &protocol.LinesideLevelReport{
		Station:    station,
		ReportedAt: time.Now().UTC().Truncate(time.Millisecond),
		Entries: []protocol.LinesideLevelEntry{
			linesideEntry("ALN_005", "P-COUNT-A", 10),
			linesideEntry("ALN_006", "P-COUNT-A", 11),
			linesideEntry("ALN_007", "P-COUNT-B", 12),
			{CoreNodeName: "", PayloadCode: "P-COUNT-C"},
		},
	}
	for _, delivery := range []string{"first delivery", "redelivery"} {
		counter.Reset()
		deliver(svc, mon, linesideEnvelope(station), r)
		if got := counter.Count(); got != 3 {
			t.Errorf("%s: %d statements, want 3 (one upsert per well-formed entry)", delivery, got)
		}
	}
}

// The same envelope delivered twice evaluates once. The inbox dedup does not
// gate TypeData, so the redelivery reaches the handler; the upsert's strict
// `reported_at <` makes it a no-op on the row, and a row that did not move has
// nothing new to evaluate. At Springfield on 2026-08-20/21 envelope ids were
// redelivered 2-4x each, and every one ran the fire gate again.
func TestLinesideReport_RedeliveredEnvelopeEvaluatesOnce(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc, mon := newLinesideService(t, db)

	const station = "stn-lineside-redeliver"
	env := linesideEnvelope(station)
	r := &protocol.LinesideLevelReport{
		Station:    station,
		ReportedAt: time.Now().UTC().Truncate(time.Millisecond),
		Entries:    []protocol.LinesideLevelEntry{linesideEntry("ALN_008", "P-REDELIVER", 20)},
	}
	wantCalls(t, "first delivery", deliver(svc, mon, env, r), [][]string{{"P-REDELIVER"}})
	wantCalls(t, "redelivery", deliver(svc, mon, env, r), nil)
}

// An older report arriving after a newer one moves nothing and evaluates
// nothing — a requeued dead letter, or a reordered delivery.
func TestLinesideReport_OlderAfterNewerDoesNotEvaluate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc, mon := newLinesideService(t, db)

	const station = "stn-lineside-older"
	now := time.Now().UTC().Truncate(time.Millisecond)
	newer := &protocol.LinesideLevelReport{
		Station: station, ReportedAt: now,
		Entries: []protocol.LinesideLevelEntry{linesideEntry("ALN_009", "P-OLDER", 20)},
	}
	older := &protocol.LinesideLevelReport{
		Station: station, ReportedAt: now.Add(-time.Minute),
		Entries: []protocol.LinesideLevelEntry{linesideEntry("ALN_009", "P-OLDER", 150)},
	}
	wantCalls(t, "newer report", deliver(svc, mon, linesideEnvelope(station), newer), [][]string{{"P-OLDER"}})
	wantCalls(t, "older report after it", deliver(svc, mon, linesideEnvelope(station), older), nil)
}

// A report stamped at the same instant as the stored row, with DIFFERENT
// values, does not move the row: the upsert's condition is strict
// `reported_at <`, and 529dbe1a ruled that the first report at an instant
// stands. The row did not move, so there is nothing new to evaluate.
func TestLinesideReport_SameInstantDifferentValuesDoesNotEvaluate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc, mon := newLinesideService(t, db)

	const station = "stn-lineside-same-instant"
	at := time.Now().UTC().Truncate(time.Millisecond)
	first := &protocol.LinesideLevelReport{
		Station: station, ReportedAt: at,
		Entries: []protocol.LinesideLevelEntry{linesideEntry("ALN_010", "P-INSTANT", 20)},
	}
	second := &protocol.LinesideLevelReport{
		Station: station, ReportedAt: at,
		Entries: []protocol.LinesideLevelEntry{linesideEntry("ALN_010", "P-INSTANT", 5)},
	}
	deliver(svc, mon, linesideEnvelope(station), first)
	wantCalls(t, "same instant, different values", deliver(svc, mon, linesideEnvelope(station), second), nil)
}

// In one batch, only the payloads with at least one moved row are passed on.
// P-MIXED-DUP's only row already holds this instant (an earlier envelope put
// it there); P-MIXED-MOVED is new at one node and a no-op at another, and is
// passed once because one of its rows moved.
func TestLinesideReport_MixedBatchPassesOnlyMovedPayloads(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc, mon := newLinesideService(t, db)

	const station = "stn-lineside-mixed"
	at := time.Now().UTC().Truncate(time.Millisecond)
	seed := &protocol.LinesideLevelReport{
		Station: station, ReportedAt: at,
		Entries: []protocol.LinesideLevelEntry{
			linesideEntry("ALN_011", "P-MIXED-DUP", 20),
			linesideEntry("ALN_012", "P-MIXED-MOVED", 20),
		},
	}
	deliver(svc, mon, linesideEnvelope(station), seed)

	batch := &protocol.LinesideLevelReport{
		Station: station, ReportedAt: at,
		Entries: []protocol.LinesideLevelEntry{
			linesideEntry("ALN_011", "P-MIXED-DUP", 20),
			linesideEntry("ALN_012", "P-MIXED-MOVED", 20),
			linesideEntry("ALN_013", "P-MIXED-MOVED", 30),
		},
	}
	wantCalls(t, "mixed batch", deliver(svc, mon, linesideEnvelope(station), batch), [][]string{{"P-MIXED-MOVED"}})
}
