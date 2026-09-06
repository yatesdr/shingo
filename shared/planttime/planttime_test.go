package planttime

import (
	"html/template"
	"testing"
	"time"
)

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

// TestFormatPlantLocalOutput pins the rendered string byte for byte. This
// is the drift guard that ships with the promotion: both binaries' UIs
// render through this one function, and the pinned output — including the
// zone label and the RFC3339 data-utc attribute — is the contract the
// render tests on both sides assert against.
func TestFormatPlantLocalOutput(t *testing.T) {
	loc := chicago(t)
	// 2026-09-05 14:23:01 UTC = 09:23:01 CDT, Sep 5.
	in := time.Date(2026, 9, 5, 14, 23, 1, 0, time.UTC)
	want := template.HTML(`<time data-utc="2026-09-05T14:23:01Z">Sep 5, 2026 09:23 CDT</time>`)
	if got := Format(in, loc); got != want {
		t.Errorf("Format = %q, want %q", got, want)
	}
}

func TestFormatZeroAndNil(t *testing.T) {
	loc := chicago(t)
	if got := Format(time.Time{}, loc); got != template.HTML("-") {
		t.Errorf("Format(zero) = %q, want \"-\"", got)
	}
	if got := FormatPtr(nil, loc); got != template.HTML("-") {
		t.Errorf("FormatPtr(nil) = %q, want \"-\"", got)
	}
	// A non-nil pointer to the zero time must render the same as a zero
	// value — FormatPtr delegates, it does not have its own spelling.
	zv := time.Time{}
	if got := FormatPtr(&zv, loc); got != Format(zv, loc) {
		t.Errorf("FormatPtr(&zero) = %q, want Format(zero) = %q", got, Format(zv, loc))
	}
}

func TestFormatNilLocationFallsBackUTC(t *testing.T) {
	in := time.Date(2026, 9, 5, 14, 23, 1, 0, time.UTC)
	want := template.HTML(`<time data-utc="2026-09-05T14:23:01Z">Sep 5, 2026 14:23 UTC</time>`)
	if got := Format(in, nil); got != want {
		t.Errorf("Format(nil loc) = %q, want %q", got, want)
	}
}

// TestClockVariants pins the time-of-day shapes. These twin the JS
// formatClock in shared/utils.js — change them in both or in neither.
func TestClockVariants(t *testing.T) {
	loc := chicago(t)
	in := time.Date(2026, 9, 5, 14, 23, 1, 500000000, time.UTC)
	if got := Clock(in, loc); got != "09:23" {
		t.Errorf("Clock = %q, want 09:23", got)
	}
	if got := ClockSeconds(in, loc); got != "09:23:01.500" {
		t.Errorf("ClockSeconds = %q, want 09:23:01.500", got)
	}
}

// TestDSTBoundary names the convention's known cost so it stays chosen
// rather than discovered: across the US fall-back transition the same
// wall-clock hour occurs twice and both render "01:xx CDT"/"01:xx CST"
// distinguished only by the abbreviation. The abbreviation in
// displayLayout is what keeps the pair readable rather than identical.
func TestDSTBoundary(t *testing.T) {
	loc := chicago(t)
	first := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)  // 01:30 CDT
	second := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC) // 01:30 CST
	f1, f2 := Format(first, loc), Format(second, loc)
	if f1 == f2 {
		t.Errorf("fall-back pair renders identically: %q vs %q", f1, f2)
	}
}
