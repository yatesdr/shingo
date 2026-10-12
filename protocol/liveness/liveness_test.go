package liveness

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func watchWithLog() (*Watch, *[]string) {
	var lines []string
	w := New(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	w.dump = func() string { return "goroutine 1 [running]:" }
	return w, &lines
}

func TestAQuietLoopNeverAlarms(t *testing.T) {
	w, lines := watchWithLog()
	w.Loop("the inbound consumer")
	w.check(time.Now().Add(time.Hour))
	if len(*lines) != 0 || len(w.Frozen()) != 0 {
		t.Fatalf("an idle loop alarmed: %v", *lines)
	}
}

func TestALoopInsideOneTurnPastTheLimitAlarmsOnceWithTheDump(t *testing.T) {
	w, lines := watchWithLog()
	l := w.Loop("the scan pass")
	l.since.Store(time.Now().Add(-Limit - time.Second).UnixNano())

	w.check(time.Now())
	w.check(time.Now())
	if len(*lines) != 1 {
		t.Fatalf("logged %d entries, want one: %v", len(*lines), *lines)
	}
	if got := (*lines)[0]; !strings.HasPrefix(got, "FROZEN: the scan pass has not turned over for 31s") || !strings.Contains(got, "goroutine 1 [running]:") {
		t.Fatalf("the entry: %q; want the loop, how long, and the goroutine dump", got)
	}
	if f := w.Frozen(); len(f) != 1 || f[0].Loop != "the scan pass" {
		t.Fatalf("Frozen() = %v; want the scan pass", f)
	}

	l.Leave()
	w.check(time.Now())
	if len(*lines) != 2 || (*lines)[1] != "FROZEN cleared: the scan pass turned over again" {
		t.Fatalf("after the loop left its turn: %v; want one recovery line", *lines)
	}
	if f := w.Frozen(); len(f) != 0 {
		t.Fatalf("Frozen() after recovery = %v", f)
	}
}

func TestATurnUnderTheLimitIsNotFrozen(t *testing.T) {
	w, lines := watchWithLog()
	l := w.Loop("the PLC poll")
	l.Turn(func() {
		w.check(time.Now())
	})
	if len(*lines) != 0 || len(w.Frozen()) != 0 {
		t.Fatalf("a short turn alarmed: %v", *lines)
	}
}

func TestANilLoopMarksNothing(t *testing.T) {
	var l *Loop
	l.Enter()
	l.Leave()
	ran := false
	l.Turn(func() { ran = true })
	if !ran {
		t.Fatal("a nil loop did not run its turn")
	}
	var w *Watch
	if w.Frozen() != nil {
		t.Fatal("a nil watch reported a stall")
	}
}
