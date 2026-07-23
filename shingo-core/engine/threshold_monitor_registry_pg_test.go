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

// TestThresholdMonitor_OnThresholdChanges_ReBaselinesFromDB pins the
// re-baseline fix: a threshold edit must be evaluated against DB TRUTH, not
// against whatever the incremental in-memory cache has drifted to.
//
// engagePayloads used to query SystemUOPForPayload only when the payload
// wasn't already in uopCache, so an edit to an ALREADY-MONITORED payload
// re-evaluated against the stale cached total. Springfield 2026-07-21: nudging
// a threshold 120→121→120 fired nothing because the cache sat at ~139 while
// the DB truth was 31 — a diagnostic loop spent chasing a monitor that was
// answering from memory. Resync ((re)connect) shared the same path.
//
// Setup poisons the cache ABOVE the threshold while the DB says 0. Pre-fix the
// monitor compares 999 >= 50 and stays silent; post-fix it re-reads 0 and
// fires.
func TestThresholdMonitor_OnThresholdChanges_ReBaselinesFromDB(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-rebaseline"
		loader    = "MS-LOADER-REBASE"
		payload   = "P-REBASE"
	)

	// No bins of this payload exist — DB truth for system UOP is 0.
	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed initial registry: %v", err)
	}

	// Poison the cache: already monitored, and far ABOVE the threshold. This is
	// the drifted-ledger state the fix exists for.
	m := eng.thresholdMonitor
	m.mu.Lock()
	m.uopCache[payload] = 999
	m.mu.Unlock()

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	m.OnThresholdChanges([]demands.RegistryChange{{
		StationID:    stationID,
		CoreNodeName: loader,
		PayloadCode:  payload,
		OldThreshold: 40,
		NewThreshold: 50,
	}})

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
		t.Fatalf("expected a signal after the threshold edit — the monitor evaluated the stale cached total (999) instead of DB truth (0); outbox=%v",
			outboxSummary(msgs))
	}
	if hit.CurrentUOP != 0 {
		t.Errorf("signal CurrentUOP = %d, want 0 (DB truth, not the poisoned cache)", hit.CurrentUOP)
	}

	// The cache itself must be corrected, not just the one evaluation —
	// otherwise the next incremental delta resumes from the stale number.
	m.mu.Lock()
	cached := m.uopCache[payload]
	m.mu.Unlock()
	if cached != 0 {
		t.Errorf("uopCache[%s] = %d after re-baseline, want 0", payload, cached)
	}
}

// TestThresholdMonitor_PeriodicReconcile_CorrectsDriftAndFires is the drift backstop:
// the incremental delta path can't see inventory that leaves a payload without a
// consume-delta (direct-DB reassign/reseed, dropped event), so the cached total sticks
// high and silently suppresses ordering. The periodic reconcile must re-read DB truth,
// correct the cache, and fire the signal that was being withheld. This is the
// Springfield 2026-07-23 shape (cache stuck at 263/404 while DB truth went to 0).
func TestThresholdMonitor_PeriodicReconcile_CorrectsDriftAndFires(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-reconcile"
		loader    = "MS-LOADER-RECON"
		payload   = "P-RECON"
	)

	// Binding at threshold 50; no bins of this payload exist → DB truth = 0.
	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	// Inject the drifted state directly: the payload is monitored and its cached total
	// is stuck ABOVE threshold (999) — a reassign removed the stock with no consume-
	// delta, so the binding never fires even though DB truth is 0. No prior fire here,
	// so there is no debounce to fight.
	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID: stationID, coreNodeName: loader, payloadCode: payload, threshold: 50,
	}}
	m.uopCache[payload] = 999
	m.mu.Unlock()

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	m.reconcileAll(context.Background())

	// Cache re-baselined to DB truth (0), not left at the drifted 999.
	m.mu.Lock()
	cached := m.uopCache[payload]
	m.mu.Unlock()
	if cached != 0 {
		t.Errorf("uopCache[%s] = %d after reconcile, want 0 (DB truth)", payload, cached)
	}

	// And the withheld signal fires now that the cache reflects reality.
	msgs, _ := db.ListPendingOutbox(50)
	if countLoopBelowThresholdSignals(msgs, stationID) <= preCount {
		t.Fatalf("expected a LoopBelowThreshold signal after reconcile corrected the drifted cache (999 -> 0); outbox=%v", outboxSummary(msgs))
	}
	if hit := findLoopBelowThresholdSignal(t, msgs, stationID); hit != nil && hit.CurrentUOP != 0 {
		t.Errorf("signal CurrentUOP = %d, want 0 (DB truth, not the drifted 999)", hit.CurrentUOP)
	}
}

