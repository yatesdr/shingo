package engine

import (
	"time"

	"shingo/protocol/liveness"
)

// StartScanProbe marks the scan lock's turnover for the freeze alarm. Every 5 s
// a probe takes the lock with an empty body: a scan pass, an acquire and a door
// all hold it, so a probe that waits 30 s means nothing has been able to scan.
// Cost: one lock take per 5 s; no statements, no messages.
func (e *Engine) StartScanProbe(l *liveness.Loop) {
	if e.fulfillment == nil {
		return
	}
	go func() {
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
	}()
}
