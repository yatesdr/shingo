package clock

import (
	"sync"
	"time"
)

// Manual is a deterministic Clock for tests. Time only moves on Advance, which
// fires — synchronously, before returning — every pending After channel and
// ticker tick whose deadline falls within (now, now+d].
type Manual struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
	tickers []*manualTicker
}

type waiter struct {
	deadline time.Time
	ch       chan time.Time
}

// NewManual returns a Manual clock starting at start.
func NewManual(start time.Time) *Manual { return &Manual{now: start} }

func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

func (m *Manual) After(d time.Duration) <-chan time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- m.now
		return ch
	}
	m.waiters = append(m.waiters, &waiter{deadline: m.now.Add(d), ch: ch})
	return ch
}

func (m *Manual) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: NewTicker interval must be > 0")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &manualTicker{parent: m, ch: make(chan time.Time, 1), interval: d, next: m.now.Add(d)}
	m.tickers = append(m.tickers, t)
	return t
}

// Advance moves time forward by d, firing all pending After channels and ticker
// ticks with a deadline in (now, now+d]. Sends happen after the lock is released
// so a woken consumer can re-register timers without deadlocking. Ticker sends
// are non-blocking (coalesce/drop, matching time.Ticker).
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	target := m.now.Add(d)

	var keep, fireW []*waiter
	for _, w := range m.waiters {
		if !w.deadline.After(target) {
			fireW = append(fireW, w)
		} else {
			keep = append(keep, w)
		}
	}
	m.waiters = keep

	type tick struct {
		ch chan time.Time
		at time.Time
	}
	var fireT []tick
	for _, tk := range m.tickers {
		if tk.stopped {
			continue
		}
		for !tk.next.After(target) {
			fireT = append(fireT, tick{tk.ch, tk.next})
			tk.next = tk.next.Add(tk.interval)
		}
	}

	m.now = target
	m.mu.Unlock()

	for _, w := range fireW {
		w.ch <- w.deadline // buffered cap 1, single send — never blocks
	}
	for _, t := range fireT {
		select {
		case t.ch <- t.at:
		default:
		}
	}
}

type manualTicker struct {
	parent   *Manual
	ch       chan time.Time
	interval time.Duration
	next     time.Time
	stopped  bool
}

func (t *manualTicker) C() <-chan time.Time { return t.ch }

func (t *manualTicker) Stop() {
	t.parent.mu.Lock()
	t.stopped = true
	t.parent.mu.Unlock()
}
