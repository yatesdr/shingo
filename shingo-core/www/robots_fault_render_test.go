package www

import (
	"strings"
	"testing"
	"time"

	"shingocore/fleet"
)

// The robots page is the surface an operator walks toward, and it has never
// named the order a robot is on. These pin the three shapes the tile renders.

func robotTileHTML(t *testing.T, line RobotOrderLine) string {
	t.Helper()
	robot := fleet.RobotStatus{VehicleID: "AMR-04", Connected: true, BatteryLevel: 72}
	namer := &fakeNamer{byUID: map[string]string{}}
	data := map[string]any{
		"Page":       "robots",
		"Robots":     []fleet.RobotStatus{robot},
		"OrderLines": map[string]RobotOrderLine{"AMR-04": line},
	}
	return renderPageWithNamer(t, "robots.html", namer, data)
}

func TestRobotTile_NamesTheOrderItIsOn(t *testing.T) {
	t.Parallel()
	html := robotTileHTML(t, RobotOrderLine{OrderID: 4412, OrderStatus: "in_transit"})
	if !strings.Contains(html, "on #4412") {
		t.Error("a robot on an order must name it")
	}
	if strings.Contains(html, "chip-warn") {
		t.Error("an ordinary order must not raise a warning chip")
	}
}

func TestRobotTile_ReplanningIsQuiet(t *testing.T) {
	t.Parallel()
	since := time.Now().UTC().Add(-14 * time.Second)
	html := robotTileHTML(t, RobotOrderLine{
		OrderID: 4412, OrderStatus: "faulted", FaultSince: &since, FaultNotice: false,
	})
	if !strings.Contains(html, "on #4412 · replanning") {
		t.Error("a fault under the threshold reads as replanning")
	}
	if strings.Contains(html, "chip-warn") {
		t.Error("a replan must not raise a warning chip — it is not something to walk toward")
	}
}

func TestRobotTile_NoticeFaultIsAChipWithALiveClock(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	html := robotTileHTML(t, RobotOrderLine{
		OrderID: 4412, OrderStatus: "faulted", FaultSince: &since, FaultNotice: true,
	})
	if !strings.Contains(html, "chip-warn") || !strings.Contains(html, "on #4412 · fault") {
		t.Error("a fault past the threshold must be visible on the tile")
	}
	if !strings.Contains(html, `data-since="2026-08-22T14:00:00Z"`) {
		t.Error("the tile's fault must carry a live clock")
	}
}

func TestRobotTile_IdleRobotSaysNothing(t *testing.T) {
	t.Parallel()
	robot := fleet.RobotStatus{VehicleID: "AMR-04", Connected: true, BatteryLevel: 72}
	namer := &fakeNamer{byUID: map[string]string{}}
	html := renderPageWithNamer(t, "robots.html", namer, map[string]any{
		"Page":       "robots",
		"Robots":     []fleet.RobotStatus{robot},
		"OrderLines": map[string]RobotOrderLine{},
	})
	if strings.Contains(html, "robot-order") {
		t.Error("an idle robot must not render an empty order line")
	}
}

// No alarms on the tile. The alarm-fault join is unproven — the one sample was
// a standing 54018 on 95 of 95 faults — and an unproven alarm printed beside a
// fault reads as its cause.
func TestRobotOrderLine_CarriesNoAlarms(t *testing.T) {
	t.Parallel()
	since := time.Now().UTC()
	line := RobotOrderLine{OrderID: 1, OrderStatus: "faulted", FaultSince: &since, FaultNotice: true}
	html := robotTileHTML(t, line)
	for _, word := range []string{"alarm", "54018"} {
		if strings.Contains(strings.ToLower(html), word) {
			t.Errorf("the robot tile must not carry %q in this wave", word)
		}
	}
}
