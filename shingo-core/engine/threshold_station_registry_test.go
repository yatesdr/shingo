//go:build docker

package engine

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/payloads"
)

// threshold_station_registry_test.go — what happens to the monitor's MEMORY
// when a station's demand_registry loses every binding it had.
//
// THE INCIDENT THAT TAUGHT THIS. A station's rows went away and nothing told
// the monitor. Its bindings live in thresholdsByPayload, in memory, so
// evaluatePayload kept minting threshold episodes for config that no longer
// existed; the reconciling sweep closed each mint `threshold_removed` on its
// next pass, and closeThresholdEpisodeRef cleared belowThresholdSince, which
// re-armed the mint. Springfield logged 1293 rows for one station over two days
// — one demand rendered as a stream of instantaneous ones, which is the exact
// failure the episode grain was built to end.
//
// What emptied the rows there was the stale-edge reaper, and that path is gone:
// an Edge going quiet says nothing about config Core derives for itself, so the
// stale pass no longer touches the registry (demand_stale_station_test.go pins
// that it does not). What remains is the case where the rows really did go —
// somebody retired the loader — and the monitor still has to find out. That is
// the whole-station grain and it is what these tests are about;
// threshold_episodes_test.go already pins the per-binding grain.

// emptyStationRegistry removes every binding a station has in one transaction
// and discards the change list — the shape any writer leaves behind when it
// does not tell the monitor what it did.
func emptyStationRegistry(t *testing.T, db *store.DB, stationID string) {
	t.Helper()
	if _, err := db.SyncDemandRegistry(stationID, nil); err != nil {
		t.Fatalf("empty registry for %s: %v", stationID, err)
	}
}

// stationBinding is episodeBinding's multi-station sibling: it names the
// station rather than taking threshold_episodes_test.go's single hard-coded
// one, because every test here is about telling two stations apart.
func stationBinding(t *testing.T, eng *Engine, stationID, node, payload string, capacity int) thresholdEntry {
	t.Helper()
	if capacity > 0 {
		// LOOKUP THEN CREATE, because two bindings in the same test share one
		// payload and the second call would otherwise be a duplicate-key error
		// that has to be swallowed. Swallowing it would also swallow a real
		// failure to write the catalog row, which is the denominator every
		// expected_orders in this file is computed from.
		//
		// payloads.GetByCode reports a MISS as sql.ErrNoRows rather than a nil
		// payload, so that one error is the "not there yet" answer and every
		// other one is a fault.
		existing, err := eng.db.GetPayloadByCode(payload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			testutil.MustNoErr(t, err, "look up payload")
		}
		if existing == nil {
			testutil.MustNoErr(t, eng.db.CreatePayload(&payloads.Payload{Code: payload, UOPCapacity: capacity}), "create payload")
		}
	}
	return thresholdEntry{
		stationID: stationID, coreNodeName: node, payloadCode: payload, threshold: 100,
	}
}

// episodeRow is one demand_origins row, open or closed — what a test that
// counts MINTS needs and ListOpenThresholdEpisodes cannot give it. The
// oscillator's damage is rows that were opened and closed again, so a query
// that only sees open ones would report a perfectly healthy plant.
type episodeRow struct {
	originID    string
	stationID   string
	closeReason string
	closedBy    string
	open        bool
}

// openThresholdEpisodes is the checked form of the lookup every test here
// opens with. The error is not noise: a failed read returns an empty slice,
// which reads as "no episode was opened" and would blame the monitor for a
// database problem.
func openThresholdEpisodes(t *testing.T, db *store.DB) []store.DemandOrigin {
	t.Helper()
	open, err := db.ListOpenThresholdEpisodes()
	testutil.MustNoErr(t, err, "list open threshold episodes")
	return open
}

func episodesForPayload(t *testing.T, db *store.DB, payload string) []episodeRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT origin_id, station_id, COALESCE(close_reason, ''), COALESCE(closed_by, ''), closed_at IS NULL
		  FROM demand_origins
		 WHERE payload_code = $1
		 ORDER BY opened_at, origin_id`, payload)
	if err != nil {
		t.Fatalf("list episodes for %s: %v", payload, err)
	}
	defer rows.Close()
	var out []episodeRow
	for rows.Next() {
		var e episodeRow
		if err := rows.Scan(&e.originID, &e.stationID, &e.closeReason, &e.closedBy, &e.open); err != nil {
			t.Fatalf("scan episode: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate episodes: %v", err)
	}
	return out
}

// hideDemandRegistry makes every demand_registry read FAIL, and restores the
// table when the test ends.
//
// A read error and an empty result are the same shape to a caller that does not
// look, and the difference between them is the whole plant: "this station has no
// bindings" closes every episode it has, while "Postgres blipped" must close
// nothing. There is no other way to produce a read error against a live table,
// and each test gets its own cloned database, so renaming it out from under the
// query is safe and isolated.
func hideDemandRegistry(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE demand_registry RENAME TO demand_registry_hidden`); err != nil {
		t.Fatalf("hide demand_registry: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`ALTER TABLE demand_registry_hidden RENAME TO demand_registry`); err != nil {
			t.Logf("restore demand_registry: %v", err)
		}
	})
}

