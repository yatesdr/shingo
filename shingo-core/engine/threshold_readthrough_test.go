//go:build docker

package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/demands"
)

// threshold_readthrough_test.go — THE MONITOR'S BEHAVIOUR, DRIVEN ONLY
// THROUGH PRODUCTION DOORS.
//
// Every test here reaches the monitor through a door production uses — a
// delta, the boot pass, a config-edit change list, a reconnect — and plants
// any state a door cannot produce (a registry row deleted under it, an episode
// closed behind its back) by direct SQL. None of them reads or writes the
// monitor's private state, so the same file describes the monitor whether it
// holds copies of demand_registry and demand_origins or reads them.

const rtThreshold = 100

// rtRig is one monitored place: a binding registered in demand_registry, a
// catalog payload, and one line bin whose uop is the in-loop total.
type rtRig struct {
	t     *testing.T
	eng   *Engine
	m     *ThresholdMonitor
	sink  *logSink
	fires *fireLog
	b     thresholdEntry
	binID int64
	clock *rtClock
}

// rtClock is the monitor's clock, moved by hand so debounce windows are
// deterministic. Guarded because the concurrency test reads it from two
// goroutines.
type rtClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *rtClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *rtClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newRTRig(t *testing.T, payload string, uop int) *rtRig {
	t.Helper()
	db := testDB(t)
	sink := &logSink{}
	eng := newLoggingEngine(t, db, sink)
	clk := &rtClock{now: time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)}
	m := eng.thresholdMonitor
	m.now = clk.Now
	fires := captureThresholdFires(t, eng)

	b := stationBinding(t, eng, "PLANT.RT", "SLN_RT", payload, 18)
	b.threshold = rtThreshold
	registerBinding(t, db, b)

	sd := testdb.SetupStandardData(t, db)
	bin := testdb.CreateBinAtNode(t, db, payload, sd.LineNode.ID, "BIN-"+payload)
	r := &rtRig{t: t, eng: eng, m: m, sink: sink, fires: fires, b: b, binID: bin.ID, clock: clk}
	r.setUOP(uop)
	return r
}

func (r *rtRig) setUOP(n int) {
	r.t.Helper()
	_, err := r.eng.db.Exec(`UPDATE bins SET uop_remaining=$1 WHERE id=$2`, n, r.binID)
	testutil.MustNoErr(r.t, err, "set uop")
}

// boot is the startup sweep, run synchronously.
func (r *rtRig) boot() { r.m.startupSweep(context.Background()) }

// delta is one bin-UOP delta for the payload — the hot path's door.
func (r *rtRig) delta() { r.m.OnBinUOPDelta(r.b.payloadCode, -1) }

// pastDebounce moves the monitor's clock beyond the debounce window.
func (r *rtRig) pastDebounce() { r.clock.advance(thresholdDebounceWindow + time.Second) }

// rows is every threshold episode for the payload, oldest first.
func (r *rtRig) rows() []episodeRow {
	r.t.Helper()
	return episodesForPayload(r.t, r.eng.db, r.b.payloadCode)
}

func (r *rtRig) openRows() []episodeRow {
	var out []episodeRow
	for _, e := range r.rows() {
		if e.open {
			out = append(out, e)
		}
	}
	return out
}

// failedMints counts mint attempts the partial unique index refused, or that
// failed for any other reason — every one is a mint the monitor should not
// have tried, or could not make.
func (r *rtRig) failedMints() int { return r.sink.countContaining("open demand episode key=") }

// fired is every fire decision for the rig's station, in order.
func (r *rtRig) fired() []firedBinding {
	r.fires.mu.Lock()
	defer r.fires.mu.Unlock()
	var out []firedBinding
	for _, f := range r.fires.fired {
		if f.PayloadCode == r.b.payloadCode {
			out = append(out, f)
		}
	}
	return out
}

// ── (1) the fire decision ───────────────────────────────────────────────────

