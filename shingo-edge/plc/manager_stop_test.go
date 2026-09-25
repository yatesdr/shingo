package plc

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingEmitter is mockEmitter that counts the counter deltas it is handed.
type countingEmitter struct {
	mockEmitter
	mu     sync.Mutex
	deltas int
}

func (e *countingEmitter) EmitCounterDelta(rpID, processID, styleID, delta, newCount int64, anomaly string) {
	e.mu.Lock()
	e.deltas++
	e.mu.Unlock()
}

func (e *countingEmitter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deltas
}

// TestStop_NoTickAfterReturn pins what the Edge's shutdown order rests on
// (main.go stops the engine before the deferred accumulator stops flush). Once
// Manager.Stop returns, the poll records no further counter snapshot, and every
// snapshot it recorded before then has had its delta handed to the emitter,
// which the engine handles synchronously into the accumulator. So a flush
// after Stop carries every tick the Edge recorded.
func TestStop_NoTickAfterReturn(t *testing.T) {
	t.Parallel()
	em := &countingEmitter{}
	r := newTickRigEmitting(t, em)
	r.mgr.cfg.PollRate = 5 * time.Millisecond

	// The counter climbs on its own for the whole test, before and after
	// Stop, so a poll that outlived Stop would find a change to record.
	var v atomic.Int64
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			r.setCount(v.Add(1))
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() { close(done) })

	snapshots := func() int {
		var n int
		if err := r.db.QueryRow(`SELECT COUNT(*) FROM counter_snapshots WHERE delta > 0`).Scan(&n); err != nil {
			t.Fatalf("count snapshots: %v", err)
		}
		return n
	}

	r.mgr.StartPolling()
	deadline := time.Now().Add(2 * time.Second)
	for em.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r.mgr.Stop()

	atStop, emittedAtStop := snapshots(), em.count()
	if atStop < 3 {
		t.Fatalf("snapshots before Stop = %d, want at least 3: the poll never ran", atStop)
	}
	if emittedAtStop != atStop {
		t.Errorf("at Stop: %d snapshots recorded, %d deltas emitted; want equal (a tick was in flight)", atStop, emittedAtStop)
	}

	time.Sleep(20 * r.mgr.cfg.PollRate)
	if got := snapshots(); got != atStop {
		t.Errorf("snapshots after Stop = %d, want %d: the poll recorded a tick after Stop returned", got, atStop)
	}
	if got := em.count(); got != emittedAtStop {
		t.Errorf("deltas after Stop = %d, want %d: a tick was emitted after Stop returned", got, emittedAtStop)
	}
}
