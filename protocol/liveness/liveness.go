// Package liveness tells a frozen loop from a quiet one, in the plant build.
//
// A loop marks the start and the end of each turn of work (Enter, Leave). A
// loop waiting for work is idle and never alarms: an inbound consumer blocked
// on an empty topic is quiet, not frozen. A loop that entered a turn and has
// not left it for the limit has stopped turning over: the watch logs one entry
// with every goroutine's stack and reports it until the loop leaves, then logs
// that it recovered. Alarm only; nothing is restarted.
//
// Wall time throughout: the sim's clock runs ahead, and a freeze is a real-time
// fact. Each mark is one atomic store; the watch reads them every 5 s.
package liveness

import (
	"fmt"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"time"
)

// Limit is how long a loop may stay inside one turn before it is frozen.
const Limit = 30 * time.Second

// checkEvery is the watch's read interval.
const checkEvery = 5 * time.Second

// Loop is one loop's turnover. The zero value is idle; a nil Loop is a no-op,
// so a component built without a watch marks nothing.
type Loop struct {
	name    string
	since   atomic.Int64 // wall unix nanos the current turn entered; 0 idle
	alarmed atomic.Bool
}

// Enter marks the start of a turn.
func (l *Loop) Enter() {
	if l != nil {
		l.since.Store(time.Now().UnixNano())
	}
}

// Leave marks the end of a turn.
func (l *Loop) Leave() {
	if l != nil {
		l.since.Store(0)
	}
}

// Turn runs fn as one turn of the loop.
func (l *Loop) Turn(fn func()) {
	l.Enter()
	defer l.Leave()
	fn()
}

// stuck is how long the loop has been inside its current turn; 0 when idle.
func (l *Loop) stuck(now time.Time) time.Duration {
	s := l.since.Load()
	if s == 0 {
		return 0
	}
	return now.Sub(time.Unix(0, s))
}

// Stall is one frozen loop as the page shows it.
type Stall struct {
	Loop string        `json:"loop"`
	For  time.Duration `json:"for_ns"`
}

// Sentence is the stall in the page's words.
func (s Stall) Sentence() string {
	return fmt.Sprintf("%s has not turned over for %s", s.Loop, s.For.Round(time.Second))
}

// Watch holds a process's loops and alarms on a frozen one.
type Watch struct {
	loops []*Loop
	logf  func(format string, args ...any)
	dump  func() string
}

// New returns a watch that logs through logf.
func New(logf func(format string, args ...any)) *Watch {
	return &Watch{logf: logf, dump: goroutines}
}

// Loop registers a loop under the name the alarm uses. Called at wiring, before
// Run.
func (w *Watch) Loop(name string) *Loop {
	l := &Loop{name: name}
	w.loops = append(w.loops, l)
	return l
}

// Run reads the loops every 5 s until stop closes.
func (w *Watch) Run(stop <-chan struct{}) {
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			w.check(now)
		}
	}
}

// check alarms a loop the first time it is past the limit and logs its
// recovery once it leaves the turn.
func (w *Watch) check(now time.Time) {
	for _, l := range w.loops {
		d := l.stuck(now)
		switch {
		case d >= Limit && !l.alarmed.Load():
			l.alarmed.Store(true)
			w.logf("FROZEN: %s; every goroutine follows (alarm only, nothing is restarted)\n%s",
				Stall{Loop: l.name, For: d}.Sentence(), w.dump())
		case d < Limit && l.alarmed.Load():
			l.alarmed.Store(false)
			w.logf("FROZEN cleared: %s turned over again", l.name)
		}
	}
}

// Frozen lists the loops past the limit now.
func (w *Watch) Frozen() []Stall {
	if w == nil {
		return nil
	}
	now := time.Now()
	var out []Stall
	for _, l := range w.loops {
		if d := l.stuck(now); d >= Limit {
			out = append(out, Stall{Loop: l.name, For: d})
		}
	}
	return out
}

// Sentences is Frozen in the page's words.
func Sentences(stalls []Stall) []string {
	out := make([]string, len(stalls))
	for i, s := range stalls {
		out[i] = s.Sentence()
	}
	return out
}

func goroutines() string {
	var b strings.Builder
	if err := pprof.Lookup("goroutine").WriteTo(&b, 2); err != nil {
		return "goroutine dump failed: " + err.Error()
	}
	return b.String()
}
