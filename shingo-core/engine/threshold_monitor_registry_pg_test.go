//go:build docker

package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/demands"
	"shingocore/store/messaging"
)

// TestThresholdMonitor_OnThresholdChanges_FiresImmediatelyWhenBelowThreshold
// pins the Springfield 6883 fix: when a demand-registry sync newly adds
// (or raises) a threshold for a payload whose current system UOP is
// already below the new value, the monitor must fire
// LoopBelowThresholdSignal during OnThresholdChanges — not wait for the
// next bin/bucket delta. Before the fix, OnThresholdChanges only rebuilt
// the cache and reset the debounce; a zero-stock payload (no upcoming
// delta) stayed silent until Core restart.
func TestThresholdMonitor_OnThresholdChanges_FiresImmediatelyWhenBelowThreshold(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-springfield"
		loader    = "MS-LOADER-1"
		payload   = "P-6883"
	)

	// No bins of this payload exist anywhere — system UOP for the
	// payload is 0. Simulates the Springfield case where the payload's
	// in-loop total is below any positive threshold.
	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed initial registry: %v", err)
	}

	// Snapshot outbox state pre-OnThresholdChanges so the assertion
	// below distinguishes the new signal from anything the test engine
	// emitted at startup. The 3s startup-sweep gate keeps the sweep
	// out of this test's window, but we belt-and-brace anyway.
	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	// Drive OnThresholdChanges directly with a synthetic change list — the
	// same shape handleClaimSync would produce after a real SyncRegistry
	// returned a non-empty change set. This isolates the immediate-fire
	// behavior without depending on the full ClaimSync path.
	eng.thresholdMonitor.OnThresholdChanges([]demands.RegistryChange{{
		StationID:    stationID,
		CoreNodeName: loader,
		PayloadCode:  payload,
		OldThreshold: 0,
		NewThreshold: 50,
	}})

	// SendDataToEdge is synchronous to the outbox (DB write inside
	// SendDataToEdge), so a single re-read should suffice. Allow a
	// small retry window for the rare CI scheduling jitter.
	deadline := time.Now().Add(2 * time.Second)
	var hit *protocol.LoopBelowThresholdSignal
	for time.Now().Before(deadline) {
		msgs, _ := db.ListPendingOutbox(50)
		if countLoopBelowThresholdSignals(msgs, stationID) > preCount {
			hit = findLoopBelowThresholdSignal(t, msgs, stationID)
			if hit != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hit == nil {
		msgs, _ := db.ListPendingOutbox(50)
		t.Fatalf("expected immediate LoopBelowThresholdSignal to %s after OnThresholdChanges, outbox=%v",
			stationID, outboxSummary(msgs))
	}
	if hit.PayloadCode != payload {
		t.Errorf("signal PayloadCode = %q, want %q", hit.PayloadCode, payload)
	}
	if hit.CoreNodeName != loader {
		t.Errorf("signal CoreNodeName = %q, want %q", hit.CoreNodeName, loader)
	}
	if hit.Threshold != 50 {
		t.Errorf("signal Threshold = %d, want 50", hit.Threshold)
	}
	if hit.CurrentUOP != 0 {
		t.Errorf("signal CurrentUOP = %d, want 0 (no bins of this payload)", hit.CurrentUOP)
	}
}

// TestThresholdMonitor_ReadsAuthoritativeSum_NotAStaleCache replaces the old
// cache-re-baseline and periodic-reconcile tests. Both existed to prove the
// monitor could recover from a private tally that had drifted from DB truth.
// That tally is deleted — the monitor now reads SystemUOPForPayload on every
// evaluation — so the property to pin is simpler and stronger: an evaluation
// always reflects DB truth, and there is no stale below-threshold belief that
// could fire against a payload that is actually stocked.
//
// Setup: threshold 50, and a bin holding 200 UOP of the payload — the DB says
// STOCKED. A delta arrives. Because the monitor reads the DB (200 >= 50) rather
// than any cached number, it must NOT fire.
func TestThresholdMonitor_ReadsAuthoritativeSum_NotAStaleCache(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-authoritative"
		loader    = "MS-LOADER-AUTH"
		payload   = "P-AUTH"
	)

	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// DB truth: stocked well above threshold.
	seedBinWithUOP(t, db, payload, 200)

	// Engage the binding (as a real Resync/startup would) so the payload is
	// monitored, then drive a delta.
	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID: stationID, coreNodeName: loader, payloadCode: payload, threshold: 50,
	}}
	m.mu.Unlock()

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	m.OnBinUOPDelta(payload, -1)
	time.Sleep(300 * time.Millisecond)

	msgs, _ := db.ListPendingOutbox(50)
	if got := countLoopBelowThresholdSignals(msgs, stationID); got != preCount {
		t.Errorf("stocked payload (DB total 200 >= threshold 50) produced %d new signal(s); want 0 — the monitor must read DB truth, not a stale below-threshold cache (outbox=%v)",
			got-preCount, outboxSummary(msgs))
	}
}

