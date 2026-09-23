//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/fleet/simulator"
	"shingocore/messaging"
	"shingocore/store"
	"shingocore/store/demands"
)

// demand_stale_station_test.go — what a SILENT EDGE does to the demand Core is
// already holding for it.
//
// Core holds the last information it had and reconciles when the Edge comes
// back. `demand_registry` is Core's own derivation from Core's own loader
// aggregate — the Edge pushes no claim config over the wire — so an Edge going
// quiet is not evidence that any of it changed, and Core deleting it on the
// strength of that silence invents a config withdrawal that nobody performed.
// The demand it ends may still have robots driving orders against it.
//
// These tests drive the REAL stale pass (messaging.CoreHandler.SweepStaleEdges)
// rather than a local copy of its body, because a copy pins the copy. The
// handler is built WITHOUT a threshold monitor on purpose: the monitor wire on
// this path exists only to tell the monitor about a registry wipe, so a test
// that installed it would be pinning the very thing being removed. A nil
// monitor is a supported configuration of the handler either way.

// staleSweep runs one production stale-edge pass: mark every edge that has
// missed its window, and notify it. A one-minute threshold with the fixtures
// below (which age the heartbeat by half an hour) means the pass has an
// unambiguous answer rather than one that depends on how long the test took.
func staleSweep(t *testing.T, db *store.DB) {
	t.Helper()
	h := messaging.NewCoreHandler(db, nil, "test-core", "shingo.dispatch", nil)
	h.StaleEdgeThreshold = time.Minute
	h.SweepStaleEdges()
}

// registryRows counts what Core still believes about a station's replenishment
// config. Zero is the state this lane exists to stop producing, so the count
// has to be read rather than inferred from something downstream of it.
func registryRows(t *testing.T, db *store.DB, stationID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM demand_registry WHERE station_id = $1`,
		stationID).Scan(&n); err != nil {
		t.Fatalf("count demand_registry rows for %s: %v", stationID, err)
	}
	return n
}

// staleNotifications counts the edge.stale announcements queued for a station.
// The announcement is the half of the stale path that STAYS, and a test that
// only asserted what no longer happens would pass just as well against a pass
// that had been deleted outright.
func staleNotifications(t *testing.T, db *store.DB, stationID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE msg_type = $1 AND station_id = $2`,
		"data."+protocol.SubjectEdgeStale, stationID).Scan(&n); err != nil {
		t.Fatalf("count stale notifications for %s: %v", stationID, err)
	}
	return n
}

