//go:build docker

package engine

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/cleaning"
	"shingocore/store/scene"
)

// seamFleet is the simulator with the one call the cleaning loop needs and the
// simulator lacks: a vendor order's state. The state is set by the test, so
// the completion path is asserted without a vendor. CancelOrder is recorded.
type seamFleet struct {
	*simulator.SimulatorBackend
	mu        sync.Mutex
	states    map[string]string
	cancelled []string
}

func newSeamFleet() *seamFleet {
	return &seamFleet{SimulatorBackend: simulator.New(), states: map[string]string{}}
}

func (f *seamFleet) GetVendorOrderDetail(id string) (*fleet.VendorOrderDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[id]
	if !ok {
		return nil, errors.New("no such order")
	}
	return &fleet.VendorOrderDetail{State: st}, nil
}

func (f *seamFleet) set(id, state string) {
	f.mu.Lock()
	f.states[id] = state
	f.mu.Unlock()
}

func (f *seamFleet) CancelOrder(id string) error {
	f.mu.Lock()
	f.cancelled = append(f.cancelled, id)
	f.mu.Unlock()
	return f.SimulatorBackend.CancelOrder(id)
}

// seedKnife puts LM513 and LM514 in the scene as LocationMarks — the knife's
// points at Hopkinsville, which Core never makes nodes of.
func seedKnife(t *testing.T, db *store.DB) {
	t.Helper()
	for _, p := range []string{"LM513", "LM514"} {
		testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{AreaName: "default", InstanceName: p,
			ClassName: "LocationMark", PropertiesJSON: "{}"}), "seed "+p)
	}
}

func knifeSetup() CleaningSetupInput {
	return CleaningSetupInput{Points: []string{"LM513", "LM514"}, Frequency: "4 h",
		ActiveFrom: "00:00", ActiveTo: "00:00", MinBattery: 25}
}