// TestThresholdMonitor_ReadsAuthoritativeSum_FiresWhenDBBelow is the positive
// twin: with DB truth genuinely below threshold, the same delta-driven path
// fires. Together with the test above this pins "the fire decision follows the
// authoritative read, in both directions."
func TestThresholdMonitor_ReadsAuthoritativeSum_FiresWhenDBBelow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-auth-below"
		loader    = "MS-LOADER-AUTH-BELOW"
		payload   = "P-AUTH-BELOW"
	)

	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// DB truth: 10 UOP, below the threshold of 50.
	seedBinWithUOP(t, db, payload, 10)

	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID: stationID, coreNodeName: loader, payloadCode: payload, threshold: 50,
	}}
	m.mu.Unlock()

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	m.OnBinUOPDelta(payload, -1)

	deadline := time.Now().Add(2 * time.Second)
	var hit *protocol.LoopBelowThresholdSignal
	for time.Now().Before(deadline) {
		msgs, _ := db.ListPendingOutbox(50)
		if countLoopBelowThresholdSignals(msgs, stationID) > preCount {
			hit = findLoopBelowThresholdSignal(t, msgs, stationID)
			if hit != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hit == nil {
		msgs, _ := db.ListPendingOutbox(50)
		t.Fatalf("expected a signal — DB truth (10) is below threshold (50); outbox=%v", outboxSummary(msgs))
	}
	if hit.CurrentUOP != 10 {
		t.Errorf("signal CurrentUOP = %d, want 10 (the authoritative DB read)", hit.CurrentUOP)
	}
}

// TestThresholdMonitor_SwapContradiction_ChipsWhenStocked pins P2-C9: a manual
// swap request for a payload whose ledger reads fully stocked (>= its max
// binding threshold) raises the Replenishment Health contradiction chip and
// creates NO signal — the SNF3 phantom-on-hand shape where the operator swaps
// while Core believes stocked.
func TestThresholdMonitor_SwapContradiction_ChipsWhenStocked(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-c9-stocked"
		loader    = "MS-LOADER-C9"
		payload   = "P-C9-STOCKED"
	)

	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// DB truth: stocked at/above the threshold — nothing should fire.
	seedBinWithUOP(t, db, payload, 200)

	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID: stationID, coreNodeName: loader, payloadCode: payload, threshold: 50,
	}}
	m.mu.Unlock()

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	m.NoteSwapRequestContradiction(payload)
	time.Sleep(200 * time.Millisecond)

	// Chip raised for this payload.
	chip := false
	for _, s := range m.Snapshot() {
		if s.PayloadCode == payload && s.SwapContradiction {
			chip = true
		}
	}
	if !chip {
		t.Error("expected P2-C9 SwapContradiction chip for a swap requested against a stocked ledger")
	}

	// And NO order was created — the re-read reads stocked.
	msgs, _ := db.ListPendingOutbox(50)
	if got := countLoopBelowThresholdSignals(msgs, stationID); got != preCount {
		t.Errorf("contradiction re-evaluation created %d signal(s); want 0 (C9 must never create an order)", got-preCount)
	}
}

// TestThresholdMonitor_SwapContradiction_NoChipWhenBelow pins the other half:
// a swap request for a genuinely below-threshold payload is the operator being
// right, not a contradiction — no chip is raised. (A normal below-threshold
// signal may fire; that is the expected path and is covered elsewhere.)
func TestThresholdMonitor_SwapContradiction_NoChipWhenBelow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-c9-below"
		loader    = "MS-LOADER-C9-BELOW"
		payload   = "P-C9-BELOW"
	)

	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// DB truth: below threshold — the operator is right, no contradiction.
	seedBinWithUOP(t, db, payload, 10)

	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID: stationID, coreNodeName: loader, payloadCode: payload, threshold: 50,
	}}
	m.mu.Unlock()

	m.NoteSwapRequestContradiction(payload)
	time.Sleep(200 * time.Millisecond)

	for _, s := range m.Snapshot() {
		if s.PayloadCode == payload && s.SwapContradiction {
			t.Error("raised a contradiction chip for a genuinely below-threshold payload; the operator is right, not contradicted")
		}
	}
}