// ordersForOrigin counts the orders attributed to one episode — the work that
// is already in flight against the demand, and the reason closing it on a
// silence is not a bookkeeping matter.
func ordersForOrigin(t *testing.T, db *store.DB, originID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE origin_id = $1`, originID).Scan(&n); err != nil {
		t.Fatalf("count orders for origin %s: %v", originID, err)
	}
	return n
}

// monitorHoldsBinding reports whether the monitor would evaluate the station's
// binding — that is, whether the monitored lookup it reads on every
// evaluation still returns it. There is no second record to check: the monitor
// keeps no copy of the registry.
func monitorHoldsBinding(m *ThresholdMonitor, b thresholdEntry) bool {
	entries, err := m.eng.db.LookupDemandThresholdsByPayload(b.payloadCode)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.StationID == b.stationID && e.CoreNodeName == b.coreNodeName {
			return true
		}
	}
	return false
}

// A SILENT EDGE IS NOT A WITHDRAWN CONFIG.
//
// The station has an open threshold episode and a live order running against
// it. It then stops heartbeating — which says nothing whatsoever about its
// loader config, because that config is Core's own derivation from Core's own
// aggregate and the Edge never had a say in it.
//
// So the pass marks the edge stale and announces it, and that is all. The rows
// stay, the monitor's bindings stay, the episode stays open, and the order that
// is already moving material keeps the demand it belongs to. Sweeping
// repeatedly changes none of it: the count is the measurement, because the
// failure this pins against is an oscillator — mint, close, re-arm, mint — and
// counting only OPEN episodes would report a perfectly healthy plant while it
// ran.
func TestStaleStation_KeepsRegistryEpisodeAndOrders(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SD1"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_401", payload, 18)
	registerBinding(t, db, b)
	registerActiveEdge(t, db, b.stationID)

	m.Resync(b.stationID)
	open := openThresholdEpisodes(t, db)
	if len(open) != 1 {
		t.Fatalf("Resync opened %d episodes for one below-threshold binding, want 1", len(open))
	}
	originID := open[0].OriginID
	insertOrderWithOrigin(t, db, originID, protocol.OriginClassAttached, time.Minute)

	silenceEdge(t, db, b.stationID, 30*time.Minute)
	staleSweep(t, db)

	// The detection and the announcement are not what this lane removes, and a
	// pass that silently stopped running would satisfy every other assertion
	// here.
	if st := mustEdgeStatus(t, db, b.stationID); st != "stale" {
		t.Fatalf("edge status = %q after the stale pass, want %q — the pass did not run, so nothing below is evidence",
			st, "stale")
	}
	if got := staleNotifications(t, db, b.stationID); got != 1 {
		t.Errorf("%d edge.stale notifications queued, want 1 — a stale edge is still announced", got)
	}

	if got := registryRows(t, db, b.stationID); got != 1 {
		t.Errorf("the stale pass left %d demand_registry rows for %s, want 1 — silence is not a config change, and nobody withdrew this loader",
			got, b.stationID)
	}
	if !monitorHoldsBinding(m, b) {
		t.Error("the stale pass dropped the station's binding from what the monitor evaluates — the station stops being replenished the moment its link flaps")
	}

	// Deltas keep arriving and the reconciling sweep keeps running underneath
	// them. Four passes is enough: an oscillator adds a row per pass.
	for i := 0; i < 4; i++ {
		m.evaluatePayload(payload, "below_threshold")
		m.reconcileThresholdBindings()
	}

	rows := episodesForPayload(t, db, payload)
	if len(rows) != 1 {
		t.Fatalf("a stale station produced %d episode rows across 4 sweep passes, want 1 — the demand never ended, so nothing should have been written",
			len(rows))
	}
	if !rows[0].open {
		t.Errorf("the episode closed %q by %q while its Edge was merely quiet — no threshold was removed, and its order is still running",
			rows[0].closeReason, rows[0].closedBy)
	}
	if rows[0].originID != originID {
		t.Errorf("the open episode is %s, want the original %s", rows[0].originID, originID)
	}
	if got := ordersForOrigin(t, db, originID); got != 1 {
		t.Errorf("%d orders attributed to %s, want 1 — the in-flight work must not lose the demand it belongs to", got, originID)
	}
}

// THE RECONCILE, AND IT IS A NO-OP WHEN NOTHING MOVED.
//
// The Edge comes back and HandleEdgeRegister re-derives the station's registry
// from the aggregate and resyncs the monitor. The aggregate did not change
// while the station was down, so the derived rows are identical — and the
// demand that was open before the outage is the SAME demand, not a new one.
//
// The origin id is the assertion that matters. A close-and-reopen across an
// outage would split one demand into two rows and reset its clock, which is
// exactly the reading an operator would use to decide the line had been short
// twice.
func TestStaleStation_ReRegisterUnchangedKeepsTheSameEpisode(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SD2"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_402", payload, 18)
	registerBinding(t, db, b)
	registerActiveEdge(t, db, b.stationID)

	m.Resync(b.stationID)
	open := openThresholdEpisodes(t, db)
	if len(open) != 1 {
		t.Fatalf("Resync opened %d episodes, want 1", len(open))
	}
	originID := open[0].OriginID

	silenceEdge(t, db, b.stationID, 30*time.Minute)
	staleSweep(t, db)

	// The Edge returns. This is HandleEdgeRegister's pair of calls:
	// BuildDemandRegistryFromAggregate's entries handed to SyncDemandRegistry,
	// then Resync. The aggregate derivation itself stands outside the test,
	// which only cares that it produces what it produced before.
	registerBinding(t, db, b)
	m.Resync(b.stationID)

	rows := episodesForPayload(t, db, payload)
	if len(rows) != 1 {
		t.Fatalf("an outage with no config change produced %d episode rows, want 1 — one demand, one row", len(rows))
	}
	if !rows[0].open {
		t.Errorf("the episode closed %q by %q across an outage that changed nothing", rows[0].closeReason, rows[0].closedBy)
	}
	if rows[0].originID != originID {
		t.Errorf("the station came back holding episode %s, want the one it left open, %s — the outage split one demand in two",
			rows[0].originID, originID)
	}
	if held, err := db.OpenOriginForKey(placeKey(b.coreNodeName, b.payloadCode)); err != nil || held != originID {
		t.Errorf("the place's open episode is %q after the reconnect (err %v), want %s — signals would fire with the wrong demand attached or none at all",
			held, err, originID)
	}
}

// A CONFIG EDIT WHILE THE STATION IS DOWN IS STILL A CONFIG EDIT.
//
// Nothing about the station being quiet stops a loader's threshold being
// retyped on the Core UI: the re-derive targets every station in the registry,
// and with the rows intact this one is among them. The denominator moved, so
// the episode ends `threshold_changed` and a fresh one opens against the new
// number — carrying one episode across the change would make its cost_ratio a
// division by a threshold that was not in force for most of its life.
func TestStaleStation_ThresholdEditedWhileDownClosesAsChanged(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SD3"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_403", payload, 18)
	registerBinding(t, db, b)
	registerActiveEdge(t, db, b.stationID)

	m.Resync(b.stationID)
	open := openThresholdEpisodes(t, db)
	if len(open) != 1 {
		t.Fatalf("Resync opened %d episodes, want 1", len(open))
	}
	originID := open[0].OriginID

	silenceEdge(t, db, b.stationID, 30*time.Minute)
	staleSweep(t, db)

	// The edit, as loader_service.rederive performs it: the aggregate's new
	// value is derived into the registry and the change list drives the monitor.
	changes, err := db.SyncDemandRegistry(b.stationID, []demands.RegistryEntry{{
		StationID:             b.stationID,
		CoreNodeName:          b.coreNodeName,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 60,
	}})
	if err != nil {
		t.Fatalf("re-derive with the edited threshold: %v", err)
	}
	m.OnThresholdChanges(changes)

	got := mustGetOrigin(t, db, originID)
	if got.ClosedAt == nil {
		t.Fatal("the episode survived a threshold edit — its cost_ratio would be divided by a number that is no longer in force")
	}
	if got.CloseReason != protocol.CloseReasonThresholdChanged {
		t.Errorf("close_reason = %q, want %q — the need did not recover and the binding did not vanish",
			got.CloseReason, protocol.CloseReasonThresholdChanged)
	}
	if got.ClosedBy != protocol.ClosedByNotification {
		t.Errorf("closed_by = %q, want %q — the edit knows what it did and must say so itself",
			got.ClosedBy, protocol.ClosedByNotification)
	}
	stillOpen := openThresholdEpisodes(t, db)
	if len(stillOpen) != 1 || stillOpen[0].OriginID == originID {
		t.Fatalf("after the edit %d episodes are open, want exactly 1 and not the closed %s — the place is still short at the new threshold",
			len(stillOpen), originID)
	}
}

// RETIRING THE LOADER IS THE ONE THING THAT DOES END THE DEMAND.
//
// This is the case the stale-edge wipe used to imitate, and the difference is
// the whole lane: here a person actually removed the config, so the episode
// ends and the reason says which fact it was — the binding went away, not the
// denominator moved and not the line got its material.
//
// ONCE. A withdrawn config is one ending, and `closed_by=notification` is what
// makes the sweep's share of the closing measurable: if the edit leaves the
// close to the sweep, the demand surface cannot tell a plant whose notification
// paths all work from one where they have silently stopped firing.
func TestStaleStation_LoaderRetiredWhileDownClosesAsRemovedOnce(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SD4"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_404", payload, 18)
	registerBinding(t, db, b)
	registerActiveEdge(t, db, b.stationID)

	m.Resync(b.stationID)
	if open := openThresholdEpisodes(t, db); len(open) != 1 {
		t.Fatalf("Resync opened %d episodes, want 1", len(open))
	}

	silenceEdge(t, db, b.stationID, 30*time.Minute)
	staleSweep(t, db)

	// The loader is deleted on the Core UI. LoaderService.Delete re-derives, the
	// aggregate no longer produces an entry for this station, and the change
	// list carries the binding's disappearance to the monitor.
	changes, err := db.SyncDemandRegistry(b.stationID, nil)
	if err != nil {
		t.Fatalf("re-derive after retiring the loader: %v", err)
	}
	m.OnThresholdChanges(changes)

	rows := episodesForPayload(t, db, payload)
	if len(rows) != 1 {
		t.Fatalf("retiring one loader produced %d episode rows, want 1 — one withdrawn config is one ending", len(rows))
	}
	got := rows[0]
	if got.open {
		t.Fatal("the episode is still open after its binding was retired — nothing will ever close it, and an episode that never closes is the loudest row on the demand surface")
	}
	if got.closeReason != protocol.CloseReasonThresholdRemoved {
		t.Errorf("close_reason = %q, want %q — the binding went away; it did not recover and its denominator did not move",
			got.closeReason, protocol.CloseReasonThresholdRemoved)
	}
	if got.closedBy != protocol.ClosedByNotification {
		t.Errorf("closed_by = %q, want %q — the edit that removed the binding knows it removed it",
			got.closedBy, protocol.ClosedByNotification)
	}
	if monitorHoldsBinding(m, b) {
		t.Error("the retired binding is still monitored — the next delta mints against config that no longer exists")
	}
}
