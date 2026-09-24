//go:build docker

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/demands"
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
	fires := captureThresholdFires(t, eng)
	preCount := fires.count(stationID)

	// Drive OnThresholdChanges directly with a synthetic change list — the
	// same shape the loader config-edit path would produce after a real
	// SyncRegistry returned a non-empty change set. This isolates the
	// immediate-fire behavior without depending on the full config-edit path.
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
	var hit *firedBinding
	for time.Now().Before(deadline) {
		if fires.count(stationID) > preCount {
			hit = fires.find(stationID)
			if hit != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hit == nil {
		t.Fatalf("expected immediate LoopBelowThresholdSignal to %s after OnThresholdChanges, outbox=%v",
			stationID, fires.fired)
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

	fires := captureThresholdFires(t, eng)
	preCount := fires.count(stationID)

	m.OnBinUOPDelta(payload, -1)
	time.Sleep(300 * time.Millisecond)

	if got := fires.count(stationID); got != preCount {
		t.Errorf("stocked payload (DB total 200 >= threshold 50) produced %d new signal(s); want 0 — the monitor must read DB truth, not a stale below-threshold cache (outbox=%v)",
			got-preCount, fires.fired)
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

	fires := captureThresholdFires(t, eng)
	preCount := fires.count(stationID)

	m.OnBinUOPDelta(payload, -1)

	deadline := time.Now().Add(2 * time.Second)
	var hit *firedBinding
	for time.Now().Before(deadline) {
		if fires.count(stationID) > preCount {
			hit = fires.find(stationID)
			if hit != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hit == nil {
		t.Fatalf("expected a signal — DB truth (10) is below threshold (50); outbox=%v", fires.fired)
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

	fires := captureThresholdFires(t, eng)
	preCount := fires.count(stationID)

	m.NoteSwapRequestContradiction(payload)
	time.Sleep(200 * time.Millisecond)

	// Chip raised for this payload.
	chip := false
	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, s := range snap {
		if s.PayloadCode == payload && s.SwapContradiction {
			chip = true
		}
	}
	if !chip {
		t.Error("expected P2-C9 SwapContradiction chip for a swap requested against a stocked ledger")
	}

	// And NO order was created — the re-read reads stocked.
	if got := fires.count(stationID); got != preCount {
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

	m.NoteSwapRequestContradiction(payload)
	time.Sleep(200 * time.Millisecond)

	snap, err := m.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, s := range snap {
		if s.PayloadCode == payload && s.SwapContradiction {
			t.Error("raised a contradiction chip for a genuinely below-threshold payload; the operator is right, not contradicted")
		}
	}
}

// TestThresholdMonitor_ReportBelowTheLedger_HoldsAndOpensADivergence is the SNF3
// shape under the ruling that decisions read Core's count (seat-count round 1
// §5, S3). The ledger reads STOCKED (a bound carrier at 150, threshold 100)
// while a fresh Edge report says that carrier drained to 10.
//
// INVERTED by lane A of the memory build. It was
// TestThresholdMonitor_R1Live_FiresOffEdgeAdjustedTotal, which pinned that the
// report arrival FIRED off the edge-adjusted total (150 + (10-150) = 10 < 100).
// Now the report decides nothing: the arrival evaluates nothing, a delta judges
// the payload against 150 and HOLDS, and the disagreement is where it belongs —
// one open report_divergence episode for that carrier, Edge 10 against Core 150.
// Nothing heals it automatically; a count correction through the front door
// does (round 1 §5).
func TestThresholdMonitor_ReportBelowTheLedger_HoldsAndOpensADivergence(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-r1-inverted"
		loader    = "MS-LOADER-R1"
		payload   = "P-R1-INVERTED"
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

	// Ledger truth: a bound carrier holding 150 at the line node — STOCKED.
	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, payload, sd.LineNode.ID, "BIN-R1")
	testutil.MustNoErr(t, func() error {
		_, err := db.DB.Exec(`UPDATE bins SET uop_remaining=150 WHERE id=$1`, bin.ID)
		return err
	}(), "set bin uop")
	var epoch int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, bin.ID).Scan(&epoch), "read epoch")

	fires := captureThresholdFires(t, eng)
	preCount := fires.count(stationID)

	// The Edge's report, through the real handler with the real monitor wired,
	// as the wire carries it: this carrier, this generation, nothing flushed
	// that Core has not applied, and a count of 10.
	svc := messaging.NewCoreDataService(db, messaging.NewCoreHandler(db, nil, "core", "", nil), service.EpochAnnounce{})
	svc.SetThresholdMonitor(eng.thresholdMonitor)
	var rep protocol.LinesideLevelReport
	testutil.MustNoErr(t, json.Unmarshal([]byte(fmt.Sprintf(
		`{"station":%q,"reported_at":%q,"entries":[{"core_node_name":%q,"payload_code":%q,"bin_count":1,"bin_uop":10,"bucket_qty":0,"bin_id":%d,"bin_epoch":%d,"flushed_seq":0}]}`,
		stationID, time.Now().UTC().Format(time.RFC3339Nano), sd.LineNode.Name, payload, bin.ID, epoch)), &rep), "decode report")
	svc.HandleLinesideLevelReport(&protocol.Envelope{
		Type: protocol.TypeData,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: stationID},
	}, &rep)

	// And a delta, which evaluates the payload against Core's count.
	eng.thresholdMonitor.OnBinUOPDelta(payload, -1)
	time.Sleep(200 * time.Millisecond)

	if got := fires.count(stationID); got != preCount {
		t.Errorf("fired %d signal(s); want 0 — the ledger reads 150 against a threshold of 100 and the report decides nothing (outbox=%v)",
			got-preCount, fires.fired)
	}

	var (
		class      string
		epBin      int64
		edge, core int
	)
	err := db.QueryRow(`SELECT op, bin_id, (detail->>'edge_count')::int, (detail->>'core_count')::int
		FROM bin_uop_exception
		WHERE kind = 'report_divergence' AND actor = $1 AND recovered_at IS NULL`, stationID).
		Scan(&class, &epBin, &edge, &core)
	if err != nil {
		t.Fatalf("no open report_divergence episode for the carrier: %v", err)
	}
	if class != "count" || epBin != bin.ID || edge != 10 || core != 150 {
		t.Errorf("episode = %s bin %d edge %d core %d, want count bin %d edge 10 core 150", class, epBin, edge, core, bin.ID)
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

	// DB truth is deeply negative (the Springfield SYN-PART07A.06 total): a bin
	// carrying -443 makes SystemUOPForPayload return -443.
	seedBinWithUOP(t, db, payload, -443)

	m := eng.thresholdMonitor

	fires := captureThresholdFires(t, eng)
	preCount := fires.count(stationID)

	// Drive the hot path the way a real delta would: the monitor re-reads the
	// authoritative sum (-443), which is below threshold and must be acted on.
	m.OnBinUOPDelta(payload, -1)

	time.Sleep(300 * time.Millisecond)

	if got := fires.count(stationID); got <= preCount {
		t.Errorf("negative in-loop total produced no LoopBelowThresholdSignal; want at least one — a wrong count must not starve the line (outbox=%v)",
			fires.fired)
	}
}

// TestThresholdMonitor_Resync_EngagesAndFiresSeededBinding pins the seed-ordering
// fix. A demand_registry binding written OUT-OF-BAND (seeddev writes it
// directly; the Edge pushes no claim config over the wire) is
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

	fires := captureThresholdFires(t, eng)
	preCount := fires.count(stationID)

	// The Edge (re)connects → Resync. No diff is available, so only Resync can
	// engage the binding and fire it.
	eng.thresholdMonitor.Resync(stationID)

	deadline := time.Now().Add(2 * time.Second)
	var hit *firedBinding
	for time.Now().Before(deadline) {
		if fires.count(stationID) > preCount {
			hit = fires.find(stationID)
			if hit != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hit == nil {
		t.Fatalf("expected Resync to fire LoopBelowThresholdSignal to %s, outbox=%v", stationID, fires.fired)
	}
	if hit.PayloadCode != payload || hit.CoreNodeName != loader || hit.Threshold != 50 {
		t.Errorf("signal = payload=%q node=%q threshold=%d, want %s/%s/50", hit.PayloadCode, hit.CoreNodeName, hit.Threshold, payload, loader)
	}

	// Station scoping: Resync of a DIFFERENT station must not fire this binding.
	base := fires.count(stationID)
	eng.thresholdMonitor.Resync("some-other-station")
	time.Sleep(200 * time.Millisecond)
	if got := fires.count(stationID); got != base {
		t.Errorf("Resync(other-station) fired %s's binding (%d → %d)", stationID, base, got)
	}
}

// ── what "it fired" means now ────────────────────────────────────────────────
//
// THIS SUITE CHANGED WHAT IT WATCHES, NOT WHAT IT ASSERTS. Every test here asks
// the same question it always did — did the monitor decide to replenish this
// binding, and with what reading — and every one still asks it. What moved is
// where the answer is observed.
//
// It used to scan the outbox for a LoopBelowThresholdSignal envelope, because
// the decision was a message to the Edge and the Edge then worked out how many
// carriers were needed and where they went. That split is what the cutover
// ended: Core makes the whole decision and creates the orders itself, so there
// is no signal to scan for. Reading the outbox for orders instead would be the
// obvious substitute and it is the wrong one — it would make every test in this
// file depend on a full loader configuration, a payload capacity, and free
// windows, none of which any of them is about, and a fixture gap would then
// read as "the monitor did not fire" when the monitor fired perfectly.
//
// So the tests watch the fire decision directly, through the hook the monitor
// already carries for exactly this. The reading, the threshold, the binding and
// the reason are all on the decision; what happens downstream of it belongs to
// ReplenishLoader's own tests.

// firedBinding is one decision the monitor made.
type firedBinding struct {
	StationID    string
	CoreNodeName string
	PayloadCode  string
	Threshold    int
	CurrentUOP   int
	Reason       string
	OriginID     string
}

// fireLog records the monitor's decisions. Concurrency-safe: the startup sweep
// runs on its own goroutine.
type fireLog struct {
	mu    sync.Mutex
	fired []firedBinding
}

// captureThresholdFires installs the recorder on a started engine's monitor.
// Call after eng.Start(), before the action under test.
func captureThresholdFires(t *testing.T, eng *Engine) *fireLog {
	t.Helper()
	m := eng.ThresholdMonitor()
	if m == nil {
		t.Fatal("engine has no threshold monitor")
	}
	fl := &fireLog{}
	m.fireHook = func(b thresholdEntry, total int, reason, originID string) {
		fl.mu.Lock()
		defer fl.mu.Unlock()
		fl.fired = append(fl.fired, firedBinding{
			StationID: b.stationID, CoreNodeName: b.coreNodeName, PayloadCode: b.payloadCode,
			Threshold: b.threshold, CurrentUOP: total, Reason: reason, OriginID: originID,
		})
	}
	return fl
}

// count returns how many decisions have been made for a station.
func (f *fireLog) count(stationID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, b := range f.fired {
		if b.StationID == stationID {
			n++
		}
	}
	return n
}

// find returns the most recent decision for a station, or nil.
func (f *fireLog) find(stationID string) *firedBinding {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.fired) - 1; i >= 0; i-- {
		if f.fired[i].StationID == stationID {
			b := f.fired[i]
			return &b
		}
	}
	return nil
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
	// in-loop total negative — the Springfield SYN-PART07A.06 shape.
	seedBinWithUOP(t, db, payload, -443)

	fires := captureThresholdFires(t, eng)
	preCount := fires.count(stationID)

	// Drive the sweep directly rather than waiting out Run()'s 3s grace.
	eng.thresholdMonitor.startupSweep(context.Background())

	if got := fires.count(stationID); got <= preCount {
		t.Errorf("startup sweep emitted no signal on a negative in-loop total; want at least one — a restart is what an operator does BECAUSE the counts look wrong, and it must not leave the line unserved (outbox=%v)",
			fires.fired)
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
