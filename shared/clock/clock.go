// Package clock provides an injectable time source (brief D1) so sim code runs
// on a wall clock in dev and a deterministic manual clock in tests. All new sim
// timing (robot driver, fake-WarLink tickers, sim-operator delays) takes a Clock
// and must never call time.Now / time.After / time.NewTicker directly.
package clock

import (
	"sync"
	"time"
)

// Clock is the injectable time source.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
}

// Ticker is the subset of *time.Ticker the sim depends on.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

var (
	_ Clock = realClock{}
	_ Clock = (*Manual)(nil)
)

// Real returns a Clock backed by the standard library — the dev/production impl.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (realClock) NewTicker(d time.Duration) Ticker       { return realTicker{time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time { return r.t.C }
func (r realTicker) Stop()               { r.t.Stop() }

// --- Global default clock (G12 — injectable now-provider) ---
//
// The global default is used by clock.Now(), which replaces time.Now() in
// product code paths that need sim-governed timestamps (orders, bins,
// envelopes, www handler cutoffs, heartbeat partitioning).  In production
// it always returns time.Now().  The sim startup calls SetDefault once
// with its own Clock so all downstream writes use simulated time.
//
// Code that already holds a Clock instance (sim driver, fake WarLink) should
// continue calling that directly.  clock.Now() is for the "middle tier"
// (store layers, protocol envelope, www handlers) that don't have a Clock
// wired through their constructors.

var (
	defaultMu sync.RWMutex
	defaultCl Clock = realClock{}
)

// Now returns the current time from the default clock.  In production this is
// time.Now().  In sim it returns the injected simulated time.
func Now() time.Time {
	defaultMu.RLock()
	c := defaultCl
	defaultMu.RUnlock()
	return c.Now()
}

// SetDefault replaces the global default clock.  Call once at sim startup.
// Not safe to call concurrently with itself; safe to call concurrently with Now().
func SetDefault(c Clock) {
	defaultMu.Lock()
	defaultCl = c
	defaultMu.Unlock()
}

// AsSimClock returns the process default clock as a *SimClock when sim mode
// installed one, else nil. It lets the dev speed endpoint reach SetSpeed/Speed
// without threading a clock handle through every constructor.
func AsSimClock() *SimClock {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	sc, _ := defaultCl.(*SimClock)
	return sc
}

// ScaleTTL converts a wall-time message-expiry budget into the clock domain the
// envelope is stamped in. Envelope `exp` is stamped from clock.Now(); under a
// fast-forward SimClock that is sim time, so a fixed sim-TTL T shrinks the REAL
// pipeline budget to T/speed — at 15× a 10-minute TTL is only ~40 wall-seconds.
// A normal pipeline hiccup (a lockstep restart's catch-up, a GC pause, a burst
// of orders) then pushes order-lifecycle messages (order.update / order.staged /
// order.ack) past expiry, the consumer drops them, and the order strands —
// never released, eventually abandoned, starving the cell. Scaling the stamped
// TTL by the effective speed keeps the real budget at T regardless of how fast
// the sim runs.
//
// Production-safe and dormant: with no SimClock installed (AsSimClock()==nil) or
// speed<=1 it returns d unchanged, so non-sim builds and 1× runs are untouched.
func ScaleTTL(d time.Duration) time.Duration {
	sc := AsSimClock()
	if sc == nil {
		return d
	}
	speed := sc.Speed()
	if speed <= 1 {
		return d
	}
	return time.Duration(float64(d) * speed)
}