// BELOW FIRES, AT AND ABOVE HOLD. The boot pass runs at a healthy level first,
// so the monitor has seen the binding before the level moves, then a delta
// carries the new level in.
func TestReadThrough_FireGateBelowAtAbove(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		uop      int
		wantFire bool
	}{
		{"below", 40, true}, {"at", rtThreshold, false}, {"above", 150, false}, {"negative", -443, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRTRig(t, "PANEL-RT-G-"+c.name, 500)
			r.boot()
			r.setUOP(c.uop)
			r.delta()
			fired := len(r.fired())
			opened := len(r.openRows())
			if c.wantFire && (fired != 1 || opened != 1) {
				t.Errorf("uop=%d: fired=%d open=%d, want 1 and 1", c.uop, fired, opened)
			}
			if !c.wantFire && (fired != 0 || opened != 0) {
				t.Errorf("uop=%d: fired=%d open=%d, want neither", c.uop, fired, opened)
			}
		})
	}
}

// DEBOUNCE HOLDS THE FIRE, NEVER THE OPEN. A place that falls, recovers and
// falls again inside the debounce window is two demands and one action: the
// second episode is recorded and its fire is held.
func TestReadThrough_DebounceSuppressesTheFireNotTheOpen(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-DB", 500)
	r.boot()

	// One second between steps: inside the debounce window, and far enough
	// apart that opened_at orders the two episodes.
	r.setUOP(40)
	r.delta()
	r.clock.advance(time.Second)
	r.setUOP(150)
	r.delta()
	r.clock.advance(time.Second)
	r.setUOP(40)
	r.delta()

	rows := r.rows()
	if len(rows) != 2 {
		t.Fatalf("%d episodes, want 2 — fall, recover, fall is two demands", len(rows))
	}
	if rows[0].open || rows[0].closeReason != protocol.CloseReasonRecovered {
		t.Errorf("first episode open=%v reason=%q, want closed recovered", rows[0].open, rows[0].closeReason)
	}
	if !rows[1].open {
		t.Error("the second fall inside the debounce window must still open an episode")
	}
	if got := len(r.fired()); got != 1 {
		t.Errorf("fired %d time(s) inside one debounce window, want 1", got)
	}
}

// A COLD START GRANTS warmUpFloor UN-DEBOUNCED FIRES PER HUNGRY PLACE. The boot
// pass fires once, the next delta inside the debounce window fires again on
// the second warm-up, and the third is debounced.
func TestReadThrough_WarmUpAtColdStart(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-WU", 40)
	r.boot()
	r.delta()
	r.delta()
	if got := len(r.fired()); got != warmUpFloor {
		t.Errorf("fired %d time(s) across boot and two deltas inside one window, want %d (the warm-up floor)", got, warmUpFloor)
	}
	if n := len(r.rows()); n != 1 {
		t.Errorf("%d episodes, want 1", n)
	}
}

// THRESHOLD 0 IS THE OPT-OUT. A registry row with no threshold is not watched:
// nothing fires and nothing opens however empty the place is.
func TestReadThrough_ThresholdZeroIsOptedOut(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-OPT", 0)
	opted := r.b
	opted.threshold = 0
	registerBinding(t, r.eng.db, opted)
	r.boot()
	r.delta()
	if fired, rows := len(r.fired()), len(r.rows()); fired != 0 || rows != 0 {
		t.Errorf("an opted-out place fired %d time(s) and opened %d episode(s), want neither", fired, rows)
	}
}

// ── (2) episode edges ───────────────────────────────────────────────────────

// ONE OPEN EPISODE PER PLACE ACROSS REPEATS, AND EVERY FIRE CARRIES IT.
func TestReadThrough_OneOpenPerPlaceAndFiresCarryIt(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-ONE", 500)
	r.boot()
	r.setUOP(40)
	for i := 0; i < 5; i++ {
		r.delta()
		r.pastDebounce()
	}
	open := r.openRows()
	if len(open) != 1 || len(r.rows()) != 1 {
		t.Fatalf("%d open of %d episodes across five evaluations, want 1 of 1", len(open), len(r.rows()))
	}
	if n := r.failedMints(); n != 0 {
		t.Errorf("%d failed mint(s), want 0", n)
	}
	fired := r.fired()
	if len(fired) != 5 {
		t.Fatalf("fired %d time(s) across five windows, want 5", len(fired))
	}
	for i, f := range fired {
		if f.OriginID != open[0].originID {
			t.Errorf("fire %d carried origin %q, want the open episode %s", i, f.OriginID, open[0].originID)
		}
	}
}

