package www

import (
	"testing"
	"time"

	"shingocore/fleet"
)

// The fault alarm is the alarm that began with the fault, never background:
// faultAlarm and the onsets that feed it. (HOP, 2026-11-12: the robot map's
// activity line names why a robot faulted.)
func TestFaultAlarmIsTheOneThatBeganWithTheFault(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
	alarms := []fleet.RobotAlarm{
		{Code: 54018, Severity: "warning", Desc: "Reflector Area Name: 01,"},
		{Code: 54231, Severity: "warning", Desc: "Caution: robot is blocked"},
		{Code: 52200, Severity: "error", Desc: "robot is blocked"},
		{Code: 54019, Severity: "warning", Desc: "less than 3 reflectors detected"},
	}
	seen := map[int]time.Time{
		54018: since.Add(-time.Second),      // standing: never offered
		54231: since.Add(-30 * time.Second), // began just before the fault: its cause
		52200: since.Add(5 * time.Second),   // began with the fault, and an error
		54019: since.Add(-10 * time.Minute), // long before the fault: background
	}
	got := faultAlarm(alarms, seen, since)
	if got == nil || got.Code != 52200 {
		t.Fatalf("faultAlarm = %+v, want 52200 (the most severe that began with the fault)", got)
	}
	delete(seen, 52200)
	alarms = alarms[:2]
	if got := faultAlarm(alarms, seen, since); got == nil || got.Code != 54231 {
		t.Fatalf("faultAlarm = %+v, want 54231 (began %s before the fault, inside the lead)", got, faultAlarmLead)
	}
	seen[54231] = time.Time{}
	if got := faultAlarm(alarms, seen, since); got != nil {
		t.Fatalf("faultAlarm = %+v, want nil: an alarm whose onset is unknown explains nothing", got)
	}
	if got := faultAlarm([]fleet.RobotAlarm{{Code: 54018, Severity: "warning"}}, map[int]time.Time{54018: since}, since); got != nil {
		t.Fatalf("faultAlarm = %+v, want nil: 54018 is standing", got)
	}
}

func TestAlarmOnsetsSeedFirstSightAndForgetWhatClears(t *testing.T) {
	t.Parallel()
	var a alarmOnsets
	t0 := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
	robot := func(codes ...int) []fleet.RobotStatus {
		r := fleet.RobotStatus{VehicleID: "AMR-12", Connected: true}
		for _, c := range codes {
			r.Alarms = append(r.Alarms, fleet.RobotAlarm{Code: c})
		}
		return []fleet.RobotStatus{r}
	}
	a.observe(robot(54018), t0)
	if at := a.firstSeen("AMR-12")[54018]; !at.IsZero() {
		t.Fatalf("an alarm present at first sight has onset %s, want unknown (zero)", at)
	}
	a.observe(robot(54018, 54231), t0.Add(2*time.Second))
	if at := a.firstSeen("AMR-12")[54231]; !at.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("a new alarm's onset = %s, want the poll that first saw it", at)
	}
	a.observe(robot(54018), t0.Add(4*time.Second))
	a.observe(robot(54018, 54231), t0.Add(6*time.Second))
	if at := a.firstSeen("AMR-12")[54231]; !at.Equal(t0.Add(6 * time.Second)) {
		t.Fatalf("an alarm that cleared and came back has onset %s, want its return", at)
	}
}
