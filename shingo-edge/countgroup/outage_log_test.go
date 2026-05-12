package countgroup

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureLogger collects log lines for assertion. Safe for concurrent
// use — outageLogger calls logFn while holding its own mutex but a
// test may inspect from the main goroutine.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (c *captureLogger) logFn(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Minimal Sprintf without importing fmt for the test harness —
	// we don't care about exact formatting, only what substring fired.
	c.lines = append(c.lines, simpleFormat(format, args))
}

func (c *captureLogger) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

// simpleFormat replaces %s/%v/%d with args' string forms — good enough
// for substring assertions, avoids the fmt import.
func simpleFormat(format string, args []any) string {
	var b strings.Builder
	ai := 0
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) {
			i++
			if ai < len(args) {
				b.WriteString(stringify(args[ai]))
				ai++
			}
			continue
		}
		b.WriteByte(format[i])
	}
	return b.String()
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case error:
		return x.Error()
	default:
		// Numbers, durations — Sprint via type switch on what we use.
		// time.Duration's String is fine; int via %d falls here too.
		type stringer interface{ String() string }
		if s, ok := v.(stringer); ok {
			return s.String()
		}
		return "?"
	}
}

func TestOutageLogger_FirstFailureLogsOpen(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	l := newOutageLogger(60 * time.Second)
	l.Failure("plc1", cap.logFn, "test write", errors.New("boom"))
	got := cap.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "test write failed") {
		t.Errorf("open line = %q, want substring 'test write failed'", got[0])
	}
}

func TestOutageLogger_RepeatFailuresSuppressedUntilSummary(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	// Tiny interval so the summary actually fires inside the test.
	l := newOutageLogger(50 * time.Millisecond)
	for i := 0; i < 10; i++ {
		l.Failure("plc1", cap.logFn, "test write", errors.New("boom"))
	}
	got := cap.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 line after 10 rapid failures (open only), got %d: %v", len(got), got)
	}
	time.Sleep(60 * time.Millisecond)
	l.Failure("plc1", cap.logFn, "test write", errors.New("boom"))
	got = cap.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 lines after summary interval, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[1], "still failing") {
		t.Errorf("summary line = %q, want substring 'still failing'", got[1])
	}
}

func TestOutageLogger_SuccessAfterFailureLogsClose(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	l := newOutageLogger(60 * time.Second)
	l.Failure("plc1", cap.logFn, "test write", errors.New("boom"))
	l.Failure("plc1", cap.logFn, "test write", errors.New("boom"))
	l.Failure("plc1", cap.logFn, "test write", errors.New("boom"))
	l.Success("plc1", cap.logFn, "test write")
	got := cap.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 lines (open + close), got %d: %v", len(got), got)
	}
	if !strings.Contains(got[1], "recovered after") {
		t.Errorf("close line = %q, want substring 'recovered after'", got[1])
	}
}

func TestOutageLogger_SuccessWithoutFailureIsNoop(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	l := newOutageLogger(60 * time.Second)
	l.Success("plc1", cap.logFn, "test write")
	got := cap.snapshot()
	if len(got) != 0 {
		t.Errorf("expected 0 lines for success-without-prior-failure, got %d: %v", len(got), got)
	}
}

func TestOutageLogger_PerSourceIndependence(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	l := newOutageLogger(60 * time.Second)
	l.Failure("groupA", cap.logFn, "groupA poll", errors.New("a"))
	l.Failure("groupB", cap.logFn, "groupB poll", errors.New("b"))
	l.Failure("groupA", cap.logFn, "groupA poll", errors.New("a"))
	l.Failure("groupB", cap.logFn, "groupB poll", errors.New("b"))
	got := cap.snapshot()
	// Two opens (one per source), repeat failures suppressed.
	if len(got) != 2 {
		t.Fatalf("expected 2 open lines (one per source), got %d: %v", len(got), got)
	}
}