// THE RISING EDGE CLOSES `recovered`, by the notification path.
func TestReadThrough_RisingEdgeClosesRecovered(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-UP", 500)
	r.boot()
	r.setUOP(40)
	r.delta()
	r.setUOP(150)
	r.delta()
	rows := r.rows()
	if len(rows) != 1 || rows[0].open {
		t.Fatalf("rows=%+v, want one closed episode", rows)
	}
	if rows[0].closeReason != protocol.CloseReasonRecovered || rows[0].closedBy != protocol.ClosedByNotification {
		t.Errorf("closed %q by %q, want %q by %q", rows[0].closeReason, rows[0].closedBy,
			protocol.CloseReasonRecovered, protocol.ClosedByNotification)
	}
}

// A THRESHOLD EDIT CLOSES `threshold_changed` AND THE STILL-HUNGRY PLACE
// REOPENS against the new threshold, through the config-edit door.
func TestReadThrough_ThresholdChangeClosesAndReopens(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-TC", 500)
	r.boot()
	r.setUOP(40)
	r.delta()

	r.clock.advance(time.Second) // so opened_at orders the two episodes
	edited := r.b
	edited.threshold = 150
	registerBinding(t, r.eng.db, edited)
	r.m.OnThresholdChanges([]demands.RegistryChange{{
		StationID: r.b.stationID, CoreNodeName: r.b.coreNodeName, PayloadCode: r.b.payloadCode,
		OldThreshold: rtThreshold, NewThreshold: 150,
	}})

	rows := r.rows()
	if len(rows) != 2 {
		t.Fatalf("%d episodes, want 2 (the edited one and its successor)", len(rows))
	}
	if rows[0].open || rows[0].closeReason != protocol.CloseReasonThresholdChanged {
		t.Errorf("first episode open=%v reason=%q, want closed %q", rows[0].open, rows[0].closeReason,
			protocol.CloseReasonThresholdChanged)
	}
	if !rows[1].open {
		t.Error("the place is still below the new threshold and must reopen")
	}
	if n := r.failedMints(); n != 0 {
		t.Errorf("%d failed mint(s), want 0", n)
	}
}

// ── (3) restart mid-episode ─────────────────────────────────────────────────

// A RESTART KEEPS THE DEMAND. A fresh monitor over the same database, the place
// still hungry: the boot pass mints nothing, fires on the existing origin, and
// the warm-up grants exactly one further fire that bypasses the debounce
// window. That one bypass per hungry place per restart is the cold-start
// allowance (warmUpFloor = 2, one spent by the boot pass's own fire); both
// fires carry the same origin, so the second subtracts what the first
// ordered.
func TestReadThrough_RestartMidEpisode(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-RS", 500)
	r.boot()
	r.setUOP(40)
	r.delta()
	original := r.openRows()
	if len(original) != 1 {
		t.Fatalf("setup: %d open episodes, want 1", len(original))
	}

	restarted := NewThresholdMonitor(r.eng)
	restarted.now = r.clock.Now
	restarted.fireHook = r.m.fireHook
	before := len(r.fired())
	restarted.startupSweep(context.Background())
	restarted.OnBinUOPDelta(r.b.payloadCode, -1) // inside the window: the warm-up bypass
	restarted.OnBinUOPDelta(r.b.payloadCode, -1) // inside the window: debounced

	if n := len(r.rows()); n != 1 {
		t.Errorf("%d episodes after a restart, want 1 — no second mint", n)
	}
	if n := r.failedMints(); n != 0 {
		t.Errorf("%d failed mint(s) across the restart, want 0", n)
	}
	after := r.fired()[before:]
	if len(after) != warmUpFloor {
		t.Fatalf("restart fired %d time(s) inside one debounce window, want %d", len(after), warmUpFloor)
	}
	for i, f := range after {
		if f.OriginID != original[0].originID {
			t.Errorf("post-restart fire %d carried %q, want the pre-restart origin %s", i, f.OriginID, original[0].originID)
		}
	}
}

// ── helpers for the verify-red pins ─────────────────────────────────────────