// THE ADD PATH, which must keep working. Resync is what turns a registry
// written out-of-band — seeddev, or the aggregate derivation on an Edge
// register — into live monitor bindings without a Core restart. Every change
// this lane makes is on the other side of the same function, so the add half is
// pinned here at the EPISODE grain (the existing PG test watches fire
// decisions).
func TestThresholdEpisode_ResyncEngagesAStationAndMints(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_101", "PANEL-SR1", 18)
	registerBinding(t, db, b)

	m.Resync(b.stationID)

	open := openThresholdEpisodes(t, db)
	if len(open) != 1 {
		t.Fatalf("Resync engaged %d episodes for one below-threshold binding, want 1", len(open))
	}
	key := bindingKey(b.stationID, b.coreNodeName, b.payloadCode)
	if held := m.currentThresholdOrigin(key); held != open[0].OriginID {
		t.Errorf("monitor holds %q, the open episode is %s — signals would fire with no demand attached",
			held, open[0].OriginID)
	}
	m.mu.Lock()
	_, monitored := m.thresholdsByPayload[b.payloadCode]
	m.mu.Unlock()
	if !monitored {
		t.Error("Resync opened an episode but did not leave the binding in thresholdsByPayload — the next delta would not evaluate it")
	}
}

// A READ FAILURE IS NOT AN EMPTY BINDING SET, in Resync.
//
// This is the direction that takes the plant down. Resync's job is to make the
// monitor's memory agree with demand_registry for one station; if a transient
// Postgres error reads as "the station has no bindings", one blip during an
// Edge reconnect closes every open demand that station has and stops
// replenishing it until something else re-engages it.
func TestThresholdEpisode_ResyncReadErrorIsNotAnEmptyBindingSet(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_102", "PANEL-SR2", 18)
	registerBinding(t, db, b)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)
	open := openThresholdEpisodes(t, db)
	if len(open) != 1 {
		t.Fatalf("no episode opened: %d", len(open))
	}
	originID := open[0].OriginID
	m.mu.Lock()
	m.thresholdsByPayload[b.payloadCode] = []thresholdEntry{b}
	m.mu.Unlock()

	hideDemandRegistry(t, db)
	m.Resync(b.stationID)

	got := mustGetOrigin(t, db, originID)
	if got.ClosedAt != nil {
		t.Errorf("a demand_registry read error closed a live episode (reason=%q, by=%q) — a blip must not look like a withdrawn config",
			got.CloseReason, got.ClosedBy)
	}
	m.mu.Lock()
	_, monitored := m.thresholdsByPayload[b.payloadCode]
	m.mu.Unlock()
	if !monitored {
		t.Error("a demand_registry read error dropped the binding from thresholdsByPayload — the station would stop being replenished on a transient error")
	}
}

// The same rule, on the sweep. reconcileThresholdBindings already documents it
// ("A READ FAILURE IS NOT AN EMPTY BINDING SET ... the sweep's worst possible
// failure mode, and one that would look exactly like a very effective sweep")
// and nothing asserted it. The lane widens what closes episodes, so the floor
// under the sweep gets a test before anything moves.
func TestThresholdEpisode_SweepReadErrorIsNotAnEmptyBindingSet(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_103", "PANEL-SR3", 18)
	registerBinding(t, db, b)

	m.checkBindings([]thresholdEntry{b}, 40, "below_threshold", false)
	open := openThresholdEpisodes(t, db)
	if len(open) != 1 {
		t.Fatalf("no episode opened: %d", len(open))
	}
	originID := open[0].OriginID

	hideDemandRegistry(t, db)
	if closed := m.reconcileThresholdBindings(); closed != 0 {
		t.Errorf("the sweep closed %d episodes on a demand_registry read error, want 0", closed)
	}

	if got := mustGetOrigin(t, db, originID); got.ClosedAt != nil {
		t.Errorf("the sweep closed a live episode on a read error (reason=%q, by=%q)", got.CloseReason, got.ClosedBy)
	}
}

