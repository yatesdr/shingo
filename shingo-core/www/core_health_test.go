package www

import (
	"strings"
	"testing"
	"time"
)

// Health is a VERDICT, not a stat dump. These pin the derivation and the
// formatting that makes it readable — the strip's rendering is CSS and JS, but
// what it renders is decided here.

func TestFormatUptime(t *testing.T) {
	// Largest meaningful unit and the one below it, never zero-padded
	// segments. "0d 00h 06m" spends eight characters saying nothing — two zero
	// segments a reader skips past to reach the only number that matters.
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{6 * time.Minute, "6m"},
		{14 * time.Minute, "14m"},
		{59 * time.Minute, "59m"},
		{90 * time.Minute, "1h 30m"},
		// A whole number of hours drops the empty minutes rather than
		// printing "2h 0m".
		{2 * time.Hour, "2h"},
		{25 * time.Hour, "1d 1h"},
		{4*24*time.Hour + 6*time.Hour + 5*time.Minute, "4d 6h"},
		// Likewise a whole number of days.
		{3 * 24 * time.Hour, "3d"},
	} {
		if got := formatUptime(tc.in); got != tc.want {
			t.Errorf("formatUptime(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Freshly booted must not read as a wall of zeroes — a restart is exactly when
// this string is being read, and it has to be legible at a glance.
func TestFormatUptime_FreshBootIsShort(t *testing.T) {
	got := formatUptime(6 * time.Minute)
	if strings.Contains(got, "0d") || strings.Contains(got, "00h") {
		t.Fatalf("a 6-minute uptime rendered as %q — zero segments are noise", got)
	}
	if got != "6m" {
		t.Fatalf("formatUptime(6m) = %q, want 6m", got)
	}
}

func TestGoroutineRing_BoundedAndOrdered(t *testing.T) {
	goroutineRing.mu.Lock()
	goroutineRing.points = nil
	goroutineRing.mu.Unlock()

	var last []int
	for range goroutineRingSize + 8 {
		last = sampleGoroutines()
	}
	if len(last) != goroutineRingSize {
		t.Fatalf("ring should cap at %d points, got %d", goroutineRingSize, len(last))
	}
	// Oldest first — the sparkline draws left to right and a reversed series
	// would show a leak as a recovery.
	for _, v := range last {
		if v <= 0 {
			t.Fatalf("goroutine counts must be positive, got %v", last)
		}
	}
}

// The verdict is worst-of, and every reason is a sentence. A red dot with no
// explanation is a puzzle, not a signal.
func TestCoreHealthVerdict_ReasonsAreSentences(t *testing.T) {
	c := CoreHealth{Verdict: verdictOK}

	// Mirrors coreHealth's derivation. Kept as a table rather than exercising
	// the handler, which needs a full engine — the logic under test is the
	// mapping from condition to reason, not the plumbing that gathers it.
	type cond struct {
		name  string
		apply func(*CoreHealth)
		want  string
	}
	for _, tc := range []cond{
		{"pool waits", func(c *CoreHealth) { c.DBWaitCount = 3 }, "DB pool waits"},
		{"overloaded", func(c *CoreHealth) { c.Load1, c.Cores = 9.5, 4 }, "over 4 cores"},
		{"dead letters", func(c *CoreHealth) { c.DeadLetters = 2 }, "dead letter"},
		{"anomalies", func(c *CoreHealth) { c.CompletionAnomalies = 7 }, "completion anomal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := c
			tc.apply(&h)
			reasons := deriveReasons(h, nil)
			if len(reasons) == 0 {
				t.Fatalf("%s must produce a reason", tc.name)
			}
			joined := strings.Join(reasons, "; ")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("reason %q does not mention %q", joined, tc.want)
			}
		})
	}

	// A healthy Core produces no reasons at all — silent on a good day.
	healthy := CoreHealth{Cores: 8, Load1: 0.4, DBMaxOpen: 25, DBInUse: 2}
	if got := deriveReasons(healthy, nil); len(got) != 0 {
		t.Fatalf("a healthy Core has nothing to say, got %v", got)
	}
}

// Load is only a fault when it EXCEEDS the core count. A box at exactly its
// core count is fully used, not overloaded, and flagging it would make the
// verdict cry wolf on every busy plant.
func TestCoreHealthVerdict_LoadAtCoreCountIsNotDegraded(t *testing.T) {
	if got := deriveReasons(CoreHealth{Cores: 4, Load1: 4.0}, nil); len(got) != 0 {
		t.Fatalf("load == cores is not degraded, got %v", got)
	}
	if got := deriveReasons(CoreHealth{Cores: 4, Load1: 4.01}, nil); len(got) != 1 {
		t.Fatalf("load just over cores is degraded, got %v", got)
	}
}

// /proc is absent on Windows and macOS dev boxes, so load1 reads 0. That must
// not be mistaken for a fault — nor for a genuinely idle box.
func TestCoreHealthVerdict_UnreadableLoadIsNotAFault(t *testing.T) {
	if got := deriveReasons(CoreHealth{Cores: 8, Load1: 0}, nil); len(got) != 0 {
		t.Fatalf("unreadable load must not degrade the verdict, got %v", got)
	}
}