// insertRegistryRow plants a demand_registry row by direct SQL — no production
// door, so nothing is notified.
func insertRegistryRow(t *testing.T, r *rtRig, station string, threshold int) {
	t.Helper()
	_, err := r.eng.db.Exec(`INSERT INTO demand_registry
		(station_id, core_node_name, role, payload_code, replenish_uop_threshold)
		VALUES ($1, $2, 'consume', $3, $4)`, station, r.b.coreNodeName, r.b.payloadCode, threshold)
	testutil.MustNoErr(t, err, "insert registry row")
}

// ── (4)–(8) what a change to the monitor's memory must fix ──────────────────

// REGISTRY ROWS DELETED UNDER A RUNNING MONITOR. Nothing is notified: the rows
// go by direct SQL, which is the one writer no door can announce. With the
// place below threshold and deltas arriving, the monitor must order nothing
// against config that is gone, the sweep must close the open episode once,
// and when the rows come back the next delta must mint one fresh demand.
func TestReadThrough_RegistryDeletedUnderARunningMonitor(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-DEL", 500)
	r.boot()
	r.setUOP(40)
	r.delta()
	if len(r.openRows()) != 1 {
		t.Fatal("setup: no episode opened")
	}
	firesBefore := len(r.fired())

	_, err := r.eng.db.Exec(`DELETE FROM demand_registry WHERE payload_code = $1`, r.b.payloadCode)
	testutil.MustNoErr(t, err, "delete registry rows")
	for i := 0; i < 3; i++ {
		r.pastDebounce()
		r.delta()
	}
	if got := len(r.fired()) - firesBefore; got != 0 {
		t.Errorf("fired %d time(s) against a binding the registry no longer has, want 0", got)
	}

	r.eng.reconcileDemandEpisodes()
	for i := 0; i < 3; i++ {
		r.pastDebounce()
		r.delta()
	}
	rows := r.rows()
	if len(rows) != 1 {
		t.Fatalf("%d episodes after the rows went, want 1 — zero mints against a withdrawn binding", len(rows))
	}
	if rows[0].open || rows[0].closeReason != protocol.CloseReasonThresholdRemoved || rows[0].closedBy != protocol.ClosedBySweep {
		t.Errorf("episode open=%v closed %q by %q, want closed %q by %q", rows[0].open, rows[0].closeReason,
			rows[0].closedBy, protocol.CloseReasonThresholdRemoved, protocol.ClosedBySweep)
	}

	insertRegistryRow(t, r, r.b.stationID, rtThreshold)
	r.pastDebounce()
	r.delta()
	rows = r.rows()
	if len(rows) != 2 || !rows[1].open {
		t.Fatalf("after the rows came back: %d episodes, want 2 with the second open", len(rows))
	}
	fired := r.fired()
	if len(fired) != firesBefore+1 || fired[len(fired)-1].OriginID != rows[1].originID {
		t.Errorf("after the rows came back: %d fire(s) since the delete, last carrying %q, want 1 carrying %s",
			len(fired)-firesBefore, fired[len(fired)-1].OriginID, rows[1].originID)
	}
}

// AN EPISODE CLOSED BEHIND THE MONITOR'S BACK — the childless pass's shape,
// planted by direct SQL. The next fire must not stamp the closed origin, and a
// fresh episode must open for the still-hungry place.
func TestReadThrough_EpisodeClosedBehindTheMonitorsBack(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-CL", 500)
	r.boot()
	r.setUOP(40)
	r.delta()
	open := r.openRows()
	if len(open) != 1 {
		t.Fatal("setup: no episode opened")
	}
	_, err := r.eng.db.Exec(`UPDATE demand_origins SET closed_at = now(), close_reason = 'unattributed',
		closed_by = 'sweep' WHERE origin_id = $1`, open[0].originID)
	testutil.MustNoErr(t, err, "close the episode by hand")

	r.pastDebounce()
	r.delta()

	rows := r.rows()
	if len(rows) != 2 || !rows[1].open {
		t.Fatalf("%d episodes, want 2 with the second open — the place is still hungry", len(rows))
	}
	fired := r.fired()
	last := fired[len(fired)-1]
	if last.OriginID == open[0].originID {
		t.Errorf("the fire stamped origin %s, which was already closed", last.OriginID)
	}
	if last.OriginID != rows[1].originID {
		t.Errorf("the fire carried %q, want the fresh episode %s", last.OriginID, rows[1].originID)
	}
}