// TestThresholdMonitor_NegativeTotal_EmitsNoSignal is the end-to-end half of
// the validity floor: with a negative in-loop total, NOTHING reaches the
// outbox. The unit-level counterpart
// (TestThresholdMonitor_CheckBindings_NegativeTotal_NoFire) proves the branch;
// this proves the observable contract against a real engine, and exercises the
// real logFn path that the nil-eng harness cannot.
//
// The refusal log line itself is not asserted: Engine's logFn is wired at
// construction (LogFunc: t.Logf here) with no capture hook, and adding one to
// production Engine purely for this assertion would be a worse trade than
// asserting the outcome that actually matters — no robot traffic off a broken
// ledger.
func TestThresholdMonitor_NegativeTotal_EmitsNoSignal(t *testing.T) {
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

	m := eng.thresholdMonitor
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{{
		stationID:    stationID,
		coreNodeName: loader,
		payloadCode:  payload,
		threshold:    50,
	}}
	m.uopCache[payload] = -443 // the Springfield 74577-6SA0A.06 total
	m.mu.Unlock()

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	// Drive the hot path the way a real delta would: still deeply negative
	// after applying, and trivially "below threshold".
	m.OnBinUOPDelta(payload, -1)

	time.Sleep(300 * time.Millisecond)

	msgs, _ := db.ListPendingOutbox(50)
	if got := countLoopBelowThresholdSignals(msgs, stationID); got != preCount {
		t.Errorf("negative in-loop total produced %d new LoopBelowThresholdSignal(s); want 0 — a broken ledger must not arm replenishment (outbox=%v)",
			got-preCount, outboxSummary(msgs))
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

// TestThresholdMonitor_StartupSweep_NegativeTotal_EmitsNoSignal pins the floor
// on the RESTART path.
//
// startupSweep used to make its own fire decision — its own total < threshold
// compare plus a direct fireSignalCached — which meant it bypassed every guard
// checkBindings applies, the negative-total floor included. That is the worst
// possible path to miss: restarting Core is the remedy an operator reaches for
// BECAUSE the ledger looks wrong, and the sweep would then fire on the garbage
// total, twice per binding (it seeds warm-up first, and warm-up bypasses
// debounce).
//
// The sweep now routes through checkBindings, so there is one fire decision
// with one set of guards.
func TestThresholdMonitor_StartupSweep_NegativeTotal_EmitsNoSignal(t *testing.T) {
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
	seedNegativeBin(t, db, payload, -443)

	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	// Drive the sweep directly rather than waiting out Run()'s 3s grace.
	eng.thresholdMonitor.startupSweep(context.Background())

	msgs, _ := db.ListPendingOutbox(50)
	if got := countLoopBelowThresholdSignals(msgs, stationID); got != preCount {
		t.Errorf("startup sweep emitted %d signal(s) on a negative in-loop total; want 0 — a restart must not arm replenishment off a broken ledger (outbox=%v)",
			got-preCount, outboxSummary(msgs))
	}
}

// seedNegativeBin creates a bin carrying the given (negative) uop_remaining for
// a payload, which is what makes that payload's in-loop total negative. Bins
// may legitimately go negative under the SME overpack/underpack lock; buckets
// cannot (CHECK qty >= 0), which is why a negative TOTAL always means the bin
// side is wrong.
func seedNegativeBin(t *testing.T, db *store.DB, payloadCode string, uop int) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, payloadCode, sd.StorageNode.ID, "BIN-NEG-"+payloadCode)
	testutil.MustNoErr(t, func() error {
		_, err := db.DB.Exec(`UPDATE bins SET payload_code=$1, uop_remaining=$2 WHERE id=$3`, payloadCode, uop, bin.ID)
		return err
	}(), "seed negative bin")
}