// WITHDRAWING ONE STATION'S CONFIG MUST NOT TAKE ANOTHER STATION'S BINDING
// WITH IT.
//
// The monitor's binding cache is keyed by PAYLOAD, not by station, and
// LookupDemandThresholdsByPayload is global — so every rebuild of a payload
// touches every station that watches it. A withdrawal scoped to one station
// that rebuilds through that cache is one bad key comparison away from silently
// unmonitoring a healthy line at another loader, which reads as nothing at all:
// no error, no log, just a station that stops being replenished.
func TestThresholdEpisode_WithdrawingOneStationLeavesAnotherStationsBindingAlone(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)

	const payload = "PANEL-SR4"
	withdrawn := stationBinding(t, eng, "PLANT.LINE1", "SLN_104", payload, 18)
	survivor := stationBinding(t, eng, "PLANT.LINE2", "SLN_204", payload, 18)
	registerBinding(t, db, withdrawn)
	registerBinding(t, db, survivor)

	m.checkBindings([]thresholdEntry{withdrawn, survivor}, 40, "below_threshold", false)
	m.mu.Lock()
	m.thresholdsByPayload[payload] = []thresholdEntry{withdrawn, survivor}
	m.mu.Unlock()
	open := openThresholdEpisodes(t, db)
	if len(open) != 2 {
		t.Fatalf("two below-threshold bindings should open two episodes, got %d", len(open))
	}
	survivorKey := bindingKey(survivor.stationID, survivor.coreNodeName, survivor.payloadCode)
	survivorOrigin := m.currentThresholdOrigin(survivorKey)
	if survivorOrigin == "" {
		t.Fatal("no episode held for the survivor binding")
	}

	// The withdrawal, both halves: the rows go, and the monitor is told.
	emptyStationRegistry(t, db, withdrawn.stationID)
	m.Resync(withdrawn.stationID)

	if got := mustGetOrigin(t, db, survivorOrigin); got.ClosedAt != nil {
		t.Errorf("withdrawing %s closed %s's episode (reason=%q, by=%q) — a silent Edge took a healthy line's demand with it",
			withdrawn.stationID, survivor.stationID, got.CloseReason, got.ClosedBy)
	}
	if held := m.currentThresholdOrigin(survivorKey); held != survivorOrigin {
		t.Errorf("withdrawing %s dropped the monitor's hold on %s's episode: held %q, want %s",
			withdrawn.stationID, survivor.stationID, held, survivorOrigin)
	}
	m.mu.Lock()
	bindings := append([]thresholdEntry(nil), m.thresholdsByPayload[payload]...)
	m.mu.Unlock()
	found := false
	for _, te := range bindings {
		if te.stationID == survivor.stationID && te.coreNodeName == survivor.coreNodeName {
			found = true
		}
	}
	if !found {
		t.Errorf("withdrawing %s dropped %s's binding from thresholdsByPayload (%d left) — that station would stop being replenished with nothing logged",
			withdrawn.stationID, survivor.stationID, len(bindings))
	}
	// And the survivor is still EVALUATED, which is the part a dropped cache
	// entry hides: with the binding gone, evaluatePayload short-circuits before
	// the DB read and the line just goes quiet.
	m.evaluatePayload(payload, "below_threshold")
	if got := mustGetOrigin(t, db, survivorOrigin); got.ClosedAt != nil {
		t.Errorf("an evaluation after the withdrawal closed %s's episode (reason=%q)", survivor.stationID, got.CloseReason)
	}
	stillOpen := openThresholdEpisodes(t, db)
	survivors := 0
	for _, o := range stillOpen {
		if o.StationID == survivor.stationID {
			survivors++
		}
	}
	if survivors != 1 {
		t.Errorf("%s holds %d open episodes for %s, want exactly 1", survivor.stationID, survivors, payload)
	}
}