// TestThresholdMonitor_R1Live_FiresOffEdgeAdjustedTotal is the R1-LIVE flip of the
// old shadow test. The ledger reads STOCKED (a lineside bin at 150, threshold 100)
// while a FRESH Edge report says that node's bin drained to 10 — the SNF3
// divergence. Under the new default (lineside_decision_mode=edge_reports) the fire
// gate decides off the edge-adjusted total (150 + (10−150) = 10 < 100) and MUST
// FIRE, even though the ledger alone (150 >= 100) would hold. The fired signal
// carries the edge-adjusted total, not the ledger. This also exercises the v52
// migration, the edge_lineside_reports store round-trip, and LinesideLedgerByNode
// against a real DB.
//
// CHANGE-DETECTOR: this replaces TestThresholdMonitor_R1Shadow_DisagreesButDecidesNothing,
// which asserted the shadow fired NOTHING. R1 going live inverts that: the whole
// point of R1 is that this exact SNF3 shape now orders replenishment.
func TestThresholdMonitor_R1Live_FiresOffEdgeAdjustedTotal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	// Default mode is edge_reports (config.Defaults) — assert it, so a future
	// default flip trips here rather than silently changing the fire decision.
	if got := eng.thresholdMonitor.decisionMode(); got != linesideModeEdgeReports {
		t.Fatalf("default decision mode = %q, want %q", got, linesideModeEdgeReports)
	}

	const (
		stationID = "station-r1-live"
		loader    = "MS-LOADER-R1"
		payload   = "P-R1-LIVE"
	)

	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 100,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// Ledger truth: a lineside bin holding 150 at a named node — STOCKED (>= 100).
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, payload, sd.LineNode.ID, "BIN-R1")
	testutil.MustNoErr(t, func() error {
		_, err := db.DB.Exec(`UPDATE bins SET uop_remaining=150 WHERE id=$1`, bin.ID)
		return err
	}(), "set bin uop")

	// Edge reports that node's bin drained to 10 — a fresh, sharp divergence.
	testutil.MustNoErr(t, db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
		Station:      stationID,
		CoreNodeName: sd.LineNode.Name,
		PayloadCode:  payload,
		BinCount:     1,
		BinUOP:       10,
		BucketQty:    0,
		ReportedAt:   time.Now().UTC(),
	}), "upsert edge report")

	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID: stationID, coreNodeName: loader, payloadCode: payload, threshold: 100,
	}}
	m.mu.Unlock()

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	// The report-arrival trigger. Edge-adjusted total is 10 (< 100), so it fires.
	m.OnLinesideReports([]string{payload})

	deadline := time.Now().Add(2 * time.Second)
	var hit *protocol.LoopBelowThresholdSignal
	for time.Now().Before(deadline) {
		msgs, _ := db.ListPendingOutbox(50)
		if countLoopBelowThresholdSignals(msgs, stationID) > preCount {
			hit = findLoopBelowThresholdSignal(t, msgs, stationID)
			if hit != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hit == nil {
		msgs, _ := db.ListPendingOutbox(50)
		t.Fatalf("expected R1-live fire — edge-adjusted total (10) is below threshold (100) though the ledger (150) would hold; outbox=%v", outboxSummary(msgs))
	}
	if hit.CurrentUOP != 10 {
		t.Errorf("signal CurrentUOP = %d, want 10 (the edge-adjusted total, not the ledger's 150)", hit.CurrentUOP)
	}
	if hit.Threshold != 100 {
		t.Errorf("signal Threshold = %d, want 100", hit.Threshold)
	}

	// The store round-trip the read-model relies on.
	reports, err := db.ListLinesideReportsForPayload(payload)
	if err != nil {
		t.Fatalf("list lineside reports: %v", err)
	}
	if len(reports) != 1 || reports[0].BinUOP != 10 {
		t.Errorf("stored reports = %+v, want 1 row with BinUOP=10", reports)
	}
}

// TestThresholdMonitor_R1Live_StaleReportFallsBackToLedger pins the per-node
// fallback: a STALE Edge report (older than linesideReportStaleness) contributes
// NO adjustment — that node's ledger term stands. With the ledger STOCKED (150 >=
// 100) and the only report stale, the edge-adjusted total collapses back to the
// ledger, so nothing fires — the stale drained report must not be trusted.
func TestThresholdMonitor_R1Live_StaleReportFallsBackToLedger(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-r1-stale"
		loader    = "MS-LOADER-R1-STALE"
		payload   = "P-R1-STALE"
	)

	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 100,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, payload, sd.LineNode.ID, "BIN-R1-STALE")
	testutil.MustNoErr(t, func() error {
		_, err := db.DB.Exec(`UPDATE bins SET uop_remaining=150 WHERE id=$1`, bin.ID)
		return err
	}(), "set bin uop")

	// Report says drained to 10, but it is STALE (reported well past the window),
	// so it must be ignored and the ledger term (150) stands.
	testutil.MustNoErr(t, db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
		Station:      stationID,
		CoreNodeName: sd.LineNode.Name,
		PayloadCode:  payload,
		BinCount:     1,
		BinUOP:       10,
		BucketQty:    0,
		ReportedAt:   time.Now().UTC().Add(-linesideReportStaleness - time.Minute),
	}), "upsert stale edge report")

	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID: stationID, coreNodeName: loader, payloadCode: payload, threshold: 100,
	}}
	m.mu.Unlock()

	// Prove the helper falls back: no fresh node, edge-adjusted == ledger, usedEdge false.
	edgeTotal, ledgerTotal, usedEdge, err := m.linesideDecisionTotal(context.Background(), payload)
	if err != nil {
		t.Fatalf("linesideDecisionTotal: %v", err)
	}
	if usedEdge {
		t.Error("stale-only report must yield usedEdge=false (fell back to the ledger)")
	}
	if ledgerTotal != 150 || edgeTotal != 150 {
		t.Errorf("totals = edge %d / ledger %d, want both 150 (stale report ignored)", edgeTotal, ledgerTotal)
	}

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	m.OnLinesideReports([]string{payload})
	time.Sleep(200 * time.Millisecond)

	msgs, _ := db.ListPendingOutbox(50)
	if got := countLoopBelowThresholdSignals(msgs, stationID); got != preCount {
		t.Errorf("stale report fired %d signal(s); want 0 — a stale report must fall back to the ledger (150 >= 100 holds) (outbox=%v)",
			got-preCount, outboxSummary(msgs))
	}
}

