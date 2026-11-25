// Package gogroup tracks goroutines so a stop can wait for them: an engine's
// loops and hand-offs (shingo-core engine.Engine.loops and its window pulls),
// which its Stop waits on under one deadline of its own.
package gogroup

import (
	"sync"

	"shingo/protocol/lockcheck"
)

// Group tracks goroutines so a stop can wait for them. The zero value is
// ready. R ranks its lock in the owner's lock order; the lock is held only
// across the Add and the go statement, so it is a leaf.
type Group[R lockcheck.Ranked] struct {
	mu      lockcheck.Mutex[R]
	stopped bool
	wg      sync.WaitGroup
}

// Spawn runs fn on a tracked goroutine, or not at all once stopped. The Add is
// under mu, the lock Stop sets stopped under, so it is ordered before the Wait.
func (g *Group[R]) Spawn(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		fn()
	}()
}

// Run runs fn on the calling goroutine, tracked as a spawned one is: a Stop
// begun while fn runs waits for it, a panic included. Once stopped it runs
// nothing and returns false, and what the work does then is the caller's.
func (g *Group[R]) Run(fn func()) bool {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return false
	}
	g.wg.Add(1)
	g.mu.Unlock()
	defer g.wg.Done()
	fn()
	return true
}

// Stop refuses new goroutines and waits for the running ones.
func (g *Group[R]) Stop() {
	g.mu.Lock()
	g.stopped = true
	g.mu.Unlock()
	g.wg.Wait()
}