// THE OSCILLATOR.
//
// The station's demand_registry is emptied — the loader was retired. The
// monitor's bindings are in memory and survive the delete, so without a
// notification every subsequent delta re-evaluates a binding whose config no
// longer exists; the sweep closes each mint `threshold_removed`, and
// closeThresholdEpisodeRef clears belowThresholdSince on the way out, which
// re-arms the falling edge for the next delta. Springfield: 1293 rows over two
// days for one station.
//
// One withdrawn config is ONE ending. The assertion is therefore a row count,
// not a state: counting only open episodes would report a perfectly healthy
// plant, because every one of the 1293 was closed again immediately.
func TestThresholdEpisode_WithdrawnStationStopsMintingAndClosesExactlyOnce(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SR5"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_105", payload, 18)
	registerBinding(t, db, b)

	// Engage the binding the way production does — through the registry, not by
	// poking the cache — so the memory under test is the memory the plant has.
	m.Resync(b.stationID)
	if open := openThresholdEpisodes(t, db); len(open) != 1 {
		t.Fatalf("Resync opened %d episodes, want 1", len(open))
	}

	// The withdrawal, both halves.
	emptyStationRegistry(t, db, b.stationID)
	m.Resync(b.stationID)

	// The level is still below threshold and deltas keep arriving — the place is
	// short, it just has nothing configured to fill it any more — and the sweep
	// keeps running underneath them. Four
	// passes is enough: the oscillator adds a row per pass, so the count is the
	// measurement.
	for i := 0; i < 4; i++ {
		m.evaluatePayload(payload, "below_threshold")
		m.reconcileThresholdBindings()
	}

	rows := episodesForPayload(t, db, payload)
	if len(rows) != 1 {
		t.Fatalf("a withdrawn station minted %d episodes across 4 sweep passes, want 1 — one withdrawn config is one ending, and this is the Springfield 1293-row shape",
			len(rows))
	}
	got := rows[0]
	if got.open {
		t.Fatal("the withdrawn station's episode is still open — its binding is gone, so nothing will ever close it")
	}
	if got.closeReason != protocol.CloseReasonThresholdRemoved {
		t.Errorf("close_reason = %q, want %q — the need did not recover, it stopped being watched",
			got.closeReason, protocol.CloseReasonThresholdRemoved)
	}
	// CLOSED BY THE NOTIFICATION PATH, not the sweep. closed_by is what makes
	// the sweep's share of the closing measurable: if the withdrawal leaves the
	// close to the sweep, the surface cannot tell a plant whose notification
	// paths all work from one where they have silently stopped firing.
	if got.closedBy != protocol.ClosedByNotification {
		t.Errorf("closed_by = %q, want %q — whoever removed the binding knows it went away and must say so itself",
			got.closedBy, protocol.ClosedByNotification)
	}
	// And nothing is left in memory to mint against.
	m.mu.Lock()
	_, monitored := m.thresholdsByPayload[payload]
	m.mu.Unlock()
	if monitored {
		t.Error("the withdrawn station's binding is still in thresholdsByPayload — the next delta mints again")
	}
}

// THE RECOVERY HALF. A withdrawn station that comes back must be monitored again.
//
// The withdrawal ends the demand because the config went away; restoring the
// loader brings the config back, and the place is still hungry, so that is a NEW
// demand and it gets its own episode. Re-joining the closed one would make a
// single row span a gap in which nothing was being asked for.
//
// The registry write here is the one HandleEdgeRegister performs —
// BuildDemandRegistryFromAggregate's entries handed to SyncDemandRegistry —
// with the aggregate derivation itself standing outside the test, which only
// cares that the rows come back.
func TestThresholdEpisode_WithdrawnStationIsRestoredAndMintsAfresh(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())
	m := NewThresholdMonitor(eng)
	const payload = "PANEL-SR6"
	b := stationBinding(t, eng, "PLANT.LINE1", "SLN_106", payload, 18)
	registerBinding(t, db, b)

	m.Resync(b.stationID)
	open := openThresholdEpisodes(t, db)
	if len(open) != 1 {
		t.Fatalf("Resync opened %d episodes, want 1", len(open))
	}
	first := open[0].OriginID

	emptyStationRegistry(t, db, b.stationID)
	m.Resync(b.stationID)

	// The loader is put back: the aggregate re-derives the same binding and the
	// register path resyncs the monitor.
	registerBinding(t, db, b)
	m.Resync(b.stationID)

	rows := episodesForPayload(t, db, payload)
	if len(rows) != 2 {
		t.Fatalf("withdraw then restore produced %d episodes, want 2 (one ended by the withdrawal, one opened by the return)", len(rows))
	}
	var reopened int
	for _, r := range rows {
		if r.originID == first {
			if r.open {
				t.Error("the earlier episode is still open — the withdrawn config never ended")
			}
			if r.closeReason != protocol.CloseReasonThresholdRemoved {
				t.Errorf("the earlier episode closed %q, want %q", r.closeReason, protocol.CloseReasonThresholdRemoved)
			}
			continue
		}
		if r.open {
			reopened++
		}
	}
	if reopened != 1 {
		t.Fatalf("%d open episodes after the station came back, want exactly 1", reopened)
	}
	key := bindingKey(b.stationID, b.coreNodeName, b.payloadCode)
	if held := m.currentThresholdOrigin(key); held == "" || held == first {
		t.Errorf("after the station came back the monitor holds %q — it must hold the NEW episode, not the closed one", held)
	}
}
