package engine

import (
	"log"
	"runtime/debug"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)

	"shingo/protocol/lockcheck"
	"shingocore/internal/lockorder"
)

// after.go — work a synchronous subscriber must not do on the emitter's
// goroutine.
//
// The bus runs subscribers on the goroutine that emitted, and many emits happen
// inside the scan lock: a door's OrderReceived, a pass's terminal events, the
// lane gate's chapter end. A subscriber that acquires an order (AcquireOne,
// RunOnce) or creates one that is then acquired (the threshold monitor's refill,
// through acquireCoreAsk) needs that same lock, and a sync.Mutex taken twice on
// one goroutine waits for itself: Core hears no Edge and starts no order until
// it is restarted (carry-edge2, RULINGS-carry-and-core-freeze §2).
//
// So such a subscriber hands its work here, and returns. One goroutine runs the
// work in the order it was handed, after the emitter has gone on; when the
// emitter held the scan lock, the work waits for it like any caller. Handing
// over never blocks and takes only this queue's own leaf lock, so it is safe
// under any lock. Zero statements: the same work runs, on another goroutine.
type afterQueue struct {
	mu    lockcheck.Mutex[lockorder.AfterQueue]
	work  []func()
	wake  chan struct{}
	logFn func(string, ...any)
}

func newAfterQueue(logFn func(string, ...any)) *afterQueue {
	return &afterQueue{wake: make(chan struct{}, 1), logFn: logFn}
}

// later hands fn to the queue's goroutine.
func (q *afterQueue) later(fn func()) {
	q.mu.Lock()
	q.work = append(q.work, fn)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// run serves the queue until stop closes. Work still queued at stop is dropped:
// the engine is going down, and every piece of it is a recheck a periodic pass
// or the next event repeats.
func (q *afterQueue) run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-q.wake:
		}
		for {
			q.mu.Lock()
			if len(q.work) == 0 {
				q.mu.Unlock()
				break
			}
			fn := q.work[0]
			q.work = q.work[1:]
			q.mu.Unlock()
			q.do(fn)
		}
	}
}

func (q *afterQueue) do(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			msg := "engine: after-emit work panicked: %v\n%s"
			if q.logFn != nil {
				q.logFn(msg, r, debug.Stack())
				return
			}
			log.Printf(msg, r, debug.Stack())
		}
	}()
	fn()
}
