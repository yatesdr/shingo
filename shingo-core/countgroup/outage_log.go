package countgroup

import (
	"sync"
	"time"
)

// outageLogger collapses repeat-fire log lines from per-tick failure
// loops into three messages per outage:
//
//   - one OPEN line on the first failure after a healthy period
//   - one SUMMARY line every summaryInterval while the outage continues
//   - one CLOSE line on the next success
//
// Use one outageLogger per call site, keyed by a short source string
// (the count group name here). Safe for concurrent use.
//
// Motivation: the poll loop fires every PollInterval (default 500ms)
// per group. An RDS outage at 4 groups produced ~8 log lines/sec; with
// the summary cadence at 60s, the same outage produces ~1 line/min
// plus an open and close. Combined with the per-group exponential
// backoff in groupLoop, the load on the recovering RDS also drops.
type outageLogger struct {
	mu              sync.Mutex
	tracked         map[string]*outageState
	summaryInterval time.Duration
}

type outageState struct {
	failingSince time.Time
	lastSummary  time.Time
	attempts     int
}

func newOutageLogger(summaryInterval time.Duration) *outageLogger {
	if summaryInterval <= 0 {
		summaryInterval = 60 * time.Second
	}
	return &outageLogger{
		tracked:         make(map[string]*outageState),
		summaryInterval: summaryInterval,
	}
}

// Failure records a failure for source. what is the human-readable
// subject ("group SNF2-CW1 poll"). On the first failure after healthy,
// logs the open line. On sustained failure, logs a summary every
// summaryInterval. Every tick in between is suppressed.
func (l *outageLogger) Failure(source string, logFn func(string, ...any), what string, err error) {
	if logFn == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.tracked[source]
	if !ok {
		st = &outageState{}
		l.tracked[source] = st
	}
	if st.failingSince.IsZero() {
		st.failingSince = now
		st.lastSummary = now
		st.attempts = 1
		logFn("countgroup: %s failed: %v", what, err)
		return
	}
	st.attempts++
	if now.Sub(st.lastSummary) >= l.summaryInterval {
		elapsed := now.Sub(st.failingSince).Round(time.Second)
		logFn("countgroup: %s still failing for %s (%d attempts): %v", what, elapsed, st.attempts, err)
		st.lastSummary = now
	}
}

// Success records a successful operation. If an outage was active for
// this source, emits the close message; otherwise no-op so steady-state
// callers don't pay for it.
func (l *outageLogger) Success(source string, logFn func(string, ...any), what string) {
	if logFn == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.tracked[source]
	if !ok || st.failingSince.IsZero() {
		return
	}
	elapsed := now.Sub(st.failingSince).Round(time.Second)
	logFn("countgroup: %s recovered after %s (%d attempts)", what, elapsed, st.attempts)
	st.failingSince = time.Time{}
	st.attempts = 0
}
