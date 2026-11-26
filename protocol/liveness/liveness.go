// Package liveness tells a frozen loop from a quiet one, in the plant build.
//
// A loop marks the start and the end of each turn of work (Enter, Leave). A
// loop waiting for work is idle and never alarms: an inbound consumer blocked
// on an empty topic is quiet, not frozen. A loop that entered a turn and has
// not left it for the limit has stopped turning over: the watch logs one entry
// with every goroutine's stack and reports it until the loop leaves, then logs
// that it recovered. Alarm only; nothing is restarted.
//
// A failure no turn can show (a loop that takes no turn at all) is registered
// as a Condition, which judges itself and brings its own words.
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

	// A condition's (Watch.Condition); held is nil on a loop that marks turns.
	says string
	held func(now time.Time) time.Duration
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

// stall is the loop's alarm as of now, and whether it is raised: a turn past
// the limit, or a condition that holds.
func (l *Loop) stall(now time.Time) (Stall, bool) {
	if l.held != nil {
		d := l.held(now)
		return Stall{Loop: l.name, For: d, says: l.says}, d > 0
	}
	d := l.stuck(now)
	return Stall{Loop: l.name, For: d}, d >= Limit
}

// Stall is one frozen loop as the page shows it.
type Stall struct {
	Loop string        `json:"loop"`
	For  time.Duration `json:"for_ns"`

	says string // a condition's words, "" for a turn
}

// Sentence is the stall in the page's words.
func (s Stall) Sentence() string {
	d := s.For.Round(time.Second)
	if s.says != "" {
		return fmt.Sprintf(s.says, d)
	}
	return fmt.Sprintf("%s has not turned over for %s", s.Loop, d)
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

// Condition registers an alarm that is no loop's turn. held, asked at each read
// of the watch, returns how long the condition has held, 0 while it does not;
// it applies its own threshold. says is the alarm's sentence, with one %s for
// that duration; name is what the recovery line uses. Called at wiring, before
// Run; a no-op on a nil watch.
func (w *Watch) Condition(name, says string, held func(now time.Time) time.Duration) {
	if w != nil {
		w.loops = append(w.loops, &Loop{name: name, says: says, held: held})
	}
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
		s, raised := l.stall(now)
		switch {
		case raised && !l.alarmed.Load():
			l.alarmed.Store(true)
			w.logf("FROZEN: %s; every goroutine follows (alarm only, nothing is restarted)\n%s",
				s.Sentence(), w.dump())
		case !raised && l.alarmed.Load():
			l.alarmed.Store(false)
			w.logf("FROZEN cleared: %s turned over again", l.name)
		}
	}
}

// Frozen lists the loops past the limit and the conditions that hold, now.
func (w *Watch) Frozen() []Stall {
	if w == nil {
		return nil
	}
	now := time.Now()
	var out []Stall
	for _, l := range w.loops {
		if s, raised := l.stall(now); raised {
			out = append(out, s)
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
