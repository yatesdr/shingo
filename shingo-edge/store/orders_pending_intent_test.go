package store

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"shingo/protocol"
)

// Edge v4 (orders_pending_intent) and the order-row intent doors.

// Exactly-once: take-twice returns false the second time, a stale expected
// value never takes, and of N racers on one intent exactly one wins.
func TestPendingIntentTakeIsExactlyOnce(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "take.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	id, err := db.CreateOrder("take-uuid", protocol.OrderTypeMove, nil, false, 1, "D", "", "S", protocol.Height{}, true, "P", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	const intent = `{"kind":"pullback","node_id":7}`
	if err := db.SetOrderPendingIntent(id, intent); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, err := db.GetOrderPendingIntent(id); err != nil || got != intent {
		t.Fatalf("get = (%q, %v), want (%q, nil)", got, err, intent)
	}
	if ok, err := db.TakeOrderPendingIntent(id, `{"kind":"pullback","node_id":8}`); err != nil || ok {
		t.Fatalf("take with a stale expected value = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := db.TakeOrderPendingIntent(id, intent); err != nil || !ok {
		t.Fatalf("first take = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := db.TakeOrderPendingIntent(id, intent); err != nil || ok {
		t.Fatalf("second take = (%v, %v), want (false, nil) — an intent fires once", ok, err)
	}
	if got, _ := db.GetOrderPendingIntent(id); got != "" {
		t.Fatalf("after the take pending_intent = %q, want \"\"", got)
	}

	// Race.
	if err := db.SetOrderPendingIntent(id, intent); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	const racers = 8
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := db.TakeOrderPendingIntent(id, intent)
			if err != nil {
				t.Errorf("racing take: %v", err)
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if w := wins.Load(); w != 1 {
		t.Fatalf("%d of %d racing takers won one intent, want exactly 1", w, racers)
	}
}