// ONE PLACE, TWO STATIONS. Two registry rows for the same (node, payload)
// under different station ids, both below: one place is one demand. One
// episode, one origin, one fire, and one line naming the collision.
func TestReadThrough_OnePlaceUnderTwoStations(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-DUP", 40)
	const stale = "PLANT.RT-STALE"
	insertRegistryRow(t, r, stale, rtThreshold)

	r.boot()

	if n := len(r.rows()); n != 1 {
		t.Errorf("%d episodes for one place, want 1", n)
	}
	if n := r.failedMints(); n != 0 {
		t.Errorf("%d failed mint(s), want 0 — the second station is not a second demand", n)
	}
	fired := r.fired()
	if len(fired) != 1 {
		t.Errorf("fired %d time(s) for one place, want 1", len(fired))
	} else if open := r.openRows(); len(open) == 1 && fired[0].OriginID != open[0].originID {
		t.Errorf("the fire carried %q, want %s", fired[0].OriginID, open[0].originID)
	}
	lines := r.sink.linesContaining("DUPLICATE BINDING")
	if len(lines) != 1 {
		t.Fatalf("%d DUPLICATE BINDING line(s), want 1", len(lines))
	}
	if !strings.Contains(lines[0], r.b.stationID) || !strings.Contains(lines[0], stale) {
		t.Errorf("the line must name both stations: %s", lines[0])
	}
}

// the feeders into the fire gate, each through its production door, with the
// monitor already having seen the binding at a healthy level where the door
// itself does not build that knowledge.
var rtFeeders = []struct {
	name  string
	prime bool
	drive func(r *rtRig)
}{
	{"delta", true, func(r *rtRig) { r.delta() }},
	{"lineside_report", true, func(r *rtRig) { r.m.OnLinesideReports([]string{r.b.payloadCode}) }},
	{"manual_swap", true, func(r *rtRig) { r.m.NoteSwapRequestContradiction(r.b.payloadCode) }},
	{"startup_sweep", false, func(r *rtRig) { r.boot() }},
	{"resync", false, func(r *rtRig) { r.m.Resync(r.b.stationID) }},
	{"threshold_change", false, func(r *rtRig) {
		r.m.OnThresholdChanges([]demands.RegistryChange{{
			StationID: r.b.stationID, CoreNodeName: r.b.coreNodeName, PayloadCode: r.b.payloadCode,
			OldThreshold: 0, NewThreshold: rtThreshold,
		}})
	}},
}

// A READ ERROR IS SIDE-EFFECT-FREE AT EVERY FEEDER. The authoritative total
// cannot be read (lineside_buckets hidden), the place is empty and nothing is
// open: nothing may open and nothing may be ordered. Ordering off a level
// nobody read is the fabricated-zero fire.
func TestReadThrough_ReadErrorOpensNothingOrdersNothing(t *testing.T) {
	t.Parallel()
	for _, f := range rtFeeders {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			r := newRTRig(t, "PANEL-RT-RE-"+f.name, 500)
			if f.prime {
				r.boot()
			}
			r.setUOP(40)
			withTableHidden(t, r.eng.db, "lineside_buckets", func() { f.drive(r) })
			if fired, rows := len(r.fired()), len(r.rows()); fired != 0 || rows != 0 {
				t.Errorf("a failed read fired %d time(s) and opened %d episode(s), want neither", fired, rows)
			}
		})
	}
}

// AND IT CLOSES NOTHING. An open episode, the place now stocked, the read
// failing: the rising edge needs a level, and there is none.
func TestReadThrough_ReadErrorClosesNothing(t *testing.T) {
	t.Parallel()
	for _, f := range rtFeeders {
		if f.name == "threshold_change" {
			// Its close is the config edit itself, not a reading of the level.
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			r := newRTRig(t, "PANEL-RT-RC-"+f.name, 500)
			r.boot()
			r.setUOP(40)
			r.delta()
			r.setUOP(150)
			withTableHidden(t, r.eng.db, "lineside_buckets", func() { f.drive(r) })
			if open := r.openRows(); len(open) != 1 {
				t.Errorf("%d open episode(s) after a failed read, want the 1 that was open", len(open))
			}
		})
	}
}