// TestThresholdMonitor_LedgerMode_RevertsToPreR1 pins the revert knob:
// lineside_decision_mode=ledger reproduces the pre-R1 decision. Same SNF3 setup as
// the live test (ledger 150 STOCKED, fresh Edge report at 10), but in ledger mode
// the report arrival is audit-only and decides off the ledger, so NOTHING fires —
// exactly the round-2 shadow behavior.
func TestThresholdMonitor_LedgerMode_RevertsToPreR1(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	// Flip this monitor to the revert mode (what config lineside_decision_mode=ledger
	// resolves to). Package-internal field, set directly in-test.
	eng.thresholdMonitor.linesideMode = linesideModeLedger
	if got := eng.thresholdMonitor.decisionMode(); got != linesideModeLedger {
		t.Fatalf("decision mode = %q, want %q", got, linesideModeLedger)
	}

	const (
		stationID = "station-r1-ledger"
		loader    = "MS-LOADER-R1-LEDGER"
		payload   = "P-R1-LEDGER"
	)

	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 100,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, payload, sd.LineNode.ID, "BIN-R1-LEDGER")
	testutil.MustNoErr(t, func() error {
		_, err := db.DB.Exec(`UPDATE bins SET uop_remaining=150 WHERE id=$1`, bin.ID)
		return err
	}(), "set bin uop")

	testutil.MustNoErr(t, db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
		Station:      stationID,
		CoreNodeName: sd.LineNode.Name,
		PayloadCode:  payload,
		BinCount:     1,
		BinUOP:       10,
		BucketQty:    0,
		ReportedAt:   time.Now().UTC(),
	}), "upsert edge report")

	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID: stationID, coreNodeName: loader, payloadCode: payload, threshold: 100,
	}}
	m.mu.Unlock()

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	// Report arrival in ledger mode: audit-only, decides off the ledger (150 >= 100).
	m.OnLinesideReports([]string{payload})
	time.Sleep(200 * time.Millisecond)

	msgs, _ := db.ListPendingOutbox(50)
	if got := countLoopBelowThresholdSignals(msgs, stationID); got != preCount {
		t.Errorf("ledger mode fired %d signal(s); want 0 — the revert knob must decide off the ledger and change nothing (outbox=%v)",
			got-preCount, outboxSummary(msgs))
	}

	// And the hot path also decides off the ledger in this mode: a delta re-reads
	// the ledger (150 >= 100), so still nothing fires despite the drained report.
	m.OnBinUOPDelta(payload, -1)
	time.Sleep(200 * time.Millisecond)
	msgs, _ = db.ListPendingOutbox(50)
	if got := countLoopBelowThresholdSignals(msgs, stationID); got != preCount {
		t.Errorf("ledger-mode hot path fired %d signal(s); want 0 (delta decides off the ledger, 150 >= 100) (outbox=%v)",
			got-preCount, outboxSummary(msgs))
	}
}

