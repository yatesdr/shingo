// Package planttime renders timestamps for plant-floor display in the
// plant's wall-clock timezone.
//
// It lives in shared/ because both sides need the same answer and they
// computed it in separate modules before this package existed. Core's
// formatTime rendered UTC and relied on client JS to re-render; Edge's
// did the same with its own copy. The two copies drifted on nil-handling
// ("-" vs ""), and under the plant-local convention a disagreement is not
// a cosmetic difference: a Core board and an Edge station showing the
// same event on different clocks is a defect that reaches the floor and
// gets acted on — an operator reconciling "when did this bin stage"
// against a supervisor reading the other surface reads two different
// histories. One implementation, imported by both, is the only way the
// two cannot disagree.
//
// THE CONVENTION (the reason this package exists): storage is UTC
// everywhere (Core Postgres TIMESTAMPTZ with sessions pinned to UTC;
// Edge SQLite datetime('now')), the wire stays RFC3339-UTC, and display
// is plant-local for every viewer. The server knows the plant zone, so
// the first paint is correct and final — there is no post-paint rewrite,
// which under the old UTC-then-convert scheme was a visible flicker on
// every page load and every htmx swap.
//
// WHAT DOES NOT BELONG HERE: date/hour bucketing for reports (Edge's
// HourlyTracker owns that, and its retention pass must keep rendering
// cutoffs in the same zone it derives count_date from), and anything on
// the wire. This package is display, and its input is a resolved
// time.Location.
package planttime

import (
	"html/template"
	"time"
)

// displayLayout is the single wall-clock format for fixed-instant
// timestamps on both surfaces. Zone abbreviation included so even the
// pre-JS HTML is labeled truthfully — a misconfigured zone is then
// diagnosable at a glance rather than silently wrong.
const displayLayout = "Jan 2, 2006 15:04 MST"

// Format renders one fixed instant for display, plant-local and labeled.
// Zero times render "-" — the one agreed nil/zero spelling; Edge's old
// empty string and changeover.html's hand-written "--" were the drift
// this replaces.
func Format(t time.Time, loc *time.Location) template.HTML {
	if t.IsZero() {
		return template.HTML("-")
	}
	if loc == nil {
		loc = time.UTC
	}
	return template.HTML(`<time data-utc="` + t.UTC().Format(time.RFC3339) + `">` +
		t.In(loc).Format(displayLayout) + `</time>`)
}

// FormatPtr is Format for nullable timestamps. Nil renders "-", matching
// Format's zero-time spelling rather than Edge's historical "".
func FormatPtr(t *time.Time, loc *time.Location) template.HTML {
	if t == nil {
		return template.HTML("-")
	}
	return Format(*t, loc)
}

// Clock renders a time-of-day-only string plant-local (e.g. "15:04") for
// wall-clock displays and log-style columns whose shape is deliberately
// not a full datetime. The JS twin is shared/utils.js formatClock — the
// two must agree, the same rule formatDuration follows.
func Clock(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("15:04")
}

// ClockSeconds is Clock with seconds, for log views where the ordering of
// near-simultaneous lines is the point.
func ClockSeconds(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("15:04:05.000")
}
