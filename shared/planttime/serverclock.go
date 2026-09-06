package planttime

import (
	"encoding/json"
	"html/template"
	"strconv"
	"time"

	"shingo/protocol/clock"
)

// ServerClock is what the page needs to compute "now" the way the server does.
//
// ── THE SERVER OWNS NOW, FOR THE SAME REASON IT OWNS THE ZONE ─────────────
//
// This package exists because a Core board and an Edge station rendering one
// event on two clocks is a defect that reaches the floor. The zone was only
// half of it. Every elapsed reading in the UI — the live durations, the "as
// of" stamps, the header clock — was computed as `Date.now() - serverStamp`:
// a BROWSER wall clock differenced against a SERVER timestamp. On a plant that
// is merely fragile (an operator's laptop with a wrong clock shows wrong ages
// and nothing says so). On a sim rig it is fatal, because simulated now is not
// wall now at all.
//
// The rig proved it. The sim clock advances at `speed x` real time from a fixed
// origin, so simulated time ran eight weeks ahead of the wall; `Date.now() -
// since` was therefore negative for every row, formatDuration clamps negatives
// to zero, and EVERY live duration on the rig rendered "0 s". The one
// instrument that separates backpressure from a wedge, reading zero for months,
// silently, because each half was individually correct.
//
// So the page is told the server's now, and how fast it advances, and computes
// from that. On a plant Speed is 1 and this collapses to "the server's clock,
// not the viewer's" — still the right answer, and it fixes the wrong-laptop
// case for free.
type ServerClock struct {
	// Now is the server's current time, RFC3339 UTC. Simulated time on a sim
	// stack, wall time on a plant — the page does not need to know which to be
	// correct, only to label it (see Sim).
	Now string `json:"now"`
	// Speed is the rate Now advances against the viewer's wall clock. The page
	// extrapolates as now + speed x (browserNow - browserNowAtPaint), so a tab
	// left open for a week still reads true instead of falling `speed x
	// elapsed` behind.
	Speed float64 `json:"speed"`
	// Sim is whether this is simulated time, so the page can SAY SO. Simulated
	// time that does not label itself is how a screenshot of the rig gets read
	// as a screenshot of a plant.
	Sim bool `json:"sim"`
}

// CurrentServerClock reads the process clock. clock.Now() is the sim-aware
// default: a *SimClock on a sim stack (installed by BuildSimClock at startup)
// and the real clock everywhere else, which is what makes one template
// function correct on both.
func CurrentServerClock() ServerClock {
	sc := ServerClock{Now: clock.Now().UTC().Format(time.RFC3339Nano), Speed: 1}
	if s := clock.AsSimClock(); s != nil {
		sc.Sim = true
		if sp := s.Speed(); sp > 0 {
			sc.Speed = sp
		}
	}
	return sc
}

// ServerClockJS renders the inline the page head carries, feeding
// shared/utils.js's clock. Inline and synchronous for the same reason
// window.PLANT_TZ is: a fetched value paints wrong text first, and the flicker
// is what the plant-local convention removed.
//
// Marshalled rather than concatenated — the values are a timestamp, a float and
// a bool, none of which can carry a quote, but a hand-built object literal in a
// template is the shape that eventually does.
func ServerClockJS() template.JS {
	b, err := json.Marshal(CurrentServerClock())
	if err != nil {
		// Unreachable for three scalars; degrade to the browser clock rather
		// than emitting a syntax error that takes every page script with it.
		return template.JS("null")
	}
	return template.JS(b)
}

// SimBadge renders the marker a sim stack wears next to its clock, and nothing
// at all on a plant.
//
// SIMULATED TIME LABELS ITSELF. The rig's pages are pixel-identical to a
// plant's, and its clock has been reading a date months from now — a screenshot
// of the rig is indistinguishable from a screenshot of Springfield unless the
// page says which it is. It carries the multiplier too, because "SIM" alone
// does not tell you whether an age on this screen is worth two of yours.
//
// Server-rendered, like every other timestamp here: correct on first paint,
// with no JS required and nothing to flicker. The badge is absent rather than
// hidden on a plant, so there is no CSS rule standing between a production page
// and a false claim about itself.
func SimBadge(loc *time.Location) template.HTML {
	sc := CurrentServerClock()
	if !sc.Sim {
		return ""
	}
	now, err := time.Parse(time.RFC3339Nano, sc.Now)
	if err != nil {
		now = time.Now()
	}
	speed := strconv.FormatFloat(sc.Speed, 'f', -1, 64)
	return template.HTML(`<span class="sim-badge" title="Simulated time, ` +
		template.HTMLEscapeString(speed) +
		`x wall. Ages and durations on this page are in simulated time.">SIM ` +
		template.HTMLEscapeString(speed) + `&times; &middot; ` +
		template.HTMLEscapeString(Clock(now, loc)) + `</span>`)
}