// A BLANK ORIGIN FIRES NOTHING. The mint is refused (a trigger rejects the
// INSERT), so there is no episode to stamp: an order with no origin is one
// dispatch.ReplenishLoader cannot subtract, which is how never-2N's sizing arm
// comes undone. Nothing fires, and the log says why.
func TestReadThrough_BlankOriginFiresNothing(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-BL", 500)
	r.boot()
	_, err := r.eng.db.Exec(`
		CREATE FUNCTION rt_refuse_mint() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'mint refused by test'; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER rt_refuse_mint BEFORE INSERT ON demand_origins
		FOR EACH ROW EXECUTE FUNCTION rt_refuse_mint();`)
	testutil.MustNoErr(t, err, "install the refusing trigger")

	r.setUOP(40)
	r.delta()

	if n := len(r.fired()); n != 0 {
		t.Errorf("fired %d time(s) with no episode to stamp, want 0", n)
	}
	if n := r.failedMints(); n != 1 {
		t.Errorf("%d failed-mint line(s), want 1", n)
	}
}

// TWO EVALUATIONS RACE ON ONE FALLING EDGE, twenty times. The partial unique
// index is the arbiter: each round leaves exactly one open episode, and every
// fire in the round carries it.
func TestReadThrough_ConcurrentFallingEdgeOpensOne(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-CC", 500)
	r.boot()
	for round := 0; round < 20; round++ {
		r.pastDebounce()
		r.setUOP(40)
		before := len(r.fired())
		var wg sync.WaitGroup
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() { defer wg.Done(); r.delta() }()
		}
		wg.Wait()
		open := r.openRows()
		if len(open) != 1 {
			t.Fatalf("round %d: %d open episodes, want 1", round, len(open))
		}
		for _, f := range r.fired()[before:] {
			if f.OriginID != open[0].originID {
				t.Errorf("round %d: a fire carried %q, want the open episode %s", round, f.OriginID, open[0].originID)
			}
		}
		r.setUOP(150)
		r.delta()
	}
}

// ── statements per evaluation ───────────────────────────────────────────────

// rtCountCase is one evaluation shape, measured as the statements one delta
// costs once the case is set up. The fire path's own reads (catalog, loader
// config, ReplenishLoader) are not in these numbers: fireHook intercepts
// before them, and no case below reaches a fire inside the measured delta.
type rtCountCase struct {
	name  string
	setup func(r *rtRig, lineNode string)
	// payload is what the measured delta is for; "" means the rig's payload.
	payload string
}

var rtCountCases = []rtCountCase{
	{name: "healthy_no_report", setup: func(r *rtRig, _ string) {}},
	{name: "healthy_fresh_report", setup: func(r *rtRig, node string) {
		_, err := r.eng.db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station: r.b.stationID, CoreNodeName: node, PayloadCode: r.b.payloadCode,
			BinCount: 1, BinUOP: 500, ReportedAt: time.Now().UTC(),
		})
		testutil.MustNoErr(r.t, err, "upsert report")
	}},
	{name: "below_open_debounced", setup: func(r *rtRig, _ string) {
		r.setUOP(40)
		r.delta() // opens and fires; the measured delta is then debounced
	}},
	{name: "unmonitored", payload: "PANEL-RT-NOBODY"},
}