func countOrders(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM orders`).Scan(&n), "count orders")
	return n
}

// TestCleaning_TickSendsOnePinnedOrderAndNoOrdersRow: the tick sends an
// sg-clean- order pinned to the robot through the points in order, records it,
// writes no Core orders row, sends nobody else while it is open, and closes it
// when the vendor says FINISHED.
func TestCleaning_TickSendsOnePinnedOrderAndNoOrdersRow(t *testing.T) {
	db := testDB(t)
	seedKnife(t, db)
	flt := newSeamFleet()
	eng := newTestEngine(t, db, flt)
	eng.fleetConnected.Store(true)
	testutil.MustNoErr(t, eng.SaveCleaningSetup(knifeSetup(), "test"), "save setup (LocationMarks accepted)")
	ordersBefore := countOrders(t, db)

	c := &cleaningRunner{e: eng, seam: flt}
	now := time.Now()
	robots := []fleet.RobotStatus{idleRobot("AMR-01"), idleRobot("AMR-02")}
	c.tick(now, robots)

	open, err := cleaning.OpenMissions(db.DB)
	testutil.MustNoErr(t, err, "open missions")
	if len(open) != 1 || open[0].VehicleID != "AMR-01" || open[0].Reason != cleaning.ReasonInterval {
		t.Fatalf("open = %+v, want one interval mission for AMR-01", open)
	}
	vid := open[0].VendorOrderID
	if !strings.HasPrefix(vid, "sg-clean-") {
		t.Errorf("vendor id %q lacks sg-clean-", vid)
	}
	ov := flt.GetOrder(vid)
	if ov == nil || ov.Vehicle != "AMR-01" || !ov.Complete || len(ov.Blocks) != 2 ||
		ov.Blocks[0].Location != "LM513" || ov.Blocks[1].Location != "LM514" || ov.Blocks[0].BinTask != "" {
		t.Fatalf("sim order = %+v, want pinned, complete, LM513 then LM514, no bin task", ov)
	}
	if n := countOrders(t, db); n != ordersBefore {
		t.Errorf("orders rows %d → %d: a cleaning order must never write one", ordersBefore, n)
	}

	// One at the knife: AMR-02 is due and idle, but nothing goes while open.
	c.tick(now.Add(time.Minute), robots)
	open, err = cleaning.OpenMissions(db.DB)
	testutil.MustNoErr(t, err, "cleaning.OpenMissions")
	if len(open) != 1 || flt.OrderCount() != 1 {
		t.Fatalf("second tick with one open: %d open, %d sim orders; want 1 and 1", len(open), flt.OrderCount())
	}

	// FINISHED closes it, and the same tick sends the next robot.
	flt.set(vid, "FINISHED")
	c.tick(now.Add(2*time.Minute), robots)
	list, err := cleaning.ListMissions(db.DB, now.Add(-time.Hour), 10)
	testutil.MustNoErr(t, err, "cleaning.ListMissions")
	states := map[string]string{}
	for _, m := range list {
		states[m.VehicleID] = m.State
	}
	if states["AMR-01"] != cleaning.StateFinished || states["AMR-02"] != cleaning.StateDispatched {
		t.Errorf("after FINISHED: %v, want AMR-01 finished and AMR-02 sent", states)
	}
}

// TestCleaning_MaxAgeCancelsAtTheVendorThenFails and STOPPED → cancelled.
func TestCleaning_MaxAgeCancelsAtTheVendorThenFails(t *testing.T) {
	db := testDB(t)
	seedKnife(t, db)
	flt := newSeamFleet()
	eng := newTestEngine(t, db, flt)
	testutil.MustNoErr(t, eng.SaveCleaningSetup(knifeSetup(), "test"), "save setup")
	now := time.Now()

	oldID, err := cleaning.RecordDispatch(db.DB, "AMR-01", "sg-clean-old", cleaning.ReasonInterval, now.Add(-31*time.Minute))
	testutil.MustNoErr(t, err, "cleaning.RecordDispatch")
	flt.set("sg-clean-old", "RUNNING")
	stopID, err := cleaning.RecordDispatch(db.DB, "AMR-02", "sg-clean-stop", cleaning.ReasonManual, now.Add(-2*time.Minute))
	testutil.MustNoErr(t, err, "cleaning.RecordDispatch")
	flt.set("sg-clean-stop", "STOPPED")

	(&cleaningRunner{e: eng, seam: flt}).tick(now, nil)

	flt.mu.Lock()
	cancelled := append([]string(nil), flt.cancelled...)
	flt.mu.Unlock()
	if len(cancelled) != 1 || cancelled[0] != "sg-clean-old" {
		t.Errorf("cancelled = %v, want only the over-age order", cancelled)
	}
	list, err := cleaning.ListMissions(db.DB, now.Add(-time.Hour), 10)
	testutil.MustNoErr(t, err, "cleaning.ListMissions")
	for _, m := range list {
		switch m.ID {
		case oldID:
			if m.State != cleaning.StateFailed {
				t.Errorf("over-age mission = %s, want failed", m.State)
			}
		case stopID:
			if m.State != cleaning.StateCancelled {
				t.Errorf("STOPPED mission = %s, want cancelled", m.State)
			}
		}
	}
}

// TestCleaning_NoSetupOrEmptyPointsDoesNothing: no row, and a saved setup with
// no points, both leave the fleet untouched.
func TestCleaning_NoSetupOrEmptyPointsDoesNothing(t *testing.T) {
	db := testDB(t)
	flt := newSeamFleet()
	eng := newTestEngine(t, db, flt)
	eng.fleetConnected.Store(true)
	c := &cleaningRunner{e: eng, seam: flt}
	robots := []fleet.RobotStatus{idleRobot("AMR-01")}

	c.tick(time.Now(), robots)
	empty := knifeSetup()
	empty.Points = nil
	testutil.MustNoErr(t, eng.SaveCleaningSetup(empty, "test"), "save empty")
	c.tick(time.Now(), robots)
	if flt.OrderCount() != 0 {
		t.Errorf("%d orders sent with no points", flt.OrderCount())
	}
	open, err := cleaning.AnyOpen(db.DB)
	testutil.MustNoErr(t, err, "cleaning.AnyOpen")
	if open {
		t.Error("a mission was recorded with no points")
	}
}

// TestCleaning_SimulatorIsNeverSent: the bare simulator cannot report a
// mission's state, so the loop sends nothing, says so once, and Clean now is
// refused with the same reason.
func TestCleaning_SimulatorIsNeverSent(t *testing.T) {
	db := testDB(t)
	seedKnife(t, db)
	sim := simulator.New()
	// Unstarted: the test replaces logFn, which no goroutine may be reading.
	// No dispatcher is needed — the knife's points are not Core nodes.
	eng := newUnstartedEngine(t, db, sim)
	eng.fleetConnected.Store(true)
	testutil.MustNoErr(t, eng.SaveCleaningSetup(knifeSetup(), "test"), "save setup")

	var mu sync.Mutex
	said := 0
	eng.logFn = func(format string, args ...any) {
		if strings.Contains(format, "cannot report a mission's state") {
			mu.Lock()
			said++
			mu.Unlock()
		}
	}
	c := &cleaningRunner{e: eng, seam: eng.cleaningSeam()}
	if c.seam != nil {
		t.Fatal("the simulator grew a vendor order reader; this test no longer pins the no-seam path")
	}
	robots := []fleet.RobotStatus{idleRobot("AMR-01")}
	c.tick(time.Now(), robots)
	c.tick(time.Now(), robots)
	mu.Lock()
	n := said
	mu.Unlock()
	if sim.OrderCount() != 0 || n != 1 {
		t.Errorf("sim orders %d, said %d times; want 0 and once", sim.OrderCount(), n)
	}

	eng.robotsMu.Lock()
	eng.latestPoll = robots
	eng.robotsMu.Unlock()
	if err := eng.CleanNow("AMR-01", "test"); err == nil || err.Error() != "the fleet cannot report a mission's state" {
		t.Errorf("CleanNow on the sim = %v", err)
	}
}

// TestCleanNow_SendsManualAndAudits: the human path sends with reason manual
// regardless of frequency, audits it, and refuses a robot not in the poll.
func TestCleanNow_SendsManualAndAudits(t *testing.T) {
	db := testDB(t)
	seedKnife(t, db)
	flt := newSeamFleet()
	eng := newTestEngine(t, db, flt)
	testutil.MustNoErr(t, eng.SaveCleaningSetup(knifeSetup(), "test"), "save setup")
	eng.robotsMu.Lock()
	eng.latestPoll = []fleet.RobotStatus{idleRobot("AMR-01")}
	eng.robotsMu.Unlock()

	if err := eng.CleanNow("AMR-09", "alice"); err == nil || err.Error() != "not in the fleet's latest report" {
		t.Errorf("CleanNow unknown robot = %v", err)
	}
	testutil.MustNoErr(t, eng.CleanNow("AMR-01", "alice"), "CleanNow")
	open, err := cleaning.OpenMissions(db.DB)
	testutil.MustNoErr(t, err, "cleaning.OpenMissions")
	if len(open) != 1 || open[0].Reason != cleaning.ReasonManual {
		t.Fatalf("open = %+v, want one manual mission", open)
	}
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM audit_log WHERE entity_type = 'cleaning'
		AND action = 'clean_now' AND new_value LIKE '%AMR-01%' AND actor = 'alice'`).Scan(&n), "audit")
	if n != 1 {
		t.Errorf("clean_now audit rows = %d, want 1", n)
	}
	if err := eng.CleanNow("AMR-01", "alice"); err == nil || err.Error() != "another robot is at the knife" {
		t.Errorf("second CleanNow = %v", err)
	}
}