// TestThresholdMonitor_NegativeTotal_StillEmitsSignal is the end-to-end half
// of the suppression REVERSAL: with a negative in-loop total, a signal still
// reaches the outbox.
//
// It used to assert the opposite. The floor refused to signal on a negative
// total, on the reasoning that a broken ledger must not arm replenishment —
// and on a plant floor that is backwards. A count goes negative because a
// press overpacked, or a fork truck delivered parts off the books, or someone
// moved a bin by hand. None of those are a reason to stop feeding the line,
// and the reading is too LOW, so the honest response is to order material.
//
// Suppressing paired a number saying the line is empty with a system that
// ordered nothing — the first link in the 2026-07-21 chain, logged 1,119 times
// a day at Springfield.
func TestThresholdMonitor_NegativeTotal_StillEmitsSignal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-negative"
		loader    = "MS-LOADER-NEG"
		payload   = "P-NEGATIVE"
	)

	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// DB truth is deeply negative (the Springfield 74577-6SA0A.06 total): a bin
	// carrying -443 makes SystemUOPForPayload return -443.
	seedBinWithUOP(t, db, payload, -443)

	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID:    stationID,
		coreNodeName: loader,
		payloadCode:  payload,
		threshold:    50,
	}}
	m.mu.Unlock()

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	// Drive the hot path the way a real delta would: the monitor re-reads the
	// authoritative sum (-443), which is below threshold and must be acted on.
	m.OnBinUOPDelta(payload, -1)

	time.Sleep(300 * time.Millisecond)

	msgs, _ := db.ListPendingOutbox(50)
	if got := countLoopBelowThresholdSignals(msgs, stationID); got <= preCount {
		t.Errorf("negative in-loop total produced no LoopBelowThresholdSignal; want at least one — a wrong count must not starve the line (outbox=%v)",
			outboxSummary(msgs))
	}
}

// TestThresholdMonitor_Resync_EngagesAndFiresSeededBinding pins the seed-ordering
// fix. A demand_registry binding written OUT-OF-BAND (seeddev / migrateloaders
// write it directly; ClaimSync is retired so the Edge pushes no claims) is
// invisible to the monitor's one-shot startup sweep. Resync — called on Edge
// (re)connect — must engage that binding and fire it immediately when already
// below threshold, WITHOUT relying on a SyncDemandRegistry diff (the registry was
// already written, so there is none). Before the fix the binding stayed dark
// until Core restart — the exact dev-sim symptom.
func TestThresholdMonitor_Resync_EngagesAndFiresSeededBinding(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-resync"
		loader    = "PLK-RESYNC"
		payload   = "BRKT-RESYNC"
	)

	// Seed the registry directly (the seed path), with NO OnThresholdChanges
	// notification — exactly how a fresh dev seed leaves the running monitor
	// stale. No bins of this payload exist → system UOP is 0, below threshold.
	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleProduce,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	// The Edge (re)connects → Resync. No diff is available, so only Resync can
	// engage the binding and fire it.
	eng.thresholdMonitor.Resync(stationID)

	deadline := time.Now().Add(2 * time.Second)
	var hit *protocol.LoopBelowThresholdSignal
	for time.Now().Before(deadline) {
		msgs, _ := db.ListPendingOutbox(50)
		if countLoopBelowThresholdSignals(msgs, stationID) > preCount {
			hit = findLoopBelowThresholdSignal(t, msgs, stationID)
			if hit != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hit == nil {
		msgs, _ := db.ListPendingOutbox(50)
		t.Fatalf("expected Resync to fire LoopBelowThresholdSignal to %s, outbox=%v", stationID, outboxSummary(msgs))
	}
	if hit.PayloadCode != payload || hit.CoreNodeName != loader || hit.Threshold != 50 {
		t.Errorf("signal = payload=%q node=%q threshold=%d, want %s/%s/50", hit.PayloadCode, hit.CoreNodeName, hit.Threshold, payload, loader)
	}

	// Station scoping: Resync of a DIFFERENT station must not fire this binding.
	base := countLoopBelowThresholdSignals(mustOutbox(t, db), stationID)
	eng.thresholdMonitor.Resync("some-other-station")
	time.Sleep(200 * time.Millisecond)
	if got := countLoopBelowThresholdSignals(mustOutbox(t, db), stationID); got != base {
		t.Errorf("Resync(other-station) fired %s's binding (%d → %d)", stationID, base, got)
	}
}

func mustOutbox(t *testing.T, db *store.DB) []*messaging.OutboxMessage {
	t.Helper()
	msgs, err := db.ListPendingOutbox(50)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	return msgs
}

// findLoopBelowThresholdSignal scans outbox rows for a LoopBelowThresholdSignal
// envelope addressed to the given station and decodes it. Mirrors
// findDemandSignal's pattern in wiring_kanban_test.go.
func findLoopBelowThresholdSignal(t *testing.T, msgs []*messaging.OutboxMessage, stationID string) *protocol.LoopBelowThresholdSignal {
	t.Helper()
	wantType := "data." + protocol.SubjectLoopBelowThreshold
	for _, m := range msgs {
		if m.MsgType != wantType || m.StationID != stationID {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &data), "decode data wrapper")
		var sig protocol.LoopBelowThresholdSignal
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &sig), "decode LoopBelowThresholdSignal body")
		return &sig
	}
	return nil
}

