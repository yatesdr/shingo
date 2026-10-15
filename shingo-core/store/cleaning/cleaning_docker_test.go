//go:build docker

package cleaning_test

import (
	"reflect"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/cleaning"
)

// TestSetup_RoundTrip pins the one-row setup: absent until saved, every field
// back as written (points in order, interval to duration, times as HH:MM), and
// a second save replacing the first.
func TestSetup_RoundTrip(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	if _, ok, err := cleaning.GetSetup(db.DB); err != nil || ok {
		t.Fatalf("fresh GetSetup: ok=%v err=%v, want no row", ok, err)
	}
	in := cleaning.Setup{
		Points: []string{"LM513", "LM514"}, Frequency: 4 * time.Hour,
		ActiveFrom: "05:00", ActiveTo: "23:30", MinBattery: 25,
	}
	if err := cleaning.SaveSetup(db.DB, in); err != nil {
		t.Fatalf("SaveSetup: %v", err)
	}
	got, ok, err := cleaning.GetSetup(db.DB)
	if err != nil || !ok {
		t.Fatalf("GetSetup: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(got.Points, in.Points) || got.Frequency != in.Frequency ||
		got.ActiveFrom != in.ActiveFrom || got.ActiveTo != in.ActiveTo || got.MinBattery != in.MinBattery {
		t.Errorf("GetSetup = %+v, want %+v", got, in)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at not stamped")
	}

	// Empty points is a saved setup with the loop off, not an absent row.
	if err := cleaning.SaveSetup(db.DB, cleaning.Setup{Frequency: time.Hour, ActiveFrom: "00:00", ActiveTo: "00:00", MinBattery: 30}); err != nil {
		t.Fatalf("SaveSetup empty: %v", err)
	}
	got, ok, err = cleaning.GetSetup(db.DB)
	if err != nil || !ok || len(got.Points) != 0 || got.Points == nil || got.MinBattery != 30 {
		t.Errorf("after empty save: ok=%v err=%v %+v, want saved with no points", ok, err, got)
	}
}

// TestMissions_Lifecycle pins dispatch → open → finalize in each final state,
// that a finalized row cannot be finalized again, and that "any open" is the
// fleet-wide fact the loop's one-at-a-time rule reads.
func TestMissions_Lifecycle(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	if open, err := cleaning.AnyOpen(db.DB); err != nil || open {
		t.Fatalf("fresh AnyOpen = %v, %v", open, err)
	}
	ids := map[string]int64{}
	for i, v := range []string{"AMR-01", "AMR-02", "AMR-03"} {
		id, err := cleaning.RecordDispatch(db.DB, v, "sg-clean-"+v, cleaning.ReasonInterval, t0.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("RecordDispatch %s: %v", v, err)
		}
		ids[v] = id
	}
	open, err := cleaning.OpenMissions(db.DB)
	if err != nil || len(open) != 3 || open[0].VehicleID != "AMR-01" || open[0].State != cleaning.StateDispatched {
		t.Fatalf("OpenMissions = %+v, %v; want 3 oldest first", open, err)
	}

	for v, st := range map[string]string{"AMR-01": cleaning.StateFinished, "AMR-02": cleaning.StateCancelled, "AMR-03": cleaning.StateFailed} {
		changed, err := cleaning.Finalize(db.DB, ids[v], st, t0.Add(time.Hour))
		if err != nil || !changed {
			t.Fatalf("Finalize %s %s: changed=%v err=%v", v, st, changed, err)
		}
	}
	if changed, err := cleaning.Finalize(db.DB, ids["AMR-01"], cleaning.StateFailed, t0.Add(2*time.Hour)); err != nil || changed {
		t.Errorf("second Finalize changed=%v err=%v, want unchanged", changed, err)
	}
	if _, err := cleaning.Finalize(db.DB, ids["AMR-01"], cleaning.StateDispatched, t0); err == nil {
		t.Error("Finalize accepted dispatched as a final state")
	}
	if open, err := cleaning.AnyOpen(db.DB); err != nil || open {
		t.Errorf("AnyOpen after finalizing all = %v, %v", open, err)
	}

	list, err := cleaning.ListMissions(db.DB, t0.Add(-time.Hour), 10)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListMissions = %d, %v", len(list), err)
	}
	got := map[string]string{}
	for _, m := range list {
		got[m.VehicleID] = m.State
		if m.FinishedAt == nil {
			t.Errorf("%s finished_at not stamped", m.VehicleID)
		}
	}
	want := map[string]string{"AMR-01": "finished", "AMR-02": "cancelled", "AMR-03": "failed"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("states = %v, want %v", got, want)
	}
	if list[0].VehicleID != "AMR-03" {
		t.Errorf("ListMissions not newest first: %s leads", list[0].VehicleID)
	}
}

// TestViews_LastDispatchAndOptOut pins the one-query view: last dispatch of ANY
// state (a failed mission still counts — due is measured from dispatch), the
// opt-out with or without a mission, and SetOptOut's changed flag.
func TestViews_LastDispatchAndOptOut(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	id, err := cleaning.RecordDispatch(db.DB, "AMR-01", "sg-clean-a", cleaning.ReasonInterval, t0)
	testutil.MustNoErr(t, err, "cleaning.RecordDispatch")
	cleaning.RecordDispatch(db.DB, "AMR-01", "sg-clean-b", cleaning.ReasonManual, t0.Add(2*time.Hour))
	cleaning.Finalize(db.DB, id, cleaning.StateFailed, t0.Add(time.Hour))
	cleaning.RecordDispatch(db.DB, "AMR-02", "sg-clean-c", cleaning.ReasonInterval, t0.Add(time.Hour))

	if changed, err := cleaning.SetOptOut(db.DB, "AMR-02", true); err != nil || !changed {
		t.Fatalf("SetOptOut AMR-02: %v %v", changed, err)
	}
	if changed, err := cleaning.SetOptOut(db.DB, "AMR-02", true); err != nil || changed {
		t.Errorf("repeat SetOptOut changed=%v err=%v", changed, err)
	}
	if _, err := cleaning.SetOptOut(db.DB, "AMR-09", true); err != nil {
		t.Fatal(err)
	}

	views, err := cleaning.Views(db.DB)
	if err != nil {
		t.Fatalf("Views: %v", err)
	}
	if v := views["AMR-01"]; v.LastDispatched == nil || !v.LastDispatched.Equal(t0.Add(2*time.Hour)) || v.OptedOut {
		t.Errorf("AMR-01 = %+v, want last t0+2h, not opted out", v)
	}
	if v := views["AMR-02"]; v.LastDispatched == nil || !v.OptedOut {
		t.Errorf("AMR-02 = %+v, want dispatched and opted out", v)
	}
	if v := views["AMR-09"]; v.LastDispatched != nil || !v.OptedOut {
		t.Errorf("AMR-09 = %+v, want never sent, opted out", v)
	}
	if _, ok := views["AMR-05"]; ok {
		t.Error("a robot in neither table appeared in the view")
	}

	if changed, err := cleaning.SetOptOut(db.DB, "AMR-02", false); err != nil || !changed {
		t.Errorf("clear opt-out: %v %v", changed, err)
	}
	views, err = cleaning.Views(db.DB)
	testutil.MustNoErr(t, err, "cleaning.Views")
	if views["AMR-02"].OptedOut {
		t.Error("AMR-02 still opted out after clear")
	}
}

// TestPruneMissions_NinetyDaysAtMostDaily pins retention and its once-a-day
// condition: nothing goes while the oldest row is inside 91 days; once it is
// past, every row past 90 goes and the next call deletes nothing.
func TestPruneMissions_NinetyDaysAtMostDaily(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	cleaning.RecordDispatch(db.DB, "AMR-01", "old", cleaning.ReasonInterval, now.Add(-90*day-12*time.Hour))
	cleaning.RecordDispatch(db.DB, "AMR-01", "keep", cleaning.ReasonInterval, now.Add(-89*day))
	if n, err := cleaning.PruneMissions(db.DB, now); err != nil || n != 0 {
		t.Fatalf("prune with oldest at 90.5 d = %d, %v; want 0 (inside the daily condition)", n, err)
	}

	cleaning.RecordDispatch(db.DB, "AMR-02", "older", cleaning.ReasonInterval, now.Add(-92*day))
	n, err := cleaning.PruneMissions(db.DB, now)
	if err != nil || n != 2 {
		t.Fatalf("prune with oldest at 92 d = %d, %v; want 2 (both past 90)", n, err)
	}
	n, err = cleaning.PruneMissions(db.DB, now)
	testutil.MustNoErr(t, err, "cleaning.PruneMissions")
	if n != 0 {
		t.Errorf("second prune deleted %d", n)
	}
	list, err := cleaning.ListMissions(db.DB, now.Add(-365*day), 10)
	testutil.MustNoErr(t, err, "cleaning.ListMissions")
	if len(list) != 1 || list[0].VendorOrderID != "keep" {
		t.Errorf("left %+v, want only keep", list)
	}
}
