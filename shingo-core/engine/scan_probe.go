package engine

import (
	"time"

	"shingo/protocol/liveness"
)

// WatchScanWorker puts the scanner's worker under the freeze alarm, in two
// sentences that are never true of one state: the worker has stopped (the lock
// probe below still turns over), and one pass is running long, with how long
// (fulfillment.Scanner.WorkerStopped, RunningLong). The 60 s safety timer asks
// for the pass; the watch's own read judges, in wall time, whether it started.
// Neither is raised before the boot scan has finished. Cost: atomic reads every
// 5 s; no statements, no goroutine.
func (e *Engine) WatchScanWorker(w *liveness.Watch) {
	if e.fulfillment == nil {
		return
	}
	afterBoot := func(held func(time.Time) time.Duration) func(time.Time) time.Duration {
		return func(now time.Time) time.Duration {
			select {
			case <-e.bootScanned:
				return held(now)
			default:
				return 0
			}
		}
	}
	w.Condition("the scan worker", "the scan worker has stopped: no pass has started for %s",
		afterBoot(e.fulfillment.WorkerStopped))
	w.Condition("the long scan pass", "a scan pass is running long: %s so far",
		afterBoot(e.fulfillment.RunningLong))
}

// StartScanProbe marks the scan lock's turnover for the freeze alarm. Every 5 s
// a probe takes the lock with an empty body: a scan pass, an acquire and a door
// all hold it, so a probe that waits 30 s means nothing has been able to scan.
// Cost: one lock take per 5 s; no statements, no messages.
func (e *Engine) StartScanProbe(l *liveness.Loop) {
	if e.fulfillment == nil {
		return
	}
	e.loops.spawn(func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-e.stopChan:
				return
			case <-t.C:
				l.Turn(func() { e.fulfillment.Door(func() {}) })
			}
		}
	})
}
