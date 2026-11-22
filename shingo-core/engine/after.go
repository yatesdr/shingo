package engine

import (
	"log"
	"runtime/debug"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"sync/atomic"

	"shingo/protocol/liveness"
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
// The two count hooks hand their checks here too, for the other side of the
// same wait: they run on the one inbound reader, and every message of every
// station waited behind a refill's acquire (ThresholdMonitor.evaluateAfter).
//
// So such a subscriber hands its work here, and returns. One goroutine runs the
// work in the order it was handed, after the emitter has gone on; when the
// emitter held the scan lock, the work waits for it like any caller. Handing
// over never blocks and takes only this queue's own leaf lock, so it is safe
// under any lock. Zero statements: the same work runs, on another goroutine.
//
// The freeze alarm watches the drain: each piece of work is one turn, so a
// piece that waits 30 s (for the scan lock, say) is reported. depth is the
// work handed over and not yet finished, on the Core Health page. Atomic
// stores only.
type afterQueue struct {
	mu    lockcheck.Mutex[lockorder.AfterQueue]
	work  []func()
	wake  chan struct{}
	logFn func(string, ...any)

	started atomic.Bool
	ended   chan struct{} // closed when run returns

	turnover atomic.Pointer[liveness.Loop]
	depth    atomic.Int64
}

func newAfterQueue(logFn func(string, ...any)) *afterQueue {
	return &afterQueue{wake: make(chan struct{}, 1), ended: make(chan struct{}), logFn: logFn}
}

// join waits, after stop has closed, for the work in hand: run starts no more
// (TestAfterQueue_StopWaitsForTheWorkInHand). A queue never run returns at once.
func (q *afterQueue) join() {
	if q.started.Load() {
		<-q.ended
	}
}

// later hands fn to the queue's goroutine.
func (q *afterQueue) later(fn func()) {
	q.depth.Add(1)
	q.mu.Lock()
	q.work = append(q.work, fn)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// run serves the queue until stop closes. The work in hand then finishes
// (Engine.Stop joins it) and work still queued is dropped; the engine is going
// down. What a restart does about each kind:
//   - a threshold recheck (BinUpdated, or a count's: its message's offset is
//     committed by then): the monitor's startup sweep evaluates every binding;
//   - a swap request's contradiction note: lost; it is a log line and a chip;
//   - a resumed parent's acquire: the boot pass and the sweep ask it, but its
//     station notice ("dispatched", resumeAcquire) is not sent again by
//     anything here.
func (q *afterQueue) run(stop <-chan struct{}) {
	q.started.Store(true)
	defer close(q.ended)
	for {
		select {
		case <-stop:
			return
		case <-q.wake:
		}
		for {
			// Read before each piece: a join that found the queue not yet
			// started returned at once, and stop had closed before it looked.
			select {
			case <-stop:
				return
			default:
			}
			q.mu.Lock()
			if len(q.work) == 0 {
				q.mu.Unlock()
				break
			}
			fn := q.work[0]
			q.work = q.work[1:]
			q.mu.Unlock()
			q.turnover.Load().Turn(func() { q.do(fn) })
			q.depth.Add(-1)
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

// WatchAfterQueue marks the after-queue's drain for the freeze alarm.
func (e *Engine) WatchAfterQueue(l *liveness.Loop) { e.after.turnover.Store(l) }

// AfterQueueDepth is the after-emit work handed over and not yet finished.
func (e *Engine) AfterQueueDepth() int64 { return e.after.depth.Load() }
