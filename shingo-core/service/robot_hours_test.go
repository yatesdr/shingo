package service

import (
	"testing"

	"shingocore/store/robothours"
)

// TestRobotHoursRows_TodayAndPriorFold pins the Fleet tab's fold: today's
// plant day apart, the other days summed, minutes and kilometres made at the
// edge, the lowest battery kept, lifts summed only where known, the three
// named alarm codes counted and every other code ignored, and a robot with no
// row in a span left nil (a dash on the page), never zero.
func TestRobotHoursRows_TodayAndPriorFold(t *testing.T) {
	t.Parallel()
	b := func(f float64) *float64 { return &f }
	n := func(i int) *int { return &i }
	rows := []robothours.DayRow{
		{Day: "2026-10-13", VehicleID: "AMR-02", Hours: 24, ConnectedS: 86400, ChargingS: 3600, Metres: 4000,
			ChargeStarts: 3, BatteryMin: b(40), Lifts: n(30), Onsets: map[int]int{54901: 2, 52101: 1, 50401: 1, 54211: 9}},
		{Day: "2026-10-14", VehicleID: "AMR-02", Hours: 24, ConnectedS: 86400, BlockedS: 125, Metres: 1500,
			BatteryMin: b(35), Onsets: map[int]int{54901: 1}},
		{Day: "2026-10-15", VehicleID: "AMR-01", Hours: 10, ConnectedS: 36000, Metres: 500, BatteryMin: b(60),
			Lifts: n(4), Onsets: map[int]int{}},
	}
	got := robotHoursRows(rows, "2026-10-15")
	if len(got) != 2 || got[0].VehicleID != "AMR-01" || got[1].VehicleID != "AMR-02" {
		t.Fatalf("rows = %+v, want AMR-01 then AMR-02", got)
	}
	a1 := got[0]
	if a1.Prior != nil || a1.Today == nil || a1.Today.ConnectedMin != 600 || a1.Today.Km != 0.5 || *a1.Today.Lifts != 4 {
		t.Errorf("AMR-01 = today %+v prior %+v", a1.Today, a1.Prior)
	}
	a2 := got[1]
	if a2.Today != nil {
		t.Errorf("AMR-02 today = %+v, want nil (no row today)", a2.Today)
	}
	p := a2.Prior
	if p.Hours != 48 || p.ConnectedMin != 2880 || p.ChargingMin != 60 || p.BlockedMin != 2 || p.Km != 5.5 ||
		p.ChargeStarts != 3 || *p.BatteryMinPct != 35 || *p.Lifts != 30 || p.Film != 3 || p.LaserInvalid != 1 || p.BatteryFatal != 1 {
		t.Errorf("AMR-02 prior = %+v", p)
	}
}