// countLoopBelowThresholdSignals counts outbox rows that are
// LoopBelowThresholdSignal envelopes addressed to the given station.
func countLoopBelowThresholdSignals(msgs []*messaging.OutboxMessage, stationID string) int {
	wantType := "data." + protocol.SubjectLoopBelowThreshold
	n := 0
	for _, m := range msgs {
		if m.MsgType == wantType && m.StationID == stationID {
			n++
		}
	}
	return n
}

// TestThresholdMonitor_StartupSweep_NegativeTotal_StillEmitsSignal pins the
// same reversal on the RESTART path.
//
// Restart is the case that matters most here: restarting Core is the remedy an
// operator reaches for BECAUSE the counts look wrong. Under the old floor the
// sweep came up, saw a negative total, and deliberately ordered nothing — so
// the one action a person took to fix a starving line guaranteed it stayed
// starving.
//
// The sweep routes through checkBindings, so there is one fire decision with
// one set of guards, and this pins that the decision is now "order".
func TestThresholdMonitor_StartupSweep_NegativeTotal_StillEmitsSignal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-sweep-negative"
		loader    = "MS-LOADER-SWEEP"
		payload   = "P-SWEEP-NEG"
	)

	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// A bin carrying a deeply negative count is what makes the payload's
	// in-loop total negative — the Springfield 74577-6SA0A.06 shape.
	seedBinWithUOP(t, db, payload, -443)

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	// Drive the sweep directly rather than waiting out Run()'s 3s grace.
	eng.thresholdMonitor.startupSweep(context.Background())

	msgs, _ := db.ListPendingOutbox(50)
	if got := countLoopBelowThresholdSignals(msgs, stationID); got <= preCount {
		t.Errorf("startup sweep emitted no signal on a negative in-loop total; want at least one — a restart is what an operator does BECAUSE the counts look wrong, and it must not leave the line unserved (outbox=%v)",
			outboxSummary(msgs))
	}
}

// seedBinWithUOP creates one available bin carrying the given uop_remaining for
// a payload, so that payload's authoritative in-loop total (SystemUOPForPayload)
// reflects it. Used both to seed a negative total (bins go negative under the
// SME overpack/underpack lock; buckets cannot — CHECK qty >= 0 — so a negative
// TOTAL always means the bin count drifted) and to seed a stocked total that
// must NOT fire.
func seedBinWithUOP(t *testing.T, db *store.DB, payloadCode string, uop int) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, payloadCode, sd.StorageNode.ID, "BIN-"+payloadCode)
	testutil.MustNoErr(t, func() error {
		_, err := db.DB.Exec(`UPDATE bins SET payload_code=$1, uop_remaining=$2 WHERE id=$3`, payloadCode, uop, bin.ID)
		return err
	}(), "seed bin with uop")
}