// measureStatements returns, per lineside mode and case, the statements one
// delta costs.
func measureStatements(t *testing.T) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, mode := range []string{linesideModeEdgeReports, linesideModeLedger} {
		for _, c := range rtCountCases {
			_, cfg := testdb.OpenWithConfig(t)
			cdb, counter, err := store.OpenCounting(cfg)
			testutil.MustNoErr(t, err, "open counting db")
			t.Cleanup(func() { cdb.Close() })

			sink := &logSink{}
			eng := newLoggingEngine(t, cdb, sink)
			m := eng.thresholdMonitor
			m.linesideMode = mode
			clk := &rtClock{now: time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)}
			m.now = clk.Now
			fires := captureThresholdFires(t, eng)
			payload := "PANEL-RT-N-" + c.name
			b := stationBinding(t, eng, "PLANT.RT", "SLN_RT", payload, 18)
			b.threshold = rtThreshold
			registerBinding(t, cdb, b)
			sd := testdb.SetupStandardData(t, cdb)
			bin := testdb.CreateBinAtNode(t, cdb, payload, sd.LineNode.ID, "BIN-"+payload)
			r := &rtRig{t: t, eng: eng, m: m, sink: sink, fires: fires, b: b, binID: bin.ID, clock: clk}
			r.setUOP(500)
			r.boot()
			if c.setup != nil {
				c.setup(r, sd.LineNode.Name)
			}
			target := c.payload
			if target == "" {
				target = payload
			}
			counter.Reset()
			m.OnBinUOPDelta(target, -1)
			out[mode+"/"+c.name] = counter.Count()
		}
	}
	return out
}

// TestReadThrough_StatementsPerEvaluation pins what one delta costs in each
// shape and mode. Reading the tables instead of copies of them costs two
// single-row index probes per evaluation of a monitored payload — the bindings
// lookup and the open-episode probe — and one lookup that returns nothing for
// an unmonitored one. Measured before the copies were deleted: 3, 5, 3 and 0.
// The total read itself is 2 statements, the lineside report list 1, and the
// per-node ledger 2 more when a fresh report exists.
func TestReadThrough_StatementsPerEvaluation(t *testing.T) {
	want := map[string]int64{
		"healthy_no_report":    5,
		"healthy_fresh_report": 7,
		"below_open_debounced": 5,
		"unmonitored":          1,
	}
	got := measureStatements(t)
	for k, v := range got {
		t.Logf("statements %s = %d", k, v)
		c := k[strings.Index(k, "/")+1:]
		if v != want[c] {
			t.Errorf("statements %s = %d, want %d", k, v, want[c])
		}
	}
}

// THE PAGE READS THE REGISTRY. Snapshot is what Replenishment Health renders
// for "is this loader monitored, at what threshold?", so it shows every row the
// registry holds — a duplicate station included — stops showing a binding the
// moment its row is gone, and reports a failed read as a failure rather than
// as "nothing is monitored".
func TestReadThrough_SnapshotReadsTheRegistry(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-SNAP", 500)
	insertRegistryRow(t, r, "PLANT.RT-STALE", rtThreshold)

	snap, err := r.m.Snapshot()
	testutil.MustNoErr(t, err, "snapshot")
	if len(snap) != 1 || len(snap[0].Bindings) != 2 {
		t.Fatalf("snapshot = %+v, want the payload with both registry rows", snap)
	}

	_, err = r.eng.db.Exec(`DELETE FROM demand_registry WHERE payload_code = $1`, r.b.payloadCode)
	testutil.MustNoErr(t, err, "delete registry rows")
	snap, err = r.m.Snapshot()
	testutil.MustNoErr(t, err, "snapshot")
	if len(snap) != 0 {
		t.Errorf("snapshot still shows %+v after the rows went — the page would say a loader is monitored that is not", snap)
	}

	hideDemandRegistry(t, r.eng.db)
	if _, err := r.m.Snapshot(); err == nil {
		t.Error("a failed registry read returned no error — the page would render it as nothing monitored")
	}
}

// A NEGATIVE TOTAL'S LOG LINE IS THROTTLED; ITS ORDERING IS NOT. Four deltas
// sixteen seconds apart — each past the debounce window, all inside the
// negative-log window — fire four times and log once.
func TestReadThrough_NegativeLogThrottleDoesNotGateOrdering(t *testing.T) {
	t.Parallel()
	r := newRTRig(t, "PANEL-RT-NEG", 500)
	r.boot()
	r.setUOP(-443)
	for i := 0; i < 4; i++ {
		r.delta()
		r.pastDebounce()
	}
	if got := len(r.fired()); got != 4 {
		t.Errorf("fired %d time(s), want 4 — a negative count must still order material every window", got)
	}
	if got := r.sink.countContaining("NEGATIVE COUNT"); got != 1 {
		t.Errorf("%d NEGATIVE COUNT line(s), want 1 — the log is throttled per place", got)
	}
}