// TestSaveCleaningSetup_Refusals: 3 h is refused, a name not in the scene is
// refused on its row, a scene point that is a Core lane slot is refused, and
// nothing is saved when anything is refused.
func TestSaveCleaningSetup_Refusals(t *testing.T) {
	db := testDB(t)
	seedKnife(t, db)
	eng := newTestEngine(t, db, simulator.New())
	_, slot := laneWithSlot(t, db, "CLN")
	testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{AreaName: "default", InstanceName: slot.Name,
		ClassName: "GeneralLocation", PropertiesJSON: "{}"}), "scene point for the slot")

	s := knifeSetup()
	s.Frequency = "3h"
	s.Points = []string{"LM513", " ", "LM999", slot.Name}
	err := eng.SaveCleaningSetup(s, "test")
	var ref *CleaningSetupRefusal
	if !errors.As(err, &ref) {
		t.Fatalf("SaveCleaningSetup = %v, want a field refusal", err)
	}
	// The blank is dropped before numbering: LM999 is row 1, the slot row 2.
	if ref.Fields["frequency"] == "" || ref.Fields["points.1"] != "LM999 is not in the scene" ||
		!strings.Contains(ref.Fields["points.2"], "is in lane") || ref.Fields["points.0"] != "" {
		t.Errorf("fields = %v", ref.Fields)
	}
	_, ok, err := cleaning.GetSetup(db.DB)
	testutil.MustNoErr(t, err, "cleaning.GetSetup")
	if ok {
		t.Error("a refused setup was saved")
	}
}
