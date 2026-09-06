package planttime_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingo/shared/planttime"
)

// On a plant there is no SimClock, so the page is told wall time at 1x and
// wears no badge. This is the production path and it must stay boring.
func TestServerClock_PlantIsWallAtOneTimesAndUnbadged(t *testing.T) {
	clock.SetDefault(clock.Real())
	sc := planttime.CurrentServerClock()
	if sc.Sim {
		t.Error("Sim = true with no SimClock installed")
	}
	if sc.Speed != 1 {
		t.Errorf("Speed = %v, want 1", sc.Speed)
	}
	if got := planttime.SimBadge(time.UTC); got != "" {
		t.Errorf("SimBadge = %q on a plant, want empty — a production page must not claim to be simulated", got)
	}
	if _, err := time.Parse(time.RFC3339Nano, sc.Now); err != nil {
		t.Errorf("Now %q is not RFC3339: %v", sc.Now, err)
	}
}

// On a sim stack the page is told SIMULATED now and the multiplier, and the
// badge says so. Simulated time that does not label itself is how a screenshot
// of the rig gets read as a screenshot of a plant.
func TestServerClock_SimReportsSimulatedNowAndLabelsItself(t *testing.T) {
	// A PAST origin, like a real anchor: sim-now = epoch + speed x (wallNow -
	// anchor), so the clock is already well ahead of the wall by the time this
	// reads it. (A future origin would make elapsed negative and put simulated
	// time BEHIND — not a shape a minted anchor can produce.)
	epoch := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	sim, _ := clock.BuildSimClock(epoch, epoch, 2, 0)
	clock.SetDefault(sim)
	t.Cleanup(func() { clock.SetDefault(clock.Real()) })

	sc := planttime.CurrentServerClock()
	if !sc.Sim {
		t.Error("Sim = false with a SimClock installed")
	}
	if sc.Speed != 2 {
		t.Errorf("Speed = %v, want 2", sc.Speed)
	}
	got, err := time.Parse(time.RFC3339Nano, sc.Now)
	if err != nil {
		t.Fatalf("Now %q: %v", sc.Now, err)
	}
	// Simulated, not wall: the whole point, and the drift is the defect stated
	// as a value. At 2x from a 2020 origin the clock stands roughly six years
	// ahead of the machine's real one.
	if d := got.Sub(time.Now()); d < 365*24*time.Hour {
		t.Errorf("Now = %s is only %s ahead of wall; a 2x clock from 2020 should be years ahead", got, d)
	}
	if got.Before(epoch) {
		t.Errorf("Now = %s is before the epoch %s", got, epoch)
	}

	badge := string(planttime.SimBadge(time.UTC))
	if !strings.Contains(badge, "SIM 2") {
		t.Errorf("badge = %q, want it to name SIM and the multiplier", badge)
	}
	if !strings.Contains(badge, "sim-badge") {
		t.Errorf("badge = %q, want the class the stylesheet targets", badge)
	}
}

// The inline the page head carries has to be valid JSON, because the template
// drops it straight into `window.SHINGO_CLOCK = {{ serverClock }}` — a broken
// literal there takes every script on the page with it.
func TestServerClockJS_IsParseableJSON(t *testing.T) {
	clock.SetDefault(clock.Real())
	var sc planttime.ServerClock
	if err := json.Unmarshal([]byte(planttime.ServerClockJS()), &sc); err != nil {
		t.Fatalf("ServerClockJS is not JSON: %v", err)
	}
	if sc.Now == "" || sc.Speed == 0 {
		t.Errorf("round-tripped %+v, want a populated clock", sc)
	}
}
