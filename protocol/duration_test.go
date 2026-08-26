package protocol

import (
	"testing"
	"time"
)

// TestFormatDuration_TheLadder pins the ladder byte for byte.
//
// THIS TABLE IS DUPLICATED VERBATIM in shared/utils.livedurations.test.js, and
// the duplication is the point: the fault line is rendered once by this function
// server-side and then once per second by the JS ladder in the browser. While
// the two disagreed, every faulted row on every board visibly rewrote itself a
// second after it painted — "4m 07s" became "4m 7s" — and again after each
// reconcile, because the reconcile restores the server's text.
//
// If you change a row here, change it there too, or the suites disagree and one
// of them is lying about what an operator sees.
func TestFormatDuration_TheLadder(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0 s"},
		{999 * time.Millisecond, "0 s"},
		{time.Second, "1 s"},
		{18 * time.Second, "18 s"},
		{59999 * time.Millisecond, "59 s"},
		{time.Minute, "1m 00s"},
		{247 * time.Second, "4m 07s"},
		{599 * time.Second, "9m 59s"},
		{600 * time.Second, "10m"},
		{1380 * time.Second, "23m"},
		{3599 * time.Second, "59m"},
		{time.Hour, "1h 00m"},
		{7500 * time.Second, "2h 05m"},
		{86399 * time.Second, "23h 59m"},
		{86400 * time.Second, "1d 00h"},
		{273600 * time.Second, "3d 04h"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.d); got != c.want {
			t.Errorf("FormatDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
	// Negative floors at zero rather than rendering "-3 s", which on the floor
	// reads as a bug in the page rather than as two clocks disagreeing.
	if got := FormatDuration(-3 * time.Second); got != "0 s" {
		t.Errorf("FormatDuration(-3s) = %q, want %q", got, "0 s")
	}
}
